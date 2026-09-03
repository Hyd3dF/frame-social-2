package api

import (
	"net/http"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type friendRequestView struct {
	CreatedAt string   `json:"createdAt"`
	ID        string   `json:"id"`
	Recipient userView `json:"recipient"`
	Sender    userView `json:"sender"`
	Status    string   `json:"status"`
}

func (s *Server) createFriendRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RecipientID string `json:"recipientId"`
	}
	if !decode(w, r, &input) || !validRecord(input.RecipientID, "account") {
		if input.RecipientID != "" {
			respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		}
		return
	}
	sender := accountID(r)
	if sender == input.RecipientID {
		respondError(w, http.StatusBadRequest, "self_request", "Kendinize arkadaşlık isteği gönderemezsiniz.")
		return
	}
	pair := security.PairKey(sender, input.RecipientID)
	var allowed []struct {
		Allowed bool `json:"allowed"`
	}
	err := s.db.Query(r.Context(), `RETURN [{ allowed:
array::len(SELECT id FROM account WHERE id = type::record($recipient) AND status = 'active') > 0
AND (SELECT VALUE friend_request_permission FROM privacy_setting WHERE account = type::record($recipient) LIMIT 1)[0] = 'everyone'
AND array::len(SELECT id FROM blocked_account WHERE pair_key = $pair) = 0
AND array::len(SELECT id FROM friendship WHERE pair_key = $pair) = 0
AND array::len(SELECT id FROM friend_request WHERE pair_key = $pair AND status = 'pending') = 0
}];`, map[string]any{"recipient": input.RecipientID, "pair": pair}, &allowed)
	if err != nil {
		s.databaseError(w, "validate friend request", err)
		return
	}
	if len(allowed) == 0 || !allowed[0].Allowed {
		respondError(w, http.StatusForbidden, "request_not_allowed", "Bu kullanıcıya arkadaşlık isteği gönderilemiyor.")
		return
	}
	var result []recordID
	err = s.db.Query(r.Context(), `LET $request = CREATE ONLY friend_request CONTENT {
	sender: type::record($sender), recipient: type::record($recipient), pair_key: $pair, status: 'pending'
}; RETURN [{ id: <string>$request.id }];`, map[string]any{"sender": sender, "recipient": input.RecipientID, "pair": pair}, &result)
	if err != nil || len(result) == 0 {
		s.databaseError(w, "create friend request", err)
		return
	}
	respondJSON(w, http.StatusCreated, result[0])
}

func (s *Server) listFriendRequests(w http.ResponseWriter, r *http.Request) {
	var requests []friendRequestView
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, status, <string>created_at AS createdAt,
{
 id: <string>sender.id, fullName: sender.full_name, displayName: sender.display_name,
 username: sender.username, avatarUrl: sender.avatar.public_url, isPrivate: false
} AS sender,
{
 id: <string>recipient.id, fullName: recipient.full_name, displayName: recipient.display_name,
 username: recipient.username, avatarUrl: recipient.avatar.public_url, isPrivate: false
} AS recipient
FROM friend_request WHERE (sender = type::record($account) OR recipient = type::record($account))
AND status = 'pending' ORDER BY createdAt DESC LIMIT 100;`, map[string]any{"account": accountID(r)}, &requests)
	if err != nil {
		s.databaseError(w, "list friend requests", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Server) respondFriendRequest(w http.ResponseWriter, r *http.Request) {
	id := "friend_request:" + r.PathValue("id")
	if !validRecord(id, "friend_request") {
		respondError(w, http.StatusBadRequest, "invalid_request", "Arkadaşlık isteği geçersiz.")
		return
	}
	var input struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &input) || (input.Action != "accept" && input.Action != "reject") {
		if input.Action != "" {
			respondError(w, http.StatusBadRequest, "invalid_action", "Yanıt geçersiz.")
		}
		return
	}
	status := "rejected"
	if input.Action == "accept" {
		status = "accepted"
	}
	var found []struct {
		Sender    string `json:"sender"`
		Recipient string `json:"recipient"`
		Pair      string `json:"pair"`
	}
	err := s.db.Query(r.Context(), `SELECT <string>sender AS sender, <string>recipient AS recipient, pair_key AS pair
FROM type::record($request) WHERE recipient = type::record($account) AND status = 'pending' LIMIT 1;`, map[string]any{"request": id, "account": accountID(r)}, &found)
	if err != nil || len(found) == 0 {
		respondError(w, http.StatusNotFound, "request_not_found", "Bekleyen arkadaşlık isteği bulunamadı.")
		return
	}
	query := `UPDATE type::record($request) SET status = $status, responded_at = time::now();`
	if status == "accepted" {
		query += ` LET $sender_record = type::record($sender); LET $recipient_record = type::record($recipient);
RELATE $sender_record->friendship->$recipient_record CONTENT { pair_key: $pair };`
	}
	if err := s.db.Query(r.Context(), query, map[string]any{"request": id, "status": status, "sender": found[0].Sender, "recipient": found[0].Recipient, "pair": found[0].Pair}, nil); err != nil {
		s.databaseError(w, "respond friend request", err)
		return
	}
	response := map[string]string{"status": status}
	if status == "accepted" {
		conversationID, err := s.ensureDirectConversation(r.Context(), found[0].Recipient, found[0].Sender, found[0].Pair)
		if err != nil {
			s.databaseError(w, "create accepted friend conversation", err)
			return
		}
		response["conversationId"] = conversationID
		s.members.Set(conversationID, []string{found[0].Sender, found[0].Recipient})
		s.events.publish([]string{found[0].Sender, found[0].Recipient}, conversationID)
	}
	respondJSON(w, http.StatusOK, response)
}

func (s *Server) unfriend(w http.ResponseWriter, r *http.Request) {
	target := normalizeRecordID(r.PathValue("id"), "account")
	actor := accountID(r)
	if !validRecord(target, "account") {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	if target == actor {
		respondError(w, http.StatusBadRequest, "self_request", "Kendinizle olan arkadaşlığınızı silemezsiniz.")
		return
	}
	pair := security.PairKey(actor, target)
	query := `BEGIN TRANSACTION;
DELETE friendship WHERE pair_key = $pair;
UPDATE friend_request SET status = 'cancelled', responded_at = time::now() WHERE pair_key = $pair AND status = 'pending';
COMMIT TRANSACTION;`
	if err := s.db.Query(r.Context(), query, map[string]any{"pair": pair}, nil); err != nil {
		s.databaseError(w, "unfriend", err)
		return
	}
	s.members.Clear()
	w.WriteHeader(http.StatusNoContent)
}
