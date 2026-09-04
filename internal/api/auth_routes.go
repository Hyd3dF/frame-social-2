package api

import "net/http"

func (s *Server) registerAuthRoutes(mux *http.ServeMux, limits routeLimiters) {
	mux.Handle("POST /v1/auth/signup/request", limits.otp.middleware(http.HandlerFunc(s.requestSignup)))
	mux.Handle("POST /v1/auth/signup/verify", limits.verify.middleware(http.HandlerFunc(s.verifySignup)))
	mux.Handle("POST /v1/auth/login/request", limits.otp.middleware(http.HandlerFunc(s.requestLogin)))
	mux.Handle("POST /v1/auth/login/verify", limits.verify.middleware(http.HandlerFunc(s.verifyLogin)))
	mux.HandleFunc("POST /v1/auth/refresh", s.refresh)
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
}
