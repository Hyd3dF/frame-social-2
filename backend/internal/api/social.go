package api

import (
	"net/http"
	"strings"
	"sync"

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
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already contains") || strings.Contains(msg, "already exists") || strings.Contains(msg, "record already exists")
}

func (s *Server) unblockUser(w http.ResponseWriter, r *http.Request) {
	target := normalizeRecordID(r.PathValue("id"), "account")
	actor := accountID(r)
	if !validRecord(target, "account") || !validRecord(actor, "account") {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	if target == actor {
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
	account := accountID(r)
	type blockedUserView struct {
		AvatarURL   *string `json:"avatarUrl"`
		DisplayName string  `json:"displayName"`
		FullName    string  `json:"fullName"`
		ID          string  `json:"id"`
		Username    string  `json:"username"`
	}
	var users []blockedUserView
	err := s.db.Query(r.Context(), `SELECT <string>out AS id, out.full_name AS fullName, out.display_name AS displayName, out.username AS username, out.avatar.public_url AS avatarUrl FROM blocked_account WHERE in = type::record($account) ORDER BY created_at DESC LIMIT 100;`, map[string]any{"account": account}, &users)
	if err != nil {
		s.databaseError(w, "list blocked users", err)
		return
	}
	if users == nil {
		users = []blockedUserView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"users": users})
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

func (s *Server) searchUsers(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len([]rune(query)) < 2 || len([]rune(query)) > 50 {
		respondError(w, http.StatusBadRequest, "invalid_query", "Arama için en az iki karakter girin.")
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
		respondJSON(w, http.StatusOK, map[string]any{"users": users})
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
	var requests []struct {
		PairKey string `json:"pairKey"`
		Sender  string `json:"sender"`
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var firstOp string
	wg.Add(2)
	go func() {
		defer wg.Done()
		var f []struct {
			PairKey string `json:"pairKey"`
		}
		err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey FROM friendship WHERE pair_key IN $pairs;`, map[string]any{"pairs": pairs}, &f)
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
			firstOp = "search friendship status"
		}
		friendships = f
	}()
	go func() {
		defer wg.Done()
		var rq []struct {
			PairKey string `json:"pairKey"`
			Sender  string `json:"sender"`
		}
		err := s.db.Query(r.Context(), `SELECT pair_key AS pairKey, <string>sender AS sender FROM friend_request WHERE pair_key IN $pairs AND status = 'pending';`, map[string]any{"pairs": pairs}, &rq)
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
			firstOp = "search friend request status"
		}
		requests = rq
	}()
	wg.Wait()
	if firstErr != nil {
		s.databaseError(w, firstOp, firstErr)
		return
	}
	for _, friendship := range friendships {
		if index, ok := userByPair[friendship.PairKey]; ok {
			users[index].Relationship = "friend"
		}
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
	respondJSON(w, http.StatusOK, map[string]any{"users": users})
}
