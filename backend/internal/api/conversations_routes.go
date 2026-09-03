package api

import "net/http"

func (s *Server) registerConversationRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/conversations", s.authenticated(limits.read, "read", s.listConversations))
	mux.Handle("POST /v1/conversations/direct", s.authenticated(limits.read, "read", s.createDirectConversation))
}
