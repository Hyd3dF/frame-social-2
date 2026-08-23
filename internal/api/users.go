package api

import (
	"net/http"
	"strings"

	"github.com/Hyd3dF/frame-social-2/internal/httpx"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type userView struct {
	AvatarURL    *string `json:"avatarUrl"`
	DisplayName  string  `json:"displayName"`
	FullName     string  `json:"fullName"`
	ID           string  `json:"id"`
	IsPrivate    bool    `json:"isPrivate"`
	Relationship string  `json:"relationship"`
	Username     string  `json:"username"`
}

type privacyView struct {
	AllowSearchByPhone      bool   `json:"allowSearchByPhone"`
	FriendRequestPermission string `json:"friendRequestPermission"`
	IsPrivate               bool   `json:"isPrivate"`
	MessagePermission       string `json:"messagePermission"`
	ReadReceiptsEnabled     bool   `json:"readReceiptsEnabled"`
	ShowLastSeen            bool   `json:"showLastSeen"`
	ShowPhone               bool   `json:"showPhone"`
}

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
		httpx.Error(w, http.StatusNotFound, "account_not_found", "Hesap bulunamadı.")
		return
	}
	httpx.JSON(w, http.StatusOK, result[0])
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DisplayName *string `json:"displayName"`
		FullName    *string `json:"fullName"`
		Username    *string `json:"username"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	set := map[string]any{}
	if input.DisplayName != nil {
		value := strings.TrimSpace(*input.DisplayName)
		if len([]rune(value)) < 2 || len([]rune(value)) > 50 {
			httpx.Error(w, http.StatusBadRequest, "invalid_display_name", "Görünen ad geçersiz.")
			return
		}
		set["displayName"] = value
	}
	if input.FullName != nil {
		value := strings.TrimSpace(*input.FullName)
		if len([]rune(value)) < 2 || len([]rune(value)) > 80 {
			httpx.Error(w, http.StatusBadRequest, "invalid_full_name", "Ad geçersiz.")
			return
		}
		set["fullName"] = value
	}
	if input.Username != nil {
		value := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(*input.Username, "@")))
		if !usernamePattern.MatchString(value) {
			httpx.Error(w, http.StatusBadRequest, "invalid_username", "Kullanıcı adı geçersiz.")
			return
		}
		set["username"] = value
	}
	if len(set) == 0 {
		httpx.Error(w, http.StatusBadRequest, "empty_update", "Değiştirilecek bir alan gönderin.")
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
			httpx.Error(w, http.StatusConflict, "username_taken", "Bu kullanıcı adı kullanılıyor.")
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
			httpx.Error(w, http.StatusConflict, "username_taken", "Bu kullanıcı adı kullanılıyor.")
			return
		}
		s.databaseError(w, "update account", err)
		return
	}
	httpx.JSON(w, http.StatusOK, result[0])
}

func (s *Server) getPrivacy(w http.ResponseWriter, r *http.Request) {
	var result []privacyView
	err := s.db.Query(r.Context(), `SELECT is_private AS isPrivate, message_permission AS messagePermission,
friend_request_permission AS friendRequestPermission, read_receipts_enabled AS readReceiptsEnabled,
show_last_seen AS showLastSeen, show_phone AS showPhone, allow_search_by_phone AS allowSearchByPhone
FROM privacy_setting WHERE account = type::record($account) LIMIT 1;`, map[string]any{"account": accountID(r)}, &result)
	if err != nil || len(result) == 0 {
		s.databaseError(w, "get privacy settings", err)
		return
	}
	httpx.JSON(w, http.StatusOK, result[0])
}

func (s *Server) updatePrivacy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AllowSearchByPhone      *bool   `json:"allowSearchByPhone"`
		FriendRequestPermission *string `json:"friendRequestPermission"`
		IsPrivate               *bool   `json:"isPrivate"`
		MessagePermission       *string `json:"messagePermission"`
		ReadReceiptsEnabled     *bool   `json:"readReceiptsEnabled"`
		ShowLastSeen            *bool   `json:"showLastSeen"`
		ShowPhone               *bool   `json:"showPhone"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.MessagePermission != nil && *input.MessagePermission != "everyone" && *input.MessagePermission != "friends" && *input.MessagePermission != "nobody" {
		httpx.Error(w, http.StatusBadRequest, "invalid_privacy", "Mesaj izni geçersiz.")
		return
	}
	if input.FriendRequestPermission != nil && *input.FriendRequestPermission != "everyone" && *input.FriendRequestPermission != "nobody" {
		httpx.Error(w, http.StatusBadRequest, "invalid_privacy", "Arkadaşlık isteği izni geçersiz.")
		return
	}
	var result []privacyView
	err := s.db.Query(r.Context(), `UPDATE privacy_setting SET
is_private = IF $hasIsPrivate THEN $isPrivate ELSE is_private END,
message_permission = IF $hasMessagePermission THEN $messagePermission ELSE message_permission END,
friend_request_permission = IF $hasFriendRequestPermission THEN $friendRequestPermission ELSE friend_request_permission END,
read_receipts_enabled = IF $hasReadReceiptsEnabled THEN $readReceiptsEnabled ELSE read_receipts_enabled END,
show_last_seen = IF $hasShowLastSeen THEN $showLastSeen ELSE show_last_seen END,
show_phone = IF $hasShowPhone THEN $showPhone ELSE show_phone END,
allow_search_by_phone = IF $hasAllowSearchByPhone THEN $allowSearchByPhone ELSE allow_search_by_phone END,
updated_at = time::now()
WHERE account = type::record($account);
SELECT is_private AS isPrivate, message_permission AS messagePermission,
friend_request_permission AS friendRequestPermission, read_receipts_enabled AS readReceiptsEnabled,
show_last_seen AS showLastSeen, show_phone AS showPhone, allow_search_by_phone AS allowSearchByPhone
FROM privacy_setting WHERE account = type::record($account) LIMIT 1;`, map[string]any{
		"account":   accountID(r),
		"isPrivate": boolValue(input.IsPrivate), "hasIsPrivate": input.IsPrivate != nil,
		"messagePermission": pointerString(input.MessagePermission), "hasMessagePermission": input.MessagePermission != nil,
		"friendRequestPermission": pointerString(input.FriendRequestPermission), "hasFriendRequestPermission": input.FriendRequestPermission != nil,
		"readReceiptsEnabled": boolValue(input.ReadReceiptsEnabled), "hasReadReceiptsEnabled": input.ReadReceiptsEnabled != nil,
		"showLastSeen": boolValue(input.ShowLastSeen), "hasShowLastSeen": input.ShowLastSeen != nil,
		"showPhone": boolValue(input.ShowPhone), "hasShowPhone": input.ShowPhone != nil,
		"allowSearchByPhone": boolValue(input.AllowSearchByPhone), "hasAllowSearchByPhone": input.AllowSearchByPhone != nil,
	}, &result)
	if err != nil || len(result) == 0 {
		s.databaseError(w, "update privacy settings", err)
		return
	}
	httpx.JSON(w, http.StatusOK, result[0])
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func (s *Server) searchUsers(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len([]rune(query)) < 2 || len([]rune(query)) > 50 {
		httpx.Error(w, http.StatusBadRequest, "invalid_query", "Arama için en az iki karakter girin.")
		return
	}
	var users []userView
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, full_name AS fullName,
display_name AS displayName, username, avatar.public_url AS avatarUrl,
(SELECT VALUE is_private FROM privacy_setting WHERE account = $parent.id LIMIT 1)[0] ?? false AS isPrivate
FROM account WHERE status = 'active' AND id != type::record($account)
AND (string::lowercase(username) CONTAINS $query OR string::lowercase(display_name) CONTAINS $query)
ORDER BY username LIMIT 30;`, map[string]any{"account": accountID(r), "query": query}, &users)
	if err != nil {
		s.databaseError(w, "search users", err)
		return
	}
	if len(users) == 0 {
		httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}

	actor := accountID(r)
	pairs := make([]string, 0, len(users))
	userByPair := make(map[string]int, len(users))
	for index := range users {
		users[index].Relationship = "none"
		pair := security.PairKey(actor, users[index].ID)
		pairs = append(pairs, pair)
		userByPair[pair] = index
	}

	var friendships []struct {
		PairKey string `json:"pairKey"`
	}
	if err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey FROM friendship WHERE pair_key IN $pairs;`, map[string]any{"pairs": pairs}, &friendships); err != nil {
		s.databaseError(w, "search friendship status", err)
		return
	}
	for _, friendship := range friendships {
		if index, ok := userByPair[friendship.PairKey]; ok {
			users[index].Relationship = "friend"
		}
	}

	var requests []struct {
		PairKey string `json:"pairKey"`
		Sender  string `json:"sender"`
	}
	if err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey, <string>sender AS sender FROM friend_request WHERE pair_key IN $pairs AND status = 'pending';`, map[string]any{"pairs": pairs}, &requests); err != nil {
		s.databaseError(w, "search friend request status", err)
		return
	}
	for _, request := range requests {
		if index, ok := userByPair[request.PairKey]; ok && users[index].Relationship == "none" {
			if request.Sender == actor {
				users[index].Relationship = "outgoing_request"
			} else {
				users[index].Relationship = "incoming_request"
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
}
