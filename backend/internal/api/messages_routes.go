package api

import "net/http"

func (s *Server) registerMessageRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("GET /v1/conversations/{id}/messages", s.authenticated(limits.read, "read", s.listMessages))
	mux.Handle("POST /v1/conversations/{id}/messages", s.requireAuth(http.HandlerFunc(s.sendMessage)))
	mux.Handle("POST /v1/conversations/{id}/read", s.authenticated(limits.read, "read", s.readConversation))
	mux.Handle("POST /v1/conversations/{id}/delivered", s.authenticated(limits.read, "read", s.deliverConversation))
	mux.Handle("PUT /v1/messages/{id}/reactions", s.authenticated(limits.read, "read", s.putReaction))
	mux.Handle("DELETE /v1/messages/{id}/reactions/{emoji}", s.authenticated(limits.read, "read", s.deleteReaction))
	mux.Handle("PUT /v1/messages/{id}/saved", s.authenticated(limits.read, "read", s.saveMessage))
	mux.Handle("DELETE /v1/messages/{id}/saved", s.authenticated(limits.read, "read", s.unsaveMessage))
	mux.Handle("POST /v1/messages/{id}/receipt", s.authenticated(limits.read, "read", s.updateReceipt))
	mux.Handle("DELETE /v1/messages/{id}/for-me", s.authenticated(limits.read, "read", s.deleteMessageForMe))
	mux.Handle("DELETE /v1/messages/{id}/for-everyone", s.authenticated(limits.read, "read", s.deleteMessageForEveryone))
	mux.Handle("POST /v1/messages/{id}/retract", s.authenticated(limits.read, "read", s.retractMessage))
}
