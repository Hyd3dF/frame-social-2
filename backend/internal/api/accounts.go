package api

import (
	"net/http"
	"strings"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	var result []accountAuth
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, phone_e164 AS phone,
country_code AS countryCode, full_name AS fullName, display_name AS displayName,
username, avatar.public_url AS avatarUrl FROM type::record($account) WHERE status = 'active';`, map[string]any{"account": accountID(r)}, &result)
	if err != nil {
		s.databaseError(w, "get current account", err)
		return
	}
	if len(result) == 0 {
		respondError(w, http.StatusNotFound, "account_not_found", "Hesap bulunamadı.")
		return
	}
	respondJSON(w, http.StatusOK, result[0])
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DisplayName *string `json:"displayName"`
		FullName    *string `json:"fullName"`
		Username    *string `json:"username"`
	}
	if !decode(w, r, &input) {
		return
	}
	set := map[string]any{}
	if input.DisplayName != nil {
		value := strings.TrimSpace(*input.DisplayName)
		if len([]rune(value)) < 2 || len([]rune(value)) > 50 {
			respondError(w, http.StatusBadRequest, "invalid_display_name", "Görünen ad geçersiz.")
			return
		}
		set["displayName"] = value
	}
	if input.FullName != nil {
		value := strings.TrimSpace(*input.FullName)
		if len([]rune(value)) < 2 || len([]rune(value)) > 80 {
			respondError(w, http.StatusBadRequest, "invalid_full_name", "Ad geçersiz.")
			return
		}
		set["fullName"] = value
	}
	if input.Username != nil {
		value := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(*input.Username, "@")))
		if !usernamePattern.MatchString(value) {
			respondError(w, http.StatusBadRequest, "invalid_username", "Kullanıcı adı geçersiz.")
			return
		}
		set["username"] = value
	}
	if len(set) == 0 {
		respondError(w, http.StatusBadRequest, "empty_update", "Değiştirilecek bir alan gönderin.")
		return
	}
	if username, ok := set["username"].(string); ok {
		var conflict []recordID
		err := s.db.Query(r.Context(), `SELECT <string>id AS id FROM account
WHERE username = $username AND id != type::record($account) LIMIT 1;`, map[string]any{
			"account": accountID(r), "username": username,
		}, &conflict)
		if err != nil {
			s.databaseError(w, "check username availability", err)
			return
		}
		if len(conflict) > 0 {
			respondError(w, http.StatusConflict, "username_taken", "Bu kullanıcı adı kullanılıyor.")
			return
		}
	}
	var result []accountAuth
	err := s.db.Query(r.Context(), `UPDATE type::record($account) SET
full_name = IF $hasFullName THEN $fullName ELSE full_name END,
display_name = IF $hasDisplayName THEN $displayName ELSE display_name END,
username = IF $hasUsername THEN $username ELSE username END,
updated_at = time::now();
SELECT <string>id AS id, phone_e164 AS phone, country_code AS countryCode,
full_name AS fullName, display_name AS displayName, username, avatar.public_url AS avatarUrl
FROM type::record($account);`, map[string]any{
		"account":  accountID(r),
		"fullName": stringValue(set, "fullName"), "hasFullName": set["fullName"] != nil,
		"displayName": stringValue(set, "displayName"), "hasDisplayName": set["displayName"] != nil,
		"username": stringValue(set, "username"), "hasUsername": set["username"] != nil,
	}, &result)
	if err != nil || len(result) == 0 {
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
			respondError(w, http.StatusConflict, "username_taken", "Bu kullanıcı adı kullanılıyor.")
			return
		}
		s.databaseError(w, "update account", err)
		return
	}
	respondJSON(w, http.StatusOK, result[0])
}
