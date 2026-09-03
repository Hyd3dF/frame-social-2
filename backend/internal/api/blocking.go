package api

import (
	"net/http"
	"strings"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

func (s *Server) blockUser(w http.ResponseWriter, r *http.Request) {
	target := normalizeRecordID(r.PathValue("id"), "account")
	actor := accountID(r)
	if !validRecord(target, "account") || target == actor {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	pair := security.PairKey(actor, target)
	query := `BEGIN TRANSACTION;
DELETE friendship WHERE pair_key = $pair;
UPDATE friend_request SET status = 'cancelled', responded_at = time::now() WHERE pair_key = $pair AND status = 'pending';
LET $actor_record = type::record($actor); LET $target_record = type::record($target);
RELATE $actor_record->blocked_account->$target_record CONTENT { pair_key: $pair, created_at: time::now() };
COMMIT TRANSACTION;`
	if err := s.db.Query(r.Context(), query, map[string]any{"actor": actor, "target": target, "pair": pair}, nil); err != nil {
		if isAlreadyExistsError(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.databaseError(w, "block user", err)
		return
	}
	s.members.Clear()
	w.WriteHeader(http.StatusNoContent)
}

func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already contains") || strings.Contains(message, "already exists") || strings.Contains(message, "record already exists")
}

func (s *Server) unblockUser(w http.ResponseWriter, r *http.Request) {
	target := normalizeRecordID(r.PathValue("id"), "account")
	actor := accountID(r)
	if !validRecord(target, "account") || !validRecord(actor, "account") || target == actor {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	pair := security.PairKey(actor, target)
	err := s.db.Query(r.Context(), `DELETE FROM blocked_account WHERE pair_key = $pair AND in = type::record($actor) AND out = type::record($target);`, map[string]any{"pair": pair, "actor": actor, "target": target}, nil)
	if err != nil {
		s.databaseError(w, "unblock user", err)
		return
	}
	s.members.Clear()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBlockedUsers(w http.ResponseWriter, r *http.Request) {
	type blockedUserView struct {
		AvatarURL   *string `json:"avatarUrl"`
		CreatedAt   string  `json:"-"`
		DisplayName string  `json:"displayName"`
		FullName    string  `json:"fullName"`
		ID          string  `json:"id"`
		Username    string  `json:"username"`
	}
	var users []blockedUserView
	err := s.db.Query(r.Context(), `SELECT <string>out AS id, <string>created_at AS createdAt, out.full_name AS fullName, out.display_name AS displayName, out.username AS username, out.avatar.public_url AS avatarUrl FROM blocked_account WHERE in = type::record($account) ORDER BY createdAt DESC LIMIT 100;`, map[string]any{"account": accountID(r)}, &users)
	if err != nil {
		s.databaseError(w, "list blocked users", err)
		return
	}
	if users == nil {
		users = []blockedUserView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"users": users})
}
