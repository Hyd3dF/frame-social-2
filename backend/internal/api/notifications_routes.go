package api

import "net/http"

func (s *Server) registerNotificationRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("PUT /v1/me/push-devices", s.authenticated(limits.read, "read", s.putPushDevice))
	mux.Handle("DELETE /v1/me/push-devices/{deviceId}", s.authenticated(limits.read, "read", s.deletePushDevice))
}
