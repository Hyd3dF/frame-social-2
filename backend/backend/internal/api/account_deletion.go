package api

import (
	"net/http"
)

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	account := accountID(r)
	if !validRecord(account, "account") {
		respondError(w, http.StatusUnauthorized, "unauthorized", "Oturum açmanız gerekiyor.")
		return
	}
	query := `BEGIN TRANSACTION;
LET $acct = type::record($account);
UPDATE $acct SET status = 'deleted', deleted_at = time::now(), phone_e164 = NONE, country_code = NONE, phone_verified_at = NONE, avatar = NONE, display_name = 'Silinmiş Hesap', full_name = 'Silinmiş Hesap', username = 'deleted_' + <string>rand::uuid(), updated_at = time::now();
UPDATE auth_session SET revoked_at = time::now(), expires_at = time::now() WHERE account = $acct;
DELETE FROM push_device WHERE account = $acct;
DELETE friendship WHERE in = $acct OR out = $acct;
UPDATE friend_request SET status = 'cancelled', responded_at = time::now() WHERE (sender = $acct OR recipient = $acct) AND status = 'pending';
DELETE FROM blocked_account WHERE in = $acct OR out = $acct;
COMMIT TRANSACTION;`
	if err := s.db.Query(r.Context(), query, map[string]any{"account": account}, nil); err != nil {
		s.databaseError(w, "delete account", err)
		return
	}
	s.members.Clear()
	w.WriteHeader(http.StatusNoContent)
}
