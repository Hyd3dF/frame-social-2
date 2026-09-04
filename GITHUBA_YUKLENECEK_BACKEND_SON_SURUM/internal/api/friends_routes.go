package api

import "net/http"

func (s *Server) registerFriendRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("POST /v1/friends/requests", s.authenticated(limits.read, "read", s.createFriendRequest))
	mux.Handle("GET /v1/friends/requests", s.authenticated(limits.read, "read", s.listFriendRequests))
	mux.Handle("POST /v1/friends/requests/{id}/respond", s.authenticated(limits.read, "read", s.respondFriendRequest))
	mux.Handle("DELETE /v1/friends/{id}", s.authenticated(limits.read, "read", s.unfriend))
}
