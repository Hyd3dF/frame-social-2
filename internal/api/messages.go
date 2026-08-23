package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Hyd3dF/frame-social-2/internal/httpx"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type conversationView struct {
	ID          string       `json:"id"`
	LastMessage *messageView `json:"lastMessage"`
	OtherMember userView     `json:"otherMember"`
	UnreadCount int          `json:"unreadCount"`
	UpdatedAt   string       `json:"updatedAt"`
}

type messageView struct {
	Body         *string        `json:"body"`
	ClientID     string         `json:"clientId"`
	Conversation string         `json:"conversationId"`
	CreatedAt    string         `json:"createdAt"`
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	Reactions    []reactionView `json:"reactions"`
	ReplyTo      *replyView     `json:"replyTo"`
	Saved        bool           `json:"saved"`
	SenderID     string         `json:"senderId"`
	Status       string         `json:"status"`
}

type reactionView struct {
	Count int    `json:"count"`
	Emoji string `json:"emoji"`
	Mine  bool   `json:"mine"`
}

type replyView struct {
	Body     *string `json:"body"`
	ID       string  `json:"id"`
	SenderID string  `json:"senderId"`
}

func (s *Server) createDirectConversation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID string `json:"userId"`
	}
	if !httpx.Decode(w, r, &input) || !validRecord(input.UserID, "account") {
		if input.UserID != "" {
			httpx.Error(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		}
		return
	}
	actor := accountID(r)
	if actor == input.UserID {
		httpx.Error(w, http.StatusBadRequest, "self_conversation", "Kendinizle sohbet başlatamazsınız.")
		return
	}
	pair := security.PairKey(actor, input.UserID)
	var decision []struct {
		Exists     bool   `json:"exists"`
		IsFriend   bool   `json:"isFriend"`
		Permission string `json:"permission"`
		Blocked    bool   `json:"blocked"`
	}
	err := s.db.Query(r.Context(), `RETURN [{
exists: array::len(SELECT id FROM account WHERE id = type::record($target) AND status = 'active') > 0,
isFriend: array::len(SELECT id FROM friendship WHERE pair_key = $pair) > 0,
blocked: array::len(SELECT id FROM blocked_account WHERE pair_key = $pair) > 0,
permission: (SELECT VALUE message_permission FROM privacy_setting WHERE account = type::record($target) LIMIT 1)[0] ?? 'friends'
}];`, map[string]any{"target": input.UserID, "pair": pair}, &decision)
	if err != nil {
		s.databaseError(w, "authorize direct conversation", err)
		return
	}
	if len(decision) == 0 || !decision[0].Exists {
		httpx.Error(w, http.StatusNotFound, "user_not_found", "Kullanıcı bulunamadı.")
		return
	}
	if decision[0].Blocked || decision[0].Permission == "nobody" || (decision[0].Permission == "friends" && !decision[0].IsFriend) {
		httpx.Error(w, http.StatusForbidden, "messages_not_allowed", "Bu kullanıcı yalnızca izin verdiği kişilerden mesaj alıyor.")
		return
	}
	conversationID, err := s.ensureDirectConversation(r.Context(), actor, input.UserID, pair)
	if err != nil {
		s.databaseError(w, "create direct conversation", err)
		return
	}
	s.events.publish([]string{actor, input.UserID}, conversationID)
	httpx.JSON(w, http.StatusCreated, recordID{ID: conversationID})
}

func (s *Server) ensureDirectConversation(ctx context.Context, actor, target, pair string) (string, error) {
	var result []recordID
	err := s.db.Query(ctx, `
LET $existing = SELECT * FROM conversation WHERE direct_key = $pair LIMIT 1;
LET $actor_record = type::record($actor);
LET $target_record = type::record($target);
LET $conversation = IF array::len($existing) > 0 THEN $existing[0] ELSE (CREATE ONLY conversation CONTENT {
 kind: 'direct', direct_key: $pair, created_by: $actor_record
}) END;
IF array::len($existing) = 0 {
 RELATE $actor_record->conversation_member->$conversation CONTENT { role: 'member' };
 RELATE $target_record->conversation_member->$conversation CONTENT { role: 'member' };
};
RETURN [{ id: <string>$conversation.id }];`, map[string]any{"pair": pair, "actor": actor, "target": target}, &result)
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", errors.New("conversation was not returned")
	}
	return result[0].ID, nil
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	var conversations []conversationView
	err := s.db.Query(r.Context(), `
SELECT <string>out.id AS id, <string>out.updated_at AS updatedAt,
{
 id: <string>(SELECT VALUE in FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0].id,
 fullName: (SELECT VALUE in.full_name FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0],
 displayName: (SELECT VALUE in.display_name FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0],
 username: (SELECT VALUE in.username FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0],
 avatarUrl: (SELECT VALUE in.avatar.public_url FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0],
 isPrivate: false
} AS otherMember,
array::len(SELECT id FROM message_receipt WHERE recipient = type::record($account) AND status != 'read' AND message.conversation = $parent.out) AS unreadCount,
IF out.last_message IS NONE THEN NONE ELSE {
 id: <string>out.last_message.id, clientId: out.last_message.client_id,
 conversationId: <string>out.id, senderId: <string>out.last_message.sender,
 body: out.last_message.body, kind: out.last_message.kind,
 createdAt: <string>out.last_message.created_at, status: 'sent', saved: false,
 reactions: [], replyTo: NONE
} END AS lastMessage
FROM conversation_member WHERE in = type::record($account) AND left_at IS NONE
ORDER BY updatedAt DESC LIMIT 100;`, map[string]any{"account": accountID(r)}, &conversations)
	if err != nil {
		s.databaseError(w, "list conversations", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	conversation := "conversation:" + r.PathValue("id")
	if !validRecord(conversation, "conversation") {
		httpx.Error(w, http.StatusBadRequest, "invalid_conversation", "Sohbet geçersiz.")
		return
	}
	limit := 50
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 && value <= 100 {
		limit = value
	}
	cursor := r.URL.Query().Get("before")
	if cursor != "" {
		if _, err := time.Parse(time.RFC3339Nano, cursor); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_cursor", "Sayfalama bilgisi geçersiz.")
			return
		}
	} else {
		cursor = time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	}
	if !s.isMember(r, conversation) {
		httpx.Error(w, http.StatusForbidden, "not_a_member", "Bu sohbete erişiminiz yok.")
		return
	}
	var messages []messageView
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, client_id AS clientId,
<string>conversation AS conversationId, <string>sender AS senderId, body, kind,
<string>created_at AS createdAt,
array::len(SELECT id FROM saved_message WHERE in = type::record($account) AND out = $parent.id) > 0 AS saved,
(SELECT VALUE status FROM message_receipt WHERE message = $parent.id AND recipient != type::record($account) LIMIT 1)[0] ?? 'sent' AS status,
(SELECT emoji, 1 AS count, in = type::record($account) AS mine
 FROM message_reaction WHERE out = $parent.id) AS reactions,
(SELECT { id: <string>in.id, senderId: <string>in.sender, body: in.body } FROM message_reply WHERE out = $parent.id LIMIT 1)[0] AS replyTo
FROM message WHERE conversation = type::record($conversation) AND created_at < <datetime>$before
AND deleted_at IS NONE ORDER BY createdAt DESC LIMIT $limit;`, map[string]any{
		"account": accountID(r), "conversation": conversation, "before": cursor, "limit": limit,
	}, &messages)
	if err != nil {
		s.databaseError(w, "list messages", err)
		return
	}
	var receiptPrivacy []struct {
		Enabled bool `json:"enabled"`
	}
	err = s.db.Query(r.Context(), `SELECT read_receipts_enabled AS enabled FROM privacy_setting
WHERE account IN (SELECT VALUE in FROM conversation_member
WHERE out = type::record($conversation) AND in != type::record($account) AND left_at IS NONE)
LIMIT 1;`, map[string]any{"account": accountID(r), "conversation": conversation}, &receiptPrivacy)
	if err != nil {
		s.databaseError(w, "read receipt privacy", err)
		return
	}
	if len(receiptPrivacy) > 0 && !receiptPrivacy[0].Enabled {
		for index := range messages {
			if messages[index].SenderID == accountID(r) && messages[index].Status == "read" {
				messages[index].Status = "delivered"
			}
		}
	}
	var nextCursor *string
	if len(messages) == limit {
		value := messages[len(messages)-1].CreatedAt
		nextCursor = &value
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": messages, "nextCursor": nextCursor})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	conversation := "conversation:" + r.PathValue("id")
	if !validRecord(conversation, "conversation") || !s.isMember(r, conversation) {
		httpx.Error(w, http.StatusForbidden, "not_a_member", "Bu sohbete mesaj gönderemezsiniz.")
		return
	}
	var input struct {
		Body      string `json:"body"`
		ClientID  string `json:"clientId"`
		ReplyToID string `json:"replyToId"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	input.Body = cleanMessageBody(input.Body)
	if input.Body == "" || len([]rune(input.Body)) > 4000 || len(input.ClientID) < 8 || len(input.ClientID) > 100 {
		httpx.Error(w, http.StatusBadRequest, "invalid_message", "Mesaj boş veya çok uzun.")
		return
	}
	if input.ReplyToID != "" && !validRecord(input.ReplyToID, "message") {
		httpx.Error(w, http.StatusBadRequest, "invalid_reply", "Yanıtlanan mesaj geçersiz.")
		return
	}
	var result []messageView
	err := s.db.Query(r.Context(), `
LET $message = CREATE ONLY message CONTENT {
 conversation: type::record($conversation), sender: type::record($sender),
 client_id: $client_id, body: $body, kind: 'text'
};
IF $has_reply {
 LET $original = SELECT * FROM type::record($reply_to) WHERE conversation = type::record($conversation) LIMIT 1;
 IF array::len($original) > 0 {
  LET $original_record = $original[0];
  RELATE $original_record->message_reply->$message CONTENT { replied_by: type::record($sender) };
 };
};
LET $recipients = SELECT VALUE in FROM conversation_member
 WHERE out = type::record($conversation) AND in != type::record($sender) AND left_at IS NONE;
FOR $recipient IN $recipients {
 CREATE message_receipt CONTENT { message: $message.id, recipient: $recipient, status: 'sent' };
};
UPDATE type::record($conversation) SET last_message = $message.id, updated_at = time::now();
RETURN [{ id: <string>$message.id, clientId: $message.client_id,
conversationId: <string>$message.conversation, senderId: <string>$message.sender,
body: $message.body, kind: $message.kind, createdAt: <string>$message.created_at,
status: 'sent', saved: false, reactions: [], replyTo: NONE }];`, map[string]any{
		"conversation": conversation, "sender": accountID(r), "client_id": input.ClientID,
		"body": input.Body, "reply_to": input.ReplyToID, "has_reply": input.ReplyToID != "",
	}, &result)
	if err != nil || len(result) == 0 {
		s.databaseError(w, "send message", err)
		return
	}
	s.publishConversation(r.Context(), conversation)
	httpx.JSON(w, http.StatusCreated, result[0])
}

func cleanMessageBody(value string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(value))
	for _, character := range value {
		if character == '\uFFFC' || character == '\uFEFF' {
			continue
		}
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			continue
		}
		cleaned.WriteRune(character)
	}
	return strings.TrimSpace(cleaned.String())
}

func (s *Server) readConversation(w http.ResponseWriter, r *http.Request) {
	s.updateConversationReceipt(w, r, "read")
}

func (s *Server) deliverConversation(w http.ResponseWriter, r *http.Request) {
	s.updateConversationReceipt(w, r, "delivered")
}

func (s *Server) updateConversationReceipt(w http.ResponseWriter, r *http.Request, status string) {
	conversation := "conversation:" + r.PathValue("id")
	if !validRecord(conversation, "conversation") || !s.isMember(r, conversation) {
		httpx.Error(w, http.StatusForbidden, "not_a_member", "Bu sohbete erişiminiz yok.")
		return
	}
	var pending []recordID
	condition := "status != 'read'"
	if status == "delivered" {
		condition = "status = 'sent'"
	}
	err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM message_receipt
WHERE recipient = type::record($account) AND message.conversation = type::record($conversation) AND `+condition+` LIMIT 1;`, map[string]any{
		"account": accountID(r), "conversation": conversation,
	}, &pending)
	if err != nil {
		s.databaseError(w, "inspect conversation receipt", err)
		return
	}
	if len(pending) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	set := "status = 'delivered', delivered_at = time::now(), updated_at = time::now()"
	if status == "read" {
		set = "status = 'read', delivered_at = delivered_at ?? time::now(), read_at = time::now(), updated_at = time::now()"
	}
	err = s.db.Query(r.Context(), `UPDATE message_receipt SET `+set+`
WHERE recipient = type::record($account) AND message.conversation = type::record($conversation) AND `+condition+`;`, map[string]any{
		"account": accountID(r), "conversation": conversation,
	}, nil)
	if err != nil {
		s.databaseError(w, "update conversation receipt", err)
		return
	}
	s.publishConversation(r.Context(), conversation)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putReaction(w http.ResponseWriter, r *http.Request) {
	message := "message:" + r.PathValue("id")
	var input struct {
		Emoji string `json:"emoji"`
	}
	if !httpx.Decode(w, r, &input) || !validRecord(message, "message") || len([]rune(input.Emoji)) < 1 || len([]rune(input.Emoji)) > 8 {
		return
	}
	if !s.canAccessMessage(r, message) {
		httpx.Error(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	err := s.db.Query(r.Context(), `IF array::len(SELECT id FROM message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji) = 0 {
LET $account_record = type::record($account); LET $message_record = type::record($message);
RELATE $account_record->message_reaction->$message_record CONTENT { emoji: $emoji };
};`, map[string]any{"account": accountID(r), "message": message, "emoji": input.Emoji}, nil)
	if err != nil {
		s.databaseError(w, "add reaction", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteReaction(w http.ResponseWriter, r *http.Request) {
	message := "message:" + r.PathValue("id")
	emoji, _ := url.PathUnescape(r.PathValue("emoji"))
	if !validRecord(message, "message") {
		httpx.Error(w, http.StatusBadRequest, "invalid_message", "Mesaj geçersiz.")
		return
	}
	err := s.db.Query(r.Context(), `DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji;`, map[string]any{"account": accountID(r), "message": message, "emoji": emoji}, nil)
	if err != nil {
		s.databaseError(w, "delete reaction", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveMessage(w http.ResponseWriter, r *http.Request) {
	message := "message:" + r.PathValue("id")
	if !validRecord(message, "message") || !s.canAccessMessage(r, message) {
		httpx.Error(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	err := s.db.Query(r.Context(), `IF array::len(SELECT id FROM saved_message WHERE in = type::record($account) AND out = type::record($message)) = 0 {
LET $account_record = type::record($account); LET $message_record = type::record($message);
RELATE $account_record->saved_message->$message_record;
};`, map[string]any{"account": accountID(r), "message": message}, nil)
	if err != nil {
		s.databaseError(w, "save message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unsaveMessage(w http.ResponseWriter, r *http.Request) {
	message := "message:" + r.PathValue("id")
	err := s.db.Query(r.Context(), `DELETE saved_message WHERE in = type::record($account) AND out = type::record($message);`, map[string]any{"account": accountID(r), "message": message}, nil)
	if err != nil {
		s.databaseError(w, "unsave message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateReceipt(w http.ResponseWriter, r *http.Request) {
	message := "message:" + r.PathValue("id")
	var input struct {
		Status string `json:"status"`
	}
	if !httpx.Decode(w, r, &input) || (input.Status != "delivered" && input.Status != "read") {
		return
	}
	set := "status = 'delivered', delivered_at = time::now(), updated_at = time::now()"
	if input.Status == "read" {
		set = "status = 'read', delivered_at = delivered_at ?? time::now(), read_at = time::now(), updated_at = time::now()"
	}
	err := s.db.Query(r.Context(), `UPDATE message_receipt SET `+set+` WHERE message = type::record($message) AND recipient = type::record($account);`, map[string]any{"message": message, "account": accountID(r)}, nil)
	if err != nil {
		s.databaseError(w, "update message receipt", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) isMember(r *http.Request, conversation string) bool {
	var result []recordID
	err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation) AND left_at IS NONE LIMIT 1;`, map[string]any{"account": accountID(r), "conversation": conversation}, &result)
	return err == nil && len(result) > 0
}

func (s *Server) canAccessMessage(r *http.Request, message string) bool {
	var result []recordID
	err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM message WHERE id = type::record($message)
AND array::len(SELECT id FROM conversation_member WHERE in = type::record($account) AND out = $parent.conversation AND left_at IS NONE) > 0 LIMIT 1;`, map[string]any{"account": accountID(r), "message": message}, &result)
	return err == nil && len(result) > 0
}
