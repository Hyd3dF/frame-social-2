package api

import "net/http"

func (s *Server) registerAccountRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/me", s.authenticated(limits.read, "read", s.me))
	mux.Handle("PATCH /v1/me", s.authenticated(limits.read, "read", s.updateMe))
	mux.Handle("DELETE /v1/me", s.authenticated(limits.read, "read", s.deleteAccount))
}
