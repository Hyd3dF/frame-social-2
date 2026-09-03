package api

import (
	"net/http"
	"time"
)

// routes is the API compatibility boundary. Feature route registration stays
// separate so a new module does not require changing unrelated endpoints.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	limits := routeLimiters{
		otp:    newRateLimiterWithBucket(5, 10*time.Minute, "otp", s.log),
		verify: newRateLimiterWithBucket(10, time.Minute, "verify", s.log),
		read:   newEndpointLimiter(300, time.Minute, "read", s.log),
		events: newEndpointLimiter(120, time.Minute, "events", s.log),
	}
	mux.HandleFunc("GET /health", s.health)
	s.registerAuthRoutes(mux, limits)
	s.registerAccountRoutes(mux, limits)
	s.registerUserRoutes(mux, limits)
	s.registerFriendRoutes(mux, limits)
	s.registerBlockingRoutes(mux, limits)
	s.registerPrivacyRoutes(mux, limits)
	s.registerConversationRoutes(mux, limits)
	s.registerGroupRoutes(mux, limits)
	s.registerMessageRoutes(mux, limits)
	s.registerNotificationRoutes(mux, limits)
	s.registerEventRoutes(mux, limits)
	return mux
}

type routeLimiters struct {
	otp    *rateLimiter
	verify *rateLimiter
	read   *endpointLimiter
	events *endpointLimiter
}

func (s *Server) authenticated(limiter *endpointLimiter, bucket string, handler http.HandlerFunc) http.Handler {
	return s.requireAuth(limiter.middleware(accountKeyFunc(bucket), handler))
}
