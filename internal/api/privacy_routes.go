package api

import "net/http"

func (s *Server) registerPrivacyRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/me/privacy", s.authenticated(limits.read, "read", s.getPrivacy))
	mux.Handle("PATCH /v1/me/privacy", s.authenticated(limits.read, "read", s.updatePrivacy))
}
