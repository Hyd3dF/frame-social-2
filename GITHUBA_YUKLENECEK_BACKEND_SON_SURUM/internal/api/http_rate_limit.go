package api

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

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

func newRateLimiterWithBucket(limit int, window time.Duration, bucket string, logger *slog.Logger) *rateLimiter {
	if logger == nil {
		logger = slog.Default()
	}
	return &rateLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, bucket: bucket, log: logger}
}

func (l *rateLimiter) allowWithInfo(key string) (bool, int, time.Time) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 10000 {
		for key, entry := range l.entries {
			if entry.reset.Before(now) {
				delete(l.entries, key)
			}
		}
	}
	entry := l.entries[key]
	if entry.reset.Before(now) {
		entry = rateEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.entries[key] = entry
	if entry.count <= l.limit {
		return true, 0, time.Time{}
	}
	retry := int(math.Ceil(entry.reset.Sub(now).Seconds()))
	if retry < 1 {
		retry = 1
	}
	return false, retry, entry.reset
}

func (l *rateLimiter) allow(key string) bool {
	ok, _, _ := l.allowWithInfo(key)
	return ok
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retry, reset := l.allowWithInfo(clientIP(r) + ":" + r.URL.Path)
		if !allowed {
			l.log.Warn("rate limited", "request_id", r.Context().Value(requestIDKey), "account_id", r.Context().Value(accountKey), "endpoint", r.URL.Path, "bucket", l.bucket, "retry_after", retry, "reset", reset)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			respondError(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type endpointLimiter struct {
	entries map[string]rateEntry
	limit   int
	window  time.Duration
	bucket  string
	mu      sync.Mutex
	log     *slog.Logger
}

func newEndpointLimiter(limit int, window time.Duration, bucket string, logger *slog.Logger) *endpointLimiter {
	if logger == nil {
		logger = slog.Default()
	}
	return &endpointLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, bucket: bucket, log: logger}
}

func (l *endpointLimiter) allowWithInfo(key string) (bool, int, time.Time) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 10000 {
		for key, entry := range l.entries {
			if entry.reset.Before(now) {
				delete(l.entries, key)
			}
		}
	}
	entry := l.entries[key]
	if entry.reset.Before(now) {
		entry = rateEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.entries[key] = entry
	if entry.count <= l.limit {
		return true, 0, time.Time{}
	}
	retry := int(math.Ceil(entry.reset.Sub(now).Seconds()))
	if retry < 1 {
		retry = 1
	}
	return false, retry, entry.reset
}

func (l *endpointLimiter) middleware(keyFunc func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retry, reset := l.allowWithInfo(keyFunc(r))
		if !allowed {
			l.log.Warn("rate limited", "request_id", r.Context().Value(requestIDKey), "account_id", r.Context().Value(accountKey), "endpoint", r.URL.Path, "bucket", l.bucket, "retry_after", retry, "reset", reset)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			respondError(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func accountKeyFunc(bucket string) func(*http.Request) string {
	return func(r *http.Request) string {
		if account, ok := r.Context().Value(accountKey).(string); ok && account != "" {
			return account + ":" + bucket
		}
		return clientIP(r) + ":" + bucket + ":" + r.URL.Path
	}
}
