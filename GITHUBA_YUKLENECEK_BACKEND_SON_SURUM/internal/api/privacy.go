package api

import "net/http"

type privacyView struct {
	AllowSearchByPhone      bool   `json:"allowSearchByPhone"`
	FriendRequestPermission string `json:"friendRequestPermission"`
	IsPrivate               bool   `json:"isPrivate"`
	MessagePermission       string `json:"messagePermission"`
	ReadReceiptsEnabled     bool   `json:"readReceiptsEnabled"`
	ShowLastSeen            bool   `json:"showLastSeen"`
	ShowPhone               bool   `json:"showPhone"`
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
	respondJSON(w, http.StatusOK, result[0])
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
	if !decode(w, r, &input) {
		return
	}
	if input.MessagePermission != nil && *input.MessagePermission != "everyone" && *input.MessagePermission != "friends" && *input.MessagePermission != "nobody" {
		respondError(w, http.StatusBadRequest, "invalid_privacy", "Mesaj izni geçersiz.")
		return
	}
	if input.FriendRequestPermission != nil && *input.FriendRequestPermission != "everyone" && *input.FriendRequestPermission != "nobody" {
		respondError(w, http.StatusBadRequest, "invalid_privacy", "Arkadaşlık isteği izni geçersiz.")
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
	respondJSON(w, http.StatusOK, result[0])
}
