package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/database"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type queryer interface {
	Query(context.Context, string, map[string]any, any) error
	Ping(context.Context) error
}

type Server struct {
	cfg     config.Config
	db      queryer
	events  *messageEventBroker
	log     *slog.Logger
	members *memberCache
	pending *pendingStore
	persist *persister
}

type contextKey string

const accountKey contextKey = "accountID"

type rateEntry struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	entries map[string]rateEntry
	limit   int
	mu      sync.Mutex
	window  time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{entries: make(map[string]rateEntry), limit: limit, window: window}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 10000 {
		for k, v := range l.entries {
			if v.reset.Before(now) {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[key]
	if e.reset.Before(now) {
		e = rateEntry{reset: now.Add(l.window)}
	}
	e.count++
	l.entries[key] = e
	return e.count <= l.limit
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r) + ":" + r.URL.Path) {
			respondError(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	host = strings.Trim(host, "[]")
	if host != "" {
		return host
	}
	return r.RemoteAddr
}

func New(cfg config.Config, db *database.Client, logger *slog.Logger) http.Handler {
	mc := newMemberCache()
	ps := newPendingStore()
	server := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	server.persist = newPersister(db, ps, mc, logger)
	mux := http.NewServeMux()
	otpLimiter := newRateLimiter(5, 10*time.Minute)
	verifyLimiter := newRateLimiter(10, time.Minute)

	mux.HandleFunc("GET /health", server.health)
	mux.Handle("POST /v1/auth/signup/request", otpLimiter.middleware(http.HandlerFunc(server.requestSignup)))
	mux.Handle("POST /v1/auth/signup/verify", verifyLimiter.middleware(http.HandlerFunc(server.verifySignup)))
	mux.Handle("POST /v1/auth/login/request", otpLimiter.middleware(http.HandlerFunc(server.requestLogin)))
	mux.Handle("POST /v1/auth/login/verify", verifyLimiter.middleware(http.HandlerFunc(server.verifyLogin)))
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
	mux.Handle("GET /v1/events/stream", server.requireAuth(http.HandlerFunc(server.messageEventsStream)))
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
	return recoverer(logger, securityHeaders(limiter.middleware(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("database health check failed", "error", err)
		respondError(w, http.StatusServiceUnavailable, "service_unavailable", "Servis geçici olarak kullanılamıyor.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			respondError(w, http.StatusUnauthorized, "unauthorized", "Oturum açmanız gerekiyor.")
			return
		}
		accountID, err := security.ParseAccessToken(s.cfg.JWTSecret, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if err != nil || !validRecord(accountID, "account") {
			respondError(w, http.StatusUnauthorized, "invalid_token", "Oturum süresi dolmuş veya geçersiz.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey, accountID)))
	})
}

func accountID(r *http.Request) string { return r.Context().Value(accountKey).(string) }

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("request panic", "panic", recovered, "path", r.URL.Path)
				respondError(w, http.StatusInternalServerError, "internal_error", "Beklenmeyen bir hata oluştu.")
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
