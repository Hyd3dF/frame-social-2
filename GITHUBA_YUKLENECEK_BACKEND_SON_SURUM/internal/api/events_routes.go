package api

import "net/http"

func (s *Server) registerEventRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/events/messages", s.authenticated(limits.events, "events", s.messageEvents))
	mux.Handle("GET /v1/events/stream", s.authenticated(limits.events, "events", s.messageEventsStream))
}
