package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/database"
	"github.com/Hyd3dF/frame-social-2/internal/httpx"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type queryer interface {
	Query(context.Context, string, map[string]any, any) error
	Ping(context.Context) error
}

type Server struct {
	cfg    config.Config
	db     queryer
	events *messageEventBroker
	log    *slog.Logger
}

type contextKey string

const accountKey contextKey = "accountID"

func New(cfg config.Config, db *database.Client, logger *slog.Logger) http.Handler {
	server := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger}
	mux := http.NewServeMux()
	otpLimiter := newRateLimiter(5, 10*time.Minute)

	mux.HandleFunc("GET /health", server.health)
	mux.Handle("POST /v1/auth/signup/request", otpLimiter.middleware(http.HandlerFunc(server.requestSignup)))
	mux.HandleFunc("POST /v1/auth/signup/verify", server.verifySignup)
	mux.Handle("POST /v1/auth/login/request", otpLimiter.middleware(http.HandlerFunc(server.requestLogin)))
	mux.HandleFunc("POST /v1/auth/login/verify", server.verifyLogin)
	mux.HandleFunc("POST /v1/auth/refresh", server.refresh)
	mux.HandleFunc("POST /v1/auth/logout", server.logout)

	mux.Handle("GET /v1/me", server.requireAuth(http.HandlerFunc(server.me)))
	mux.Handle("PATCH /v1/me", server.requireAuth(http.HandlerFunc(server.updateMe)))
	mux.Handle("GET /v1/me/privacy", server.requireAuth(http.HandlerFunc(server.getPrivacy)))
	mux.Handle("PATCH /v1/me/privacy", server.requireAuth(http.HandlerFunc(server.updatePrivacy)))
	mux.Handle("GET /v1/users/search", server.requireAuth(http.HandlerFunc(server.searchUsers)))
	mux.Handle("POST /v1/friends/requests", server.requireAuth(http.HandlerFunc(server.createFriendRequest)))
	mux.Handle("GET /v1/friends/requests", server.requireAuth(http.HandlerFunc(server.listFriendRequests)))
	mux.Handle("POST /v1/friends/requests/{id}/respond", server.requireAuth(http.HandlerFunc(server.respondFriendRequest)))
	mux.Handle("POST /v1/users/{id}/block", server.requireAuth(http.HandlerFunc(server.blockUser)))

	mux.Handle("GET /v1/conversations", server.requireAuth(http.HandlerFunc(server.listConversations)))
	mux.Handle("GET /v1/events/messages", server.requireAuth(http.HandlerFunc(server.messageEvents)))
	mux.Handle("POST /v1/conversations/direct", server.requireAuth(http.HandlerFunc(server.createDirectConversation)))
	mux.Handle("GET /v1/conversations/{id}/messages", server.requireAuth(http.HandlerFunc(server.listMessages)))
	mux.Handle("POST /v1/conversations/{id}/messages", server.requireAuth(http.HandlerFunc(server.sendMessage)))
	mux.Handle("POST /v1/conversations/{id}/read", server.requireAuth(http.HandlerFunc(server.readConversation)))
	mux.Handle("POST /v1/conversations/{id}/delivered", server.requireAuth(http.HandlerFunc(server.deliverConversation)))
	mux.Handle("PUT /v1/messages/{id}/reactions", server.requireAuth(http.HandlerFunc(server.putReaction)))
	mux.Handle("DELETE /v1/messages/{id}/reactions/{emoji}", server.requireAuth(http.HandlerFunc(server.deleteReaction)))
	mux.Handle("PUT /v1/messages/{id}/saved", server.requireAuth(http.HandlerFunc(server.saveMessage)))
	mux.Handle("DELETE /v1/messages/{id}/saved", server.requireAuth(http.HandlerFunc(server.unsaveMessage)))
	mux.Handle("POST /v1/messages/{id}/receipt", server.requireAuth(http.HandlerFunc(server.updateReceipt)))

	limiter := newRateLimiter(120, time.Minute)
	return recoverer(logger, limiter.middleware(securityHeaders(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("database health check failed", "error", err)
		httpx.Error(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			httpx.Error(w, http.StatusUnauthorized, "unauthorized", "Oturum açmanız gerekiyor.")
			return
		}
		accountID, err := security.ParseAccessToken(s.cfg.JWTSecret, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if err != nil || !validRecord(accountID, "account") {
			httpx.Error(w, http.StatusUnauthorized, "invalid_token", "Oturum süresi dolmuş veya geçersiz.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey, accountID)))
	})
}

func accountID(r *http.Request) string { return r.Context().Value(accountKey).(string) }

func validRecord(value, table string) bool {
	return strings.HasPrefix(value, table+":") && len(value) > len(table)+1 && !strings.ContainsAny(value, " ;'\"")
}

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("request panic", "panic", recovered, "path", r.URL.Path)
				httpx.Error(w, http.StatusInternalServerError, "internal_error", "Beklenmeyen bir hata oluştu.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
