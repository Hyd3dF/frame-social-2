package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type conversationView struct {
	Description string       `json:"description,omitempty"`
	ID          string       `json:"id"`
	ImageURL    *string      `json:"imageUrl,omitempty"`
	JoinRule    string       `json:"joinRule,omitempty"`
	Kind        string       `json:"kind"`
	LastMessage *messageView `json:"lastMessage"`
	Name        string       `json:"name,omitempty"`
	OtherMember userView     `json:"otherMember"`
	Privacy     string       `json:"privacy,omitempty"`
	UnreadCount int          `json:"unreadCount"`
	UpdatedAt   string       `json:"updatedAt"`
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
		Blocked    bool   `json:"blocked"`
		Exists     bool   `json:"exists"`
		IsFriend   bool   `json:"isFriend"`
		Permission string `json:"permission"`
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
SELECT <string>out.id AS id,
	<string>out.kind AS kind,
	out.group_name AS name,
	out.group_description AS description,
	out.group_image_url AS imageUrl,
	out.group_privacy AS privacy,
	out.group_join_rule AS joinRule,
	<string>out.updated_at AS updatedAt,
(SELECT VALUE { id: <string>in.id, fullName: in.full_name, displayName: in.display_name, username: in.username, avatarUrl: in.avatar.public_url, isPrivate: false } FROM conversation_member WHERE out = $parent.out AND in != type::record($account) LIMIT 1)[0] AS otherMember,
array::len(SELECT id FROM message_receipt WHERE recipient = type::record($account) AND status != 'read' AND message.conversation = $parent.out) AS unreadCount,
IF out.last_message IS NONE OR out.last_message.deleted_mode = 'everyone'
OR array::len(SELECT id FROM message_hidden WHERE in = type::record($account) AND out = $parent.out.last_message) > 0 THEN NONE ELSE {
 id: <string>out.last_message.id, clientId: out.last_message.client_id,
 conversationId: <string>out.id, senderId: <string>out.last_message.sender,
 body: out.last_message.body, kind: out.last_message.kind,
 createdAt: <string>out.last_message.created_at, deleted: out.last_message.deleted_at IS NOT NONE,
 deletedAt: IF out.last_message.deleted_at IS NONE THEN NONE ELSE <string>out.last_message.deleted_at END, status: 'sent', saved: false,
 reactions: [], replyTo: NONE
} END AS lastMessage
FROM conversation_member WHERE in = type::record($account) AND left_at IS NONE
ORDER BY updatedAt DESC LIMIT 100;`, map[string]any{"account": accountID(r)}, &conversations)
	if err != nil {
		s.databaseError(w, "list conversations", err)
		return
	}
	if conversations == nil {
		conversations = []conversationView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (s *Server) conversationKind(ctx context.Context, conversation string) (string, error) {
	var rows []struct {
		Kind string `json:"kind"`
	}
	err := s.db.Query(ctx, `SELECT kind FROM type::record($conversation) LIMIT 1;`, map[string]any{"conversation": conversation}, &rows)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Kind, nil
}
