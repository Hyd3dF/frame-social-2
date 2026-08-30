package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	if !decode(w, r, &input) || !validRecord(input.UserID, "account") {
		if input.UserID != "" {
			respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		}
		return
	}
	actor := accountID(r)
	if actor == input.UserID {
		respondError(w, http.StatusBadRequest, "self_conversation", "Kendinizle sohbet başlatamazsınız.")
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
		respondError(w, http.StatusNotFound, "user_not_found", "Kullanıcı bulunamadı.")
		return
	}
	if decision[0].Blocked || decision[0].Permission == "nobody" || (decision[0].Permission == "friends" && !decision[0].IsFriend) {
		respondError(w, http.StatusForbidden, "messages_not_allowed", "Bu kullanıcı yalnızca izin verdiği kişilerden mesaj alıyor.")
		return
	}
	conversationID, err := s.ensureDirectConversation(r.Context(), actor, input.UserID, pair)
	if err != nil {
		s.databaseError(w, "create direct conversation", err)
		return
	}
	s.members.Set(conversationID, []string{actor, input.UserID})
	s.events.publish([]string{actor, input.UserID}, conversationID)
	respondJSON(w, http.StatusCreated, recordID{ID: conversationID})
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
(SELECT VALUE { id: <string>in.id, fullName: in.full_name, displayName: in.display_name, username: in.username, avatarUrl: in.avatar.public_url, isPrivate: false } FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0] AS otherMember,
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
	respondJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	conversation := normalizeRecordID(r.PathValue("id"), "conversation")
	if !validRecord(conversation, "conversation") {
		respondError(w, http.StatusBadRequest, "invalid_conversation", "Sohbet geçersiz.")
		return
	}
	limit := 50
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 && value <= 100 {
		limit = value
	}
	cursor := r.URL.Query().Get("before")
	if cursor != "" {
		if _, err := time.Parse(time.RFC3339Nano, cursor); err != nil {
			respondError(w, http.StatusBadRequest, "invalid_cursor", "Sayfalama bilgisi geçersiz.")
			return
		}
	} else {
		cursor = time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	}
	members, err := s.getMembersCached(r.Context(), conversation)
	if err != nil {
		s.databaseError(w, "list messages members", err)
		return
	}
	isMem := false
	for _, m := range members {
		if m == accountID(r) {
			isMem = true
			break
		}
	}
	if !isMem {
		respondError(w, http.StatusForbidden, "not_a_member", "Bu sohbete erişiminiz yok.")
		return
	}
	var messages []messageView
	err = s.db.Query(r.Context(), `SELECT <string>id AS id, client_id AS clientId,
<string>conversation AS conversationId, <string>sender AS senderId, body, kind,
<string>created_at AS createdAt,
array::len(SELECT id FROM saved_message WHERE in = type::record($account) AND out = $parent.id) > 0 AS saved,
(SELECT VALUE status FROM message_receipt WHERE message = $parent.id AND recipient != type::record($account) LIMIT 1)[0] ?? 'sent' AS status,
(SELECT emoji, 1 AS count, in = type::record($account) AS mine
 FROM message_reaction WHERE out = $parent.id) AS reactions,
(SELECT VALUE { id: <string>in.id, senderId: <string>in.sender, body: in.body } FROM message_reply WHERE out = $parent.id LIMIT 1)[0] AS replyTo
FROM message WHERE conversation = type::record($conversation) AND created_at < <datetime>$before
AND deleted_at IS NONE ORDER BY createdAt DESC LIMIT $limit;`, map[string]any{
		"account": accountID(r), "conversation": conversation, "before": cursor, "limit": limit,
	}, &messages)
	if err != nil {
		s.databaseError(w, "list messages", err)
		return
	}
	// Messages accepted into the bounded RAM queue are immediately visible to
	// both participants while the durable SurrealDB write completes.
	if pending := s.pending.List(conversation); len(pending) > 0 {
		beforeTime, _ := time.Parse(time.RFC3339Nano, cursor)
		for _, candidate := range pending {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, candidate.CreatedAt)
			if parseErr != nil || !createdAt.Before(beforeTime) {
				continue
			}
			duplicate := false
			for _, persisted := range messages {
				if persisted.ID == candidate.ID ||
					(candidate.ClientID != "" && persisted.ClientID == candidate.ClientID) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				messages = append(messages, candidate)
			}
		}
		sort.Slice(messages, func(i, j int) bool {
			return messages[i].CreatedAt > messages[j].CreatedAt
		})
		if len(messages) > limit {
			messages = messages[:limit]
		}
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
	if messages == nil {
		messages = []messageView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"messages": messages, "nextCursor": nextCursor})
}

func (s *Server) getMembersCached(ctx context.Context, conversation string) ([]string, error) {
	conversation = normalizeRecordID(conversation, "conversation")
	if members, ok := s.members.Get(conversation); ok {
		return members, nil
	}
	var ids []recordID
	if err := s.db.Query(ctx, `SELECT <string>in AS id FROM conversation_member WHERE out = type::record($conversation) AND left_at IS NONE;`, map[string]any{"conversation": conversation}, &ids); err != nil {
		return nil, err
	}
	members := make([]string, 0, len(ids))
	for _, v := range ids {
		members = append(members, v.ID)
	}
	s.members.Set(conversation, members)
	return members, nil
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	conversation := normalizeRecordID(r.PathValue("id"), "conversation")
	if !validRecord(conversation, "conversation") {
		respondError(w, http.StatusForbidden, "not_a_member", "Bu sohbete mesaj gönderemezsiniz.")
		return
	}
	var input struct {
		Body      string `json:"body"`
		ClientID  string `json:"clientId"`
		ReplyToID string `json:"replyToId"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Body = cleanMessageBody(input.Body)
	if input.Body == "" || len([]rune(input.Body)) > 4000 || len(input.ClientID) < 8 || len(input.ClientID) > 100 {
		respondError(w, http.StatusBadRequest, "invalid_message", "Mesaj boş veya çok uzun.")
		return
	}
	if input.ReplyToID != "" && !validRecord(input.ReplyToID, "message") {
		respondError(w, http.StatusBadRequest, "invalid_reply", "Yanıtlanan mesaj geçersiz.")
		return
	}
	acct := accountID(r)
	members, err := s.getMembersCached(r.Context(), conversation)
	if err != nil {
		s.databaseError(w, "send message members", err)
		return
	}
	isMember := false
	for _, m := range members {
		if m == acct {
			isMember = true
			break
		}
	}
	if !isMember {
		respondError(w, http.StatusForbidden, "not_a_member", "Bu sohbete mesaj gönderemezsiniz.")
		return
	}
	if s.limiter != nil {
		allowed, isDup, retryAfter, blockedUntil, err := s.limiter.Check(r.Context(), acct, input.ClientID)
		if err != nil {
			if s.log != nil {
				s.log.Error("message rate limit check failed", "account", acct, "error", err)
			}
			respondError(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
			return
		}
		if isDup {
			var existing []messageView
			_ = s.db.Query(r.Context(), `SELECT <string>id AS id, client_id AS clientId, <string>conversation AS conversationId, <string>sender AS senderId, body, kind, <string>created_at AS createdAt FROM message WHERE sender = type::record($account) AND client_id = $clientId LIMIT 1`, map[string]any{"account": acct, "clientId": input.ClientID}, &existing)
			if len(existing) > 0 {
				respondJSON(w, http.StatusOK, existing[0])
				return
			}
			for _, p := range s.pending.List(conversation) {
				if p.ClientID == input.ClientID {
					respondJSON(w, http.StatusOK, p)
					return
				}
			}
			respondJSON(w, http.StatusOK, map[string]any{"clientId": input.ClientID, "status": "sent"})
			return
		}
		if !allowed {
			if s.log != nil {
				s.log.Warn("message rate limited", "account", acct, "retryAfter", retryAfter, "blockedUntil", blockedUntil)
			}
			respondRateLimited(w, retryAfter, blockedUntil)
			return
		}
		if s.log != nil {
			s.log.Debug("message rate allowed", "account", acct)
		}
	}
	if len(members) > 1 {
		pairs := make([]string, 0, len(members)-1)
		for _, m := range members {
			if m != acct {
				pairs = append(pairs, security.PairKey(acct, m))
			}
		}
		if len(pairs) > 0 {
			var blocked []recordID
			if err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM blocked_account WHERE pair_key IN $pairs LIMIT 1;`, map[string]any{"pairs": pairs}, &blocked); err != nil {
				s.databaseError(w, "check blocked for message", err)
				return
			}
			if len(blocked) > 0 {
				respondError(w, http.StatusForbidden, "blocked", "Bu kullanıcı ile mesajlaşamazsınız.")
				return
			}
		}
	}
	job := newPersistJob(conversation, acct, input.Body, input.ClientID, input.ReplyToID)
	view := messageView{
		ID:           job.messageID,
		ClientID:     job.clientID,
		Conversation: job.conversation,
		SenderID:     job.sender,
		Body:         &job.body,
		Kind:         "text",
		CreatedAt:    job.createdAt.Format(time.RFC3339Nano),
		Status:       "sent",
		Reactions:    []reactionView{},
	}
	if input.ReplyToID != "" {
		view.ReplyTo = &replyView{ID: input.ReplyToID}
	}
	// Fast path: publish from RAM immediately, then write durably in the
	// bounded background queue. If the queue is saturated, reject instead of
	// acknowledging a message that could be lost.
	if s.persist == nil {
		respondError(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
		return
	}
	s.pending.Append(conversation, view)
	if !s.persist.enqueue(job) {
		s.pending.Remove(conversation, job.messageID)
		respondError(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
		return
	}
	s.events.publish(members, conversation)
	// Fire push notifications to recipients' devices (non-blocking, failures are logged and do not affect message delivery)
	s.triggerPushForMessage(acct, conversation, job.messageID, members)
	respondJSON(w, http.StatusCreated, view)
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
	conversation := normalizeRecordID(r.PathValue("id"), "conversation")
	if !validRecord(conversation, "conversation") || !s.isMember(r, conversation) {
		respondError(w, http.StatusForbidden, "not_a_member", "Bu sohbete erişiminiz yok.")
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
	rawID := r.PathValue("id")
	message := normalizeRecordID(rawID, "message")
	var input struct {
		Emoji string `json:"emoji"`
	}
	if !decode(w, r, &input) || !validRecord(message, "message") || len([]rune(input.Emoji)) < 1 || len([]rune(input.Emoji)) > 8 {
		return
	}
	conv, exists, isMember := s.authorizeMessageAccess(r, message)
	if !exists {
		if s.log != nil {
			rid := r.Context().Value(requestIDKey)
			s.log.Warn("reaction message not found", "request_id", rid, "account_id", accountID(r), "message_id", message, "resolved_conversation", conv)
		}
		respondError(w, http.StatusNotFound, "message_not_found", "Mesaj bulunamadı.")
		return
	}
	if !isMember {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	acct := normalizeRecordID(accountID(r), "account")
	// One-reaction-per-user: replace any existing reaction by this user for this message
	err := s.db.Query(r.Context(), `DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message);
LET $account_record = type::record($account); LET $message_record = type::record($message);
RELATE $account_record->message_reaction->$message_record CONTENT { emoji: $emoji };`, map[string]any{"account": acct, "message": message, "emoji": input.Emoji}, nil)
	if err != nil {
		s.databaseError(w, "add reaction", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteReaction(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	message := normalizeRecordID(rawID, "message")
	emoji, _ := url.PathUnescape(r.PathValue("emoji"))
	if !validRecord(message, "message") {
		respondError(w, http.StatusBadRequest, "invalid_message", "Mesaj geçersiz.")
		return
	}
	conv, exists, isMember := s.authorizeMessageAccess(r, message)
	if !exists {
		if s.log != nil {
			rid := r.Context().Value(requestIDKey)
			s.log.Warn("delete reaction message not found", "request_id", rid, "account_id", accountID(r), "message_id", message, "resolved_conversation", conv)
		}
		respondError(w, http.StatusNotFound, "message_not_found", "Mesaj bulunamadı.")
		return
	}
	if !isMember {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	acct := normalizeRecordID(accountID(r), "account")
	err := s.db.Query(r.Context(), `DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji;`, map[string]any{"account": acct, "message": message, "emoji": emoji}, nil)
	if err != nil {
		s.databaseError(w, "delete reaction", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveMessage(w http.ResponseWriter, r *http.Request) {
	message := normalizeRecordID(r.PathValue("id"), "message")
	if !validRecord(message, "message") || !s.canAccessMessage(r, message) {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	acct := normalizeRecordID(accountID(r), "account")
	err := s.db.Query(r.Context(), `IF array::len(SELECT id FROM saved_message WHERE in = type::record($account) AND out = type::record($message)) = 0 {
LET $account_record = type::record($account); LET $message_record = type::record($message);
RELATE $account_record->saved_message->$message_record;
};`, map[string]any{"account": acct, "message": message}, nil)
	if err != nil {
		s.databaseError(w, "save message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unsaveMessage(w http.ResponseWriter, r *http.Request) {
	message := normalizeRecordID(r.PathValue("id"), "message")
	if !validRecord(message, "message") || !s.canAccessMessage(r, message) {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	acct := normalizeRecordID(accountID(r), "account")
	err := s.db.Query(r.Context(), `DELETE saved_message WHERE in = type::record($account) AND out = type::record($message);`, map[string]any{"account": acct, "message": message}, nil)
	if err != nil {
		s.databaseError(w, "unsave message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateReceipt(w http.ResponseWriter, r *http.Request) {
	message := normalizeRecordID(r.PathValue("id"), "message")
	if !validRecord(message, "message") || !s.canAccessMessage(r, message) {
		respondError(w, http.StatusForbidden, "forbidden", "Mesaja erişiminiz yok.")
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &input) || (input.Status != "delivered" && input.Status != "read") {
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

func normalizeRecordID(raw, table string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if validRecord(raw, table) {
		return raw
	}
	// Handle already prefixed but with extra spaces or missing validation due to characters
	if strings.HasPrefix(raw, table+":") {
		return raw
	}
	// Raw id without prefix
	// Remove any leading table prefix duplication attempt
	raw = strings.TrimPrefix(raw, table+":")
	// Also handle case where raw contains "/" or is URL encoded – PathValue already decodes
	return table + ":" + raw
}

func (s *Server) isMember(r *http.Request, conversation string) bool {
	conversation = normalizeRecordID(conversation, "conversation")
	acct := normalizeRecordID(accountID(r), "account")
	var result []recordID
	err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation) AND left_at IS NONE LIMIT 1;`, map[string]any{"account": acct, "conversation": conversation}, &result)
	return err == nil && len(result) > 0
}

func (s *Server) isConversationMember(ctx context.Context, account, conversation string) (bool, error) {
	account = normalizeRecordID(account, "account")
	conversation = normalizeRecordID(conversation, "conversation")
	var result []recordID
	err := s.db.Query(ctx, `SELECT <string>id AS id FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation) AND left_at IS NONE LIMIT 1;`, map[string]any{"account": account, "conversation": conversation}, &result)
	if err != nil {
		return false, err
	}
	return len(result) > 0, nil
}

func (s *Server) resolveMessageConversation(ctx context.Context, messageID string) (string, bool, error) {
	normalized := normalizeRecordID(messageID, "message")
	if !validRecord(normalized, "message") {
		return "", false, nil
	}
	var result []struct {
		Conversation string `json:"conversation"`
	}
	// Use <string>conversation to normalize record to string like "conversation:xxx"
	err := s.db.Query(ctx, `SELECT <string>conversation AS conversation FROM type::record($message) LIMIT 1`, map[string]any{"message": normalized}, &result)
	if err != nil {
		return "", false, err
	}
	if len(result) == 0 || result[0].Conversation == "" {
		return "", false, nil
	}
	conv := normalizeRecordID(result[0].Conversation, "conversation")
	if !validRecord(conv, "conversation") {
		// If returned value is already a record string, try to use as is
		conv = result[0].Conversation
	}
	return conv, true, nil
}

func (s *Server) canAccessMessage(r *http.Request, message string) bool {
	conv, exists, err := s.resolveMessageConversation(r.Context(), message)
	if err != nil || !exists {
		return false
	}
	member, err := s.isConversationMember(r.Context(), accountID(r), conv)
	return err == nil && member
}

func (s *Server) authorizeMessageAccess(r *http.Request, rawMessageID string) (string, bool, bool) {
	// Returns conversationID, exists, isMember
	// Handles both raw and prefixed IDs
	normalized := normalizeRecordID(rawMessageID, "message")
	if !validRecord(normalized, "message") {
		return "", false, false
	}
	conv, exists, err := s.resolveMessageConversation(r.Context(), normalized)
	if err != nil {
		if s.log != nil {
			rid := r.Context().Value(requestIDKey)
			s.log.Error("resolve message conversation failed", "request_id", rid, "account_id", accountID(r), "message_id", normalized, "error", err)
		}
		return "", false, false
	}
	if !exists {
		return "", false, false
	}
	acct := accountID(r)
	isMem, err := s.isConversationMember(r.Context(), acct, conv)
	if err != nil {
		if s.log != nil {
			rid := r.Context().Value(requestIDKey)
			s.log.Error("membership check failed", "request_id", rid, "account_id", acct, "message_id", normalized, "conversation_id", conv, "error", err)
		}
		return conv, true, false
	}
	if !isMem && s.log != nil {
		rid := r.Context().Value(requestIDKey)
		s.log.Warn("reaction forbidden - not a member", "request_id", rid, "account_id", acct, "message_id", normalized, "conversation_id", conv, "predicate", "conversation_member.left_at IS NONE")
	}
	return conv, true, isMem
}
