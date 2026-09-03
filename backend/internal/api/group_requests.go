package api

import (
	"context"
	"net/http"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

func groupRequestID(raw, table string) string {
	return normalizeRecordID(raw, table)
}

func (s *Server) sendGroupInvitation(w http.ResponseWriter, r *http.Request) {
	group, _, ok := s.groupRole(w, r, true)
	if !ok {
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
		respondError(w, http.StatusBadRequest, "invalid_user", "Kendinizi davet edemezsiniz.")
		return
	}
	if blocked, _, err := s.isBlockedPair(r.Context(), accountID(r), input.UserID); err != nil {
		s.databaseError(w, "check group invitation block", err)
		return
	} else if blocked {
		respondError(w, http.StatusForbidden, "blocked", "Engellenen kullanıcılar davet edilemez.")
		return
	}
	if role, err := newGroupStore(s.db).role(r.Context(), group, input.UserID); err != nil {
		s.databaseError(w, "check group membership", err)
		return
	} else if role != "" {
		respondError(w, http.StatusConflict, "already_member", "Kullanıcı zaten grup üyesi.")
		return
	}
	id, err := newGroupStore(s.db).invitation(r.Context(), group, accountID(r), input.UserID)
	if err != nil {
		s.databaseError(w, "create group invitation", err)
		return
	}
	respondJSON(w, http.StatusCreated, recordID{ID: id})
}

func (s *Server) acceptGroupInvitation(w http.ResponseWriter, r *http.Request) {
	s.respondGroupInvitation(w, r, "accepted")
}

func (s *Server) rejectGroupInvitation(w http.ResponseWriter, r *http.Request) {
	s.respondGroupInvitation(w, r, "rejected")
}

func (s *Server) cancelGroupInvitation(w http.ResponseWriter, r *http.Request) {
	s.respondGroupInvitation(w, r, "cancelled")
}

func (s *Server) respondGroupInvitation(w http.ResponseWriter, r *http.Request, status string) {
	group := groupConversationID(r.PathValue("id"))
	invite := groupRequestID(r.PathValue("invitationId"), "group_invitation")
	if !validRecord(group, "conversation") || !validRecord(invite, "group_invitation") {
		respondError(w, http.StatusBadRequest, "invalid_invitation", "Grup daveti geçersiz.")
		return
	}
	account := accountID(r)
	if status == "cancelled" {
		if _, _, ok := s.groupRole(w, r, true); !ok {
			return
		}
		if err := s.db.Query(r.Context(), `UPDATE type::record($invite) SET status = 'cancelled', responded_at = time::now() WHERE group = type::record($group) AND sender = type::record($account) AND status = 'pending';`, map[string]any{"invite": invite, "group": group, "account": account}, nil); err != nil {
			s.databaseError(w, "cancel group invitation", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var inviteRow []struct {
		Recipient string `json:"recipient"`
		Status    string `json:"status"`
	}
	if err := s.db.Query(r.Context(), `SELECT <string>recipient AS recipient, status FROM type::record($invite) WHERE group = type::record($group) LIMIT 1;`, map[string]any{"invite": invite, "group": group}, &inviteRow); err != nil {
		s.databaseError(w, "read group invitation", err)
		return
	}
	if len(inviteRow) == 0 || inviteRow[0].Recipient != account || inviteRow[0].Status != "pending" {
		respondError(w, http.StatusNotFound, "invitation_not_found", "Grup daveti bulunamadı.")
		return
	}
	if status == "accepted" {
		if err := newGroupStore(s.db).addMember(r.Context(), group, account, "member"); err != nil {
			s.databaseError(w, "accept group invitation", err)
			return
		}
	}
	if err := s.db.Query(r.Context(), `UPDATE type::record($invite) SET status = $status, responded_at = time::now() WHERE group = type::record($group) AND recipient = type::record($account) AND status = 'pending';`, map[string]any{"invite": invite, "group": group, "account": account, "status": status}, nil); err != nil {
		s.databaseError(w, "respond group invitation", err)
		return
	}
	s.members.Delete(group)
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendGroupJoinRequest(w http.ResponseWriter, r *http.Request) {
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
	if view.JoinRule != "approval" {
		respondError(w, http.StatusForbidden, "join_request_not_allowed", "Bu grup katılım isteği kabul etmiyor.")
		return
	}
	if role, err := store.role(r.Context(), group, accountID(r)); err != nil {
		s.databaseError(w, "read group membership", err)
		return
	} else if role != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id, err := store.joinRequest(r.Context(), group, accountID(r))
	if err != nil {
		s.databaseError(w, "create group join request", err)
		return
	}
	respondJSON(w, http.StatusCreated, recordID{ID: id})
}

func (s *Server) approveGroupJoinRequest(w http.ResponseWriter, r *http.Request) {
	s.respondGroupJoinRequest(w, r, "approved")
}

func (s *Server) rejectGroupJoinRequest(w http.ResponseWriter, r *http.Request) {
	s.respondGroupJoinRequest(w, r, "rejected")
}

func (s *Server) cancelGroupJoinRequest(w http.ResponseWriter, r *http.Request) {
	s.respondGroupJoinRequest(w, r, "cancelled")
}

func (s *Server) respondGroupJoinRequest(w http.ResponseWriter, r *http.Request, status string) {
	group := groupConversationID(r.PathValue("id"))
	request := groupRequestID(r.PathValue("requestId"), "group_join_request")
	if !validRecord(group, "conversation") || !validRecord(request, "group_join_request") {
		respondError(w, http.StatusBadRequest, "invalid_join_request", "Katılım isteği geçersiz.")
		return
	}
	account := accountID(r)
	if status == "cancelled" {
		if err := s.db.Query(r.Context(), `UPDATE type::record($request) SET status = 'cancelled', responded_at = time::now() WHERE group = type::record($group) AND account = type::record($account) AND status = 'pending';`, map[string]any{"request": request, "group": group, "account": account}, nil); err != nil {
			s.databaseError(w, "cancel group join request", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, _, ok := s.groupRole(w, r, true); !ok {
		return
	}
	var rows []struct {
		Account string `json:"account"`
		Status  string `json:"status"`
	}
	if err := s.db.Query(r.Context(), `SELECT <string>account AS account, status FROM type::record($request) WHERE group = type::record($group) LIMIT 1;`, map[string]any{"request": request, "group": group}, &rows); err != nil {
		s.databaseError(w, "read group join request", err)
		return
	}
	if len(rows) == 0 || rows[0].Status != "pending" {
		respondError(w, http.StatusNotFound, "join_request_not_found", "Katılım isteği bulunamadı.")
		return
	}
	if status == "approved" {
		if err := newGroupStore(s.db).addMember(r.Context(), group, rows[0].Account, "member"); err != nil {
			s.databaseError(w, "approve group join request", err)
			return
		}
	}
	if err := s.db.Query(r.Context(), `UPDATE type::record($request) SET status = $status, responded_at = time::now() WHERE group = type::record($group) AND status = 'pending';`, map[string]any{"request": request, "group": group, "status": status}, nil); err != nil {
		s.databaseError(w, "respond group join request", err)
		return
	}
	s.members.Delete(group)
	s.publishConversation(r.Context(), group)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) isBlockedPair(ctx context.Context, a, b string) (bool, bool, error) {
	pair := security.PairKey(a, b)
	var blocked []recordID
	err := s.db.Query(ctx, `SELECT <string>id AS id FROM blocked_account WHERE pair_key = $pair LIMIT 1;`, map[string]any{"pair": pair}, &blocked)
	if err != nil {
		return false, false, err
	}
	return len(blocked) > 0, true, nil
}
