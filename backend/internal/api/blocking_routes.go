package api

import "net/http"

func (s *Server) registerBlockingRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("POST /v1/users/{id}/block", s.authenticated(limits.read, "read", s.blockUser))
	mux.Handle("DELETE /v1/users/{id}/block", s.authenticated(limits.read, "read", s.unblockUser))
	mux.Handle("GET /v1/me/blocked-users", s.authenticated(limits.read, "read", s.listBlockedUsers))
}
