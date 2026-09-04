package api

import "net/http"

func (s *Server) registerUserRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/users/search", s.authenticated(limits.read, "read", s.searchUsers))
}
