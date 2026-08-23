package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/httpx"
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
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{entries: make(map[string]rateEntry), limit: limit, window: window}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 10_000 {
		for candidate, value := range l.entries {
			if value.reset.Before(now) {
				delete(l.entries, candidate)
			}
		}
	}
	entry := l.entries[key]
	if entry.reset.Before(now) {
		entry = rateEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.entries[key] = entry
	return entry.count <= l.limit
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r) + ":" + r.URL.Path
		if !l.allow(key) {
			httpx.Error(w, http.StatusTooManyRequests, "rate_limited", "Çok fazla istek gönderildi. Lütfen biraz bekleyin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return r.RemoteAddr
}
