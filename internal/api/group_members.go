package api

import "net/http"

func (s *Server) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	group, _, ok := s.groupRole(w, r, false)
	if !ok {
		return
	}
	var members []groupMemberView
	err := s.db.Query(r.Context(), `SELECT <string>in.id AS id, in.full_name AS fullName, in.display_name AS displayName, in.username AS username, in.avatar.public_url AS avatarUrl, false AS isPrivate, 'none' AS relationship, role AS role, joined_at AS joinedAt
		FROM conversation_member WHERE out = type::record($group) AND left_at IS NONE ORDER BY role, joined_at LIMIT 500;`, map[string]any{"group": group}, &members)
	if err != nil {
		s.databaseError(w, "list group members", err)
		return
	}
	if members == nil {
		members = []groupMemberView{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) leaveGroup(w http.ResponseWriter, r *http.Request) {
	group, role, ok := s.groupRole(w, r, false)
	if !ok {
		return
	}
	if role == "owner" {
		var owners []struct {
			ID string `json:"id"`
		}
		_ = s.db.Query(r.Context(), `SELECT <string>in.id AS id FROM conversation_member WHERE out = type::record($group) AND role = 'owner' AND left_at IS NONE;`, map[string]any{"group": group}, &owners)
		if len(owners) <= 1 {
			respondError(w, http.StatusConflict, "ownership_transfer_required", "Grup sahibi ayrılmadan önce sahipliği devretmelidir.")
			return
		}
	}
	if err := s.db.Query(r.Context(), `UPDATE conversation_member SET left_at = time::now() WHERE in = type::record($account) AND out = type::record($group) AND left_at IS NONE;`, map[string]any{"group": group, "account": accountID(r)}, nil); err != nil {
		s.databaseError(w, "leave group", err)
		return
	}
	s.members.Delete(group)
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	group, actorRole, ok := s.groupRole(w, r, true)
	if !ok {
		return
	}
	target := normalizeRecordID(r.PathValue("userId"), "account")
	if !validRecord(target, "account") || target == accountID(r) {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	targetRole, err := newGroupStore(s.db).role(r.Context(), group, target)
	if err != nil {
		s.databaseError(w, "read target group role", err)
		return
	}
	if targetRole == "" {
		respondError(w, http.StatusNotFound, "member_not_found", "Grup üyesi bulunamadı.")
		return
	}
	if targetRole == "owner" || (actorRole == "admin" && targetRole == "admin") {
		respondError(w, http.StatusForbidden, "forbidden", "Bu üyeyi çıkaramazsınız.")
		return
	}
	if err := s.db.Query(r.Context(), `UPDATE conversation_member SET left_at = time::now() WHERE in = type::record($target) AND out = type::record($group) AND left_at IS NONE;`, map[string]any{"group": group, "target": target}, nil); err != nil {
		s.databaseError(w, "remove group member", err)
		return
	}
	s.members.Delete(group)
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) transferGroupOwnership(w http.ResponseWriter, r *http.Request) {
	group, role, ok := s.groupRole(w, r, false)
	if !ok {
		return
	}
	if role != "owner" {
		respondError(w, http.StatusForbidden, "forbidden", "Sahiplik yalnızca grup sahibi tarafından devredilebilir.")
		return
	}
	var input struct {
		UserID string `json:"userId"`
	}
	if !decode(w, r, &input) || !validRecord(input.UserID, "account") {
		if input.UserID != "" {
			respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		}
		return
	}
	if input.UserID == accountID(r) {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kendinize sahiplik devredemezsiniz.")
		return
	}
	targetRole, err := newGroupStore(s.db).role(r.Context(), group, input.UserID)
	if err != nil {
		s.databaseError(w, "read target group role", err)
		return
	}
	if targetRole == "" {
		respondError(w, http.StatusNotFound, "member_not_found", "Grup üyesi bulunamadı.")
		return
	}
	if err := s.db.Query(r.Context(), `BEGIN TRANSACTION;
		UPDATE conversation_member SET role = 'owner' WHERE in = type::record($target) AND out = type::record($group) AND left_at IS NONE;
		UPDATE conversation_member SET role = 'admin' WHERE in = type::record($owner) AND out = type::record($group) AND left_at IS NONE;
		COMMIT TRANSACTION;`, map[string]any{"group": group, "target": input.UserID, "owner": accountID(r)}, nil); err != nil {
		s.databaseError(w, "transfer group ownership", err)
		return
	}
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changeGroupRole(w http.ResponseWriter, r *http.Request) {
	group, role, ok := s.groupRole(w, r, false)
	if !ok {
		return
	}
	if role != "owner" {
		respondError(w, http.StatusForbidden, "forbidden", "Rolleri yalnızca grup sahibi değiştirebilir.")
		return
	}
	target := normalizeRecordID(r.PathValue("userId"), "account")
	var input struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validRecord(target, "account") {
		respondError(w, http.StatusBadRequest, "invalid_user", "Kullanıcı geçersiz.")
		return
	}
	if input.Role != "admin" && input.Role != "member" {
		respondError(w, http.StatusBadRequest, "invalid_role", "Rol geçersiz.")
		return
	}
	if target == accountID(r) {
		respondError(w, http.StatusBadRequest, "invalid_role", "Grup sahibinin rolü değiştirilemez.")
		return
	}
	targetRole, err := newGroupStore(s.db).role(r.Context(), group, target)
	if err != nil {
		s.databaseError(w, "read target group role", err)
		return
	}
	if targetRole == "" {
		respondError(w, http.StatusNotFound, "member_not_found", "Grup üyesi bulunamadı.")
		return
	}
	if targetRole == "owner" {
		respondError(w, http.StatusForbidden, "forbidden", "Grup sahibinin rolü değiştirilemez.")
		return
	}
	if err := s.db.Query(r.Context(), `UPDATE conversation_member SET role = $role WHERE in = type::record($target) AND out = type::record($group) AND left_at IS NONE;`, map[string]any{"group": group, "target": target, "role": input.Role}, nil); err != nil {
		s.databaseError(w, "change group role", err)
		return
	}
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}
