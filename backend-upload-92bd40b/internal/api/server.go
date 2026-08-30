package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"net/http"
	"strconv"
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
	cfg       config.Config
	db        queryer
	events    *messageEventBroker
	log       *slog.Logger
	members   *memberCache
	pending   *pendingStore
	persist   *persister
	limiter   messageRateLimiter
	pushStore *pushStore
	pusher    Pusher
}

type contextKey string

const accountKey contextKey = "accountID"
const requestIDKey contextKey = "requestID"

type rateEntry struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	entries map[string]rateEntry
	limit   int
	mu      sync.Mutex
	window  time.Duration
	bucket  string
	log     *slog.Logger
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, bucket: "global", log: slog.Default()}
}

func newRateLimiterWithBucket(limit int, window time.Duration, bucket string, log *slog.Logger) *rateLimiter {
	if log == nil {
		log = slog.Default()
	}
	return &rateLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, bucket: bucket, log: log}
}

func (l *rateLimiter) allowWithInfo(key string) (bool, int, time.Time) {
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
	if e.count <= l.limit {
		return true, 0, time.Time{}
	}
	retry := int(math.Ceil(e.reset.Sub(now).Seconds()))
	if retry < 1 {
		retry = 1
	}
	return false, retry, e.reset
}

func (l *rateLimiter) allow(key string) bool {
	ok, _, _ := l.allowWithInfo(key)
	return ok
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r) + ":" + r.URL.Path
		allowed, retry, reset := l.allowWithInfo(key)
		if !allowed {
			rid := r.Context().Value(requestIDKey)
			if rid == nil {
				rid = r.Header.Get("X-Request-ID")
			}
			if l.log != nil {
				l.log.Warn("rate limited", "request_id", rid, "account_id", r.Context().Value(accountKey), "endpoint", r.URL.Path, "bucket", l.bucket, "retry_after", retry, "reset", reset)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			respondError(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// endpointLimiter is account/session-aware for authenticated routes.
type endpointLimiter struct {
	entries map[string]rateEntry
	limit   int
	window  time.Duration
	bucket  string
	mu      sync.Mutex
	log     *slog.Logger
}

func newEndpointLimiter(limit int, window time.Duration, bucket string, log *slog.Logger) *endpointLimiter {
	if log == nil {
		log = slog.Default()
	}
	return &endpointLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, bucket: bucket, log: log}
}

func (l *endpointLimiter) allowWithInfo(key string) (bool, int, time.Time) {
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
		e = rateEntry{count: 0, reset: now.Add(l.window)}
	}
	e.count++
	l.entries[key] = e
	if e.count <= l.limit {
		return true, 0, time.Time{}
	}
	retry := int(math.Ceil(e.reset.Sub(now).Seconds()))
	if retry < 1 {
		retry = 1
	}
	return false, retry, e.reset
}

func (l *endpointLimiter) middleware(keyFunc func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFunc(r)
		allowed, retry, reset := l.allowWithInfo(key)
		if !allowed {
			rid := r.Context().Value(requestIDKey)
			if rid == nil {
				rid = r.Header.Get("X-Request-ID")
			}
			acct := r.Context().Value(accountKey)
			if l.log != nil {
				l.log.Warn("rate limited", "request_id", rid, "account_id", acct, "endpoint", r.URL.Path, "bucket", l.bucket, "retry_after", retry, "reset", reset)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			respondError(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func accountKeyFunc(bucket string) func(*http.Request) string {
	return func(r *http.Request) string {
		if acct, ok := r.Context().Value(accountKey).(string); ok && acct != "" {
			return acct + ":" + bucket
		}
		// Fallback to IP for unauthenticated (should not happen for authenticated routes)
		return clientIP(r) + ":" + bucket + ":" + r.URL.Path
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = generateRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		w.Header().Set("X-Request-ID", rid)
		next.ServeHTTP(w, r.WithContext(ctx))
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
	server.limiter = newSurrealRateLimiter(db, logger)
	server.pushStore = newPushStore(db, logger)
	server.pusher = initPusher(cfg, logger)
	mux := http.NewServeMux()

	otpLimiter := newRateLimiterWithBucket(5, 10*time.Minute, "otp", logger)
	verifyLimiter := newRateLimiterWithBucket(10, time.Minute, "verify", logger)

	// Endpoint-aware account limiters
	readLimiter := newEndpointLimiter(300, time.Minute, "read", logger)
	eventLimiter := newEndpointLimiter(120, time.Minute, "events", logger)

	// Health is not rate limited (or very high) to avoid exhausting reads
	mux.HandleFunc("GET /health", server.health)
	mux.Handle("POST /v1/auth/signup/request", otpLimiter.middleware(http.HandlerFunc(server.requestSignup)))
	mux.Handle("POST /v1/auth/signup/verify", verifyLimiter.middleware(http.HandlerFunc(server.verifySignup)))
	mux.Handle("POST /v1/auth/login/request", otpLimiter.middleware(http.HandlerFunc(server.requestLogin)))
	mux.Handle("POST /v1/auth/login/verify", verifyLimiter.middleware(http.HandlerFunc(server.verifyLogin)))
	mux.HandleFunc("POST /v1/auth/refresh", server.refresh)
	mux.HandleFunc("POST /v1/auth/logout", server.logout)

	mux.Handle("GET /v1/me", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.me))))
	mux.Handle("PATCH /v1/me", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.updateMe))))
	mux.Handle("GET /v1/me/privacy", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.getPrivacy))))
	mux.Handle("PATCH /v1/me/privacy", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.updatePrivacy))))
	mux.Handle("GET /v1/users/search", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.searchUsers))))
	mux.Handle("POST /v1/friends/requests", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.createFriendRequest))))
	mux.Handle("GET /v1/friends/requests", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.listFriendRequests))))
	mux.Handle("POST /v1/friends/requests/{id}/respond", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.respondFriendRequest))))
	mux.Handle("POST /v1/users/{id}/block", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.blockUser))))

	mux.Handle("GET /v1/conversations", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.listConversations))))
	mux.Handle("GET /v1/events/messages", server.requireAuth(eventLimiter.middleware(accountKeyFunc("events"), http.HandlerFunc(server.messageEvents))))
	mux.Handle("GET /v1/events/stream", server.requireAuth(eventLimiter.middleware(accountKeyFunc("events"), http.HandlerFunc(server.messageEventsStream))))
	mux.Handle("POST /v1/conversations/direct", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.createDirectConversation))))
	mux.Handle("GET /v1/conversations/{id}/messages", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.listMessages))))
	mux.Handle("POST /v1/conversations/{id}/messages", server.requireAuth(http.HandlerFunc(server.sendMessage)))
	mux.Handle("POST /v1/conversations/{id}/read", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.readConversation))))
	mux.Handle("POST /v1/conversations/{id}/delivered", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.deliverConversation))))
	mux.Handle("PUT /v1/messages/{id}/reactions", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.putReaction))))
	mux.Handle("DELETE /v1/messages/{id}/reactions/{emoji}", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.deleteReaction))))
	mux.Handle("PUT /v1/messages/{id}/saved", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.saveMessage))))
	mux.Handle("DELETE /v1/messages/{id}/saved", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.unsaveMessage))))
	mux.Handle("POST /v1/messages/{id}/receipt", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.updateReceipt))))

	mux.Handle("PUT /v1/me/push-devices", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.putPushDevice))))
	mux.Handle("DELETE /v1/me/push-devices/{deviceId}", server.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(server.deletePushDevice))))

	return recoverer(logger, securityHeaders(requestIDMiddleware(mux)))
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
