package api

import (
	"net/http"
	"regexp"
	"strings"
)

var groupIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,48}$`)

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Description string  `json:"description"`
		ID          string  `json:"id"`
		ImageURL    *string `json:"imageUrl"`
		JoinRule    string  `json:"joinRule"`
		Name        string  `json:"name"`
		Password    string  `json:"password"`
		Privacy     string  `json:"privacy"`
	}
	if !decode(w, r, &input) {
		return
	}
	raw := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input.ID, "conversation:")))
	raw = strings.TrimSpace(raw)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if !groupIDPattern.MatchString(raw) {
		respondError(w, http.StatusBadRequest, "invalid_group_id", "Grup kimliği geçersiz.")
		return
	}
	if len([]rune(input.Name)) < 2 || len([]rune(input.Name)) > 80 {
		respondError(w, http.StatusBadRequest, "invalid_group_name", "Grup adı geçersiz.")
		return
	}
	if len([]rune(input.Description)) > 500 {
		respondError(w, http.StatusBadRequest, "invalid_group_description", "Grup açıklaması geçersiz.")
		return
	}
	if input.ImageURL != nil && len(*input.ImageURL) > 2000 {
		respondError(w, http.StatusBadRequest, "invalid_group_image", "Grup görseli geçersiz.")
		return
	}
	if input.Privacy == "" {
		input.Privacy = "public"
	}
	if input.JoinRule == "" {
		input.JoinRule = "open"
	}
	input.Privacy = strings.ToLower(strings.TrimSpace(input.Privacy))
	input.JoinRule = strings.ToLower(strings.TrimSpace(input.JoinRule))
	if (input.Privacy != "public" && input.Privacy != "private") || (input.JoinRule != "open" && input.JoinRule != "invite" && input.JoinRule != "approval" && input.JoinRule != "password") {
		respondError(w, http.StatusBadRequest, "invalid_group_access", "Grup erişim ayarları geçersiz.")
		return
	}
	passwordHash := ""
	if input.JoinRule == "password" {
		if len(input.Password) < 8 || len(input.Password) > 128 {
			respondError(w, http.StatusBadRequest, "invalid_password", "Grup parolası geçersiz.")
			return
		}
		hash, err := hashGroupPassword(input.Password)
		if err != nil {
			s.internalError(w, "hash group password", err)
			return
		}
		passwordHash = hash
	}
	group := groupConversationID(raw)
	store := newGroupStore(s.db)
	if existing, found, err := store.group(r.Context(), group); err != nil {
		s.databaseError(w, "create group lookup", err)
		return
	} else if found && existing.ID != "" {
		respondError(w, http.StatusConflict, "group_exists", "Grup kimliği zaten kullanılıyor.")
		return
	}
	view := groupView{
		ID:          group,
		Name:        input.Name,
		Description: input.Description,
		ImageURL:    input.ImageURL,
		Privacy:     input.Privacy,
		JoinRule:    input.JoinRule,
	}
	if err := store.create(r.Context(), group, accountID(r), view, passwordHash); err != nil {
		if isAlreadyExistsError(err) {
			respondError(w, http.StatusConflict, "group_exists", "Grup kimliği zaten kullanılıyor.")
			return
		}
		s.databaseError(w, "create group", err)
		return
	}
	s.members.Set(group, []string{accountID(r)})
	s.events.publish([]string{accountID(r)}, group)
	respondJSON(w, http.StatusCreated, view)
}

func (s *Server) searchGroups(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if query == "" {
		respondError(w, http.StatusBadRequest, "invalid_group_id", "Grup kimliği geçersiz.")
		return
	}
	query = strings.TrimPrefix(query, "conversation:")
	if len(query) < 2 || len(query) > 49 || !groupIDPattern.MatchString(query) {
		respondError(w, http.StatusBadRequest, "invalid_group_id", "Grup kimliği geçersiz.")
		return
	}
	var groups []groupView
	err := s.db.Query(r.Context(), `SELECT <string>id AS id, group_name AS name, group_description AS description, group_image_url AS imageUrl, group_privacy AS privacy, group_join_rule AS joinRule
		FROM conversation WHERE kind = 'group' AND string::lowercase(group_id) CONTAINS $query
		AND (group_privacy = 'public' OR array::len(SELECT id FROM conversation_member WHERE in = type::record($account) AND out = $parent.id AND left_at IS NONE) > 0)
		ORDER BY group_id LIMIT 20;`, map[string]any{"query": query, "account": accountID(r)}, &groups)
	if err != nil {
		s.databaseError(w, "search groups", err)
		return
	}
	if groups == nil {
		groups = []groupView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) updateGroupName(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if len([]rune(input.Name)) < 2 || len([]rune(input.Name)) > 80 {
		respondError(w, http.StatusBadRequest, "invalid_group_name", "Grup adı geçersiz.")
		return
	}
	s.updateGroup(w, r, map[string]any{"name": input.Name, "hasName": true})
}

func (s *Server) updateGroupDescription(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Description string `json:"description"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Description = strings.TrimSpace(input.Description)
	if len([]rune(input.Description)) > 500 {
		respondError(w, http.StatusBadRequest, "invalid_group_description", "Grup açıklaması geçersiz.")
		return
	}
	s.updateGroup(w, r, map[string]any{"description": input.Description, "hasDescription": true})
}

func (s *Server) updateGroupImage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ImageURL *string `json:"imageUrl"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.ImageURL != nil && len(*input.ImageURL) > 2000 {
		respondError(w, http.StatusBadRequest, "invalid_group_image", "Grup görseli geçersiz.")
		return
	}
	s.updateGroup(w, r, map[string]any{"image": input.ImageURL, "hasImage": true})
}

func (s *Server) updateGroupAccess(w http.ResponseWriter, r *http.Request) {
	var input struct {
		JoinRule string  `json:"joinRule"`
		Password *string `json:"password"`
		Privacy  string  `json:"privacy"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Privacy = strings.ToLower(strings.TrimSpace(input.Privacy))
	input.JoinRule = strings.ToLower(strings.TrimSpace(input.JoinRule))
	if (input.Privacy != "public" && input.Privacy != "private") || (input.JoinRule != "open" && input.JoinRule != "invite" && input.JoinRule != "approval" && input.JoinRule != "password") {
		respondError(w, http.StatusBadRequest, "invalid_group_access", "Grup erişim ayarları geçersiz.")
		return
	}
	values := map[string]any{
		"privacy":     input.Privacy,
		"hasPrivacy":  true,
		"joinRule":    input.JoinRule,
		"hasJoinRule": true,
	}
	if input.JoinRule == "password" {
		if input.Password == nil || len(*input.Password) < 8 || len(*input.Password) > 128 {
			respondError(w, http.StatusBadRequest, "invalid_password", "Grup parolası geçersiz.")
			return
		}
		hash, err := hashGroupPassword(*input.Password)
		if err != nil {
			s.internalError(w, "hash group password", err)
			return
		}
		values["password"] = hash
		values["hasPassword"] = true
	} else {
		values["password"] = ""
		values["hasPassword"] = false
	}
	s.updateGroup(w, r, values)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, values map[string]any) {
	group, _, ok := s.groupRole(w, r, true)
	if !ok {
		return
	}
	values["group"] = group
	for _, key := range []string{"hasName", "hasDescription", "hasImage", "hasPrivacy", "hasJoinRule", "hasPassword"} {
		if _, exists := values[key]; !exists {
			values[key] = false
		}
	}
	if _, exists := values["name"]; !exists {
		values["name"] = ""
	}
	if _, exists := values["description"]; !exists {
		values["description"] = ""
	}
	if _, exists := values["image"]; !exists {
		values["image"] = nil
	}
	if _, exists := values["privacy"]; !exists {
		values["privacy"] = ""
	}
	if _, exists := values["joinRule"]; !exists {
		values["joinRule"] = ""
	}
	if _, exists := values["password"]; !exists {
		values["password"] = ""
	}
	store := newGroupStore(s.db)
	if err := store.update(r.Context(), group, values); err != nil {
		s.databaseError(w, "update group", err)
		return
	}
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) groupRole(w http.ResponseWriter, r *http.Request, admin bool) (string, string, bool) {
	group := groupConversationID(r.PathValue("id"))
	if !validRecord(group, "conversation") {
		respondError(w, http.StatusBadRequest, "invalid_group", "Grup geçersiz.")
		return "", "", false
	}
	role, err := newGroupStore(s.db).role(r.Context(), group, accountID(r))
	if err != nil {
		s.databaseError(w, "read group role", err)
		return "", "", false
	}
	if role == "" || (admin && role != "owner" && role != "admin") {
		respondError(w, http.StatusForbidden, "forbidden", "Bu grup işlemi için izniniz yok.")
		return "", "", false
	}
	return group, role, true
}

func (s *Server) joinGroup(w http.ResponseWriter, r *http.Request) {
	group := groupConversationID(r.PathValue("id"))
	if !validRecord(group, "conversation") {
		respondError(w, http.StatusBadRequest, "invalid_group", "Grup geçersiz.")
		return
	}
	store := newGroupStore(s.db)
	view, found, err := store.group(r.Context(), group)
	if err != nil {
		s.databaseError(w, "read group", err)
		return
	}
	if !found {
		respondError(w, http.StatusNotFound, "group_not_found", "Grup bulunamadı.")
		return
	}
	role, err := store.role(r.Context(), group, accountID(r))
	if err != nil {
		s.databaseError(w, "read group membership", err)
		return
	}
	if role != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if r.ContentLength != 0 {
		if r.Header.Get("Content-Type") != "" {
			if !decode(w, r, &input) {
				return
			}
		}
	}
	switch view.JoinRule {
	case "invite":
		respondError(w, http.StatusForbidden, "invite_required", "Bu grup davet gerektiriyor.")
		return
	case "approval":
		id, err := store.joinRequest(r.Context(), group, accountID(r))
		if err != nil {
			s.databaseError(w, "create group join request", err)
			return
		}
		respondJSON(w, http.StatusAccepted, recordID{ID: id})
		return
	case "password":
		var hashes []struct {
			Hash string `json:"hash"`
		}
		err := s.db.Query(r.Context(), `SELECT group_password_hash AS hash FROM type::record($group) LIMIT 1;`, map[string]any{"group": group}, &hashes)
		if err != nil {
			s.databaseError(w, "read group password", err)
			return
		}
		if len(hashes) == 0 || hashes[0].Hash == "" || !verifyGroupPassword(hashes[0].Hash, input.Password) {
			respondError(w, http.StatusForbidden, "invalid_password", "Grup parolası geçersiz.")
			return
		}
	}
	if err := store.addMember(r.Context(), group, accountID(r), "member"); err != nil {
		s.databaseError(w, "join group", err)
		return
	}
	s.members.Delete(group)
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}
