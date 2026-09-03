package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

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
		if _, deleted := s.deletedAccounts.Load(accountID); deleted && !(r.Method == http.MethodDelete && r.URL.Path == "/v1/me") {
			respondError(w, http.StatusUnauthorized, "invalid_token", "Oturum süresi dolmuş veya geçersiz.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey, accountID)))
	})
}

func accountID(r *http.Request) string { return r.Context().Value(accountKey).(string) }

func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if !validRequestID(requestID) {
			requestID = generateRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func clientIP(r *http.Request) string {
	if value := r.Header.Get("CF-Connecting-IP"); value != "" {
		return strings.TrimSpace(value)
	}
	host := r.RemoteAddr
	if index := strings.LastIndex(host, ":"); index != -1 {
		host = host[:index]
	}
	host = strings.Trim(host, "[]")
	if host != "" {
		return host
	}
	return r.RemoteAddr
}

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
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
