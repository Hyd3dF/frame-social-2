package api

import "net/http"

func (s *Server) registerGroupRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("POST /v1/groups", s.authenticated(limits.read, "read", s.createGroup))
	mux.Handle("GET /v1/groups/search", s.authenticated(limits.read, "read", s.searchGroups))
	mux.Handle("PATCH /v1/groups/{id}/name", s.authenticated(limits.read, "read", s.updateGroupName))
	mux.Handle("PATCH /v1/groups/{id}/description", s.authenticated(limits.read, "read", s.updateGroupDescription))
	mux.Handle("PATCH /v1/groups/{id}/image", s.authenticated(limits.read, "read", s.updateGroupImage))
	mux.Handle("PATCH /v1/groups/{id}/access", s.authenticated(limits.read, "read", s.updateGroupAccess))
	mux.Handle("POST /v1/groups/{id}/join", s.authenticated(limits.read, "read", s.joinGroup))
	mux.Handle("POST /v1/groups/{id}/invitations", s.authenticated(limits.read, "read", s.sendGroupInvitation))
	mux.Handle("POST /v1/groups/{id}/invitations/{invitationId}/accept", s.authenticated(limits.read, "read", s.acceptGroupInvitation))
	mux.Handle("POST /v1/groups/{id}/invitations/{invitationId}/reject", s.authenticated(limits.read, "read", s.rejectGroupInvitation))
	mux.Handle("DELETE /v1/groups/{id}/invitations/{invitationId}", s.authenticated(limits.read, "read", s.cancelGroupInvitation))
	mux.Handle("POST /v1/groups/{id}/join-requests", s.authenticated(limits.read, "read", s.sendGroupJoinRequest))
	mux.Handle("POST /v1/groups/{id}/join-requests/{requestId}/approve", s.authenticated(limits.read, "read", s.approveGroupJoinRequest))
	mux.Handle("POST /v1/groups/{id}/join-requests/{requestId}/reject", s.authenticated(limits.read, "read", s.rejectGroupJoinRequest))
	mux.Handle("DELETE /v1/groups/{id}/join-requests/{requestId}", s.authenticated(limits.read, "read", s.cancelGroupJoinRequest))
	mux.Handle("GET /v1/groups/{id}/members", s.authenticated(limits.read, "read", s.listGroupMembers))
	mux.Handle("POST /v1/groups/{id}/leave", s.authenticated(limits.read, "read", s.leaveGroup))
	mux.Handle("DELETE /v1/groups/{id}/members/{userId}", s.authenticated(limits.read, "read", s.removeGroupMember))
	mux.Handle("POST /v1/groups/{id}/ownership", s.authenticated(limits.read, "read", s.transferGroupOwnership))
	mux.Handle("PATCH /v1/groups/{id}/members/{userId}/role", s.authenticated(limits.read, "read", s.changeGroupRole))
}
