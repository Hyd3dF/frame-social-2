package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/Hyd3dF/frame-social-2/internal/config"
	"google.golang.org/api/option"
)

// Pusher abstracts FCM sending and handles invalid token detection.
type Pusher interface {
	Send(ctx context.Context, tokens []string, title, body string, data map[string]string) (invalidTokens []string, err error)
}

// noopPusher is used when Firebase is not configured (tests, local dev without credentials).
type noopPusher struct {
	log *slog.Logger
}

func (n *noopPusher) Send(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
	if n.log != nil {
		n.log.Debug("push disabled: would send", "tokens", len(tokens), "title", title)
	}
	return nil, nil
}

// mockPusher for tests allows custom behavior.
type mockPusher struct {
	mu            sync.Mutex
	calls         []mockPushCall
	sendFunc      func(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error)
	invalidTokens []string
	err           error
}

type mockPushCall struct {
	Tokens []string
	Title  string
	Body   string
	Data   map[string]string
}

func (m *mockPusher) Send(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockPushCall{Tokens: append([]string(nil), tokens...), Title: title, Body: body, Data: data})
	m.mu.Unlock()
	if m.sendFunc != nil {
		return m.sendFunc(ctx, tokens, title, body, data)
	}
	return m.invalidTokens, m.err
}

func (m *mockPusher) Calls() []mockPushCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockPushCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// firebasePusher uses Firebase Admin SDK.
type firebasePusher struct {
	client *messaging.Client
	log    *slog.Logger
}

func newFirebasePusher(ctx context.Context, credsJSON []byte, projectID string, log *slog.Logger) (Pusher, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(credsJSON) == 0 {
		return &noopPusher{log: log}, nil
	}
	// If projectID provided and not in JSON, inject?
	cfg := &firebase.Config{}
	if projectID != "" {
		cfg.ProjectID = projectID
	}
	// Validate JSON contains project_id if not provided via config
	var credMap map[string]any
	if err := json.Unmarshal(credsJSON, &credMap); err != nil {
		// Could be base64 encoded already handled earlier, try base64 decode
		decoded, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(string(credsJSON)))
		if err2 == nil {
			if err3 := json.Unmarshal(decoded, &credMap); err3 == nil {
				credsJSON = decoded
			} else {
				return nil, fmt.Errorf("invalid firebase credentials JSON: %w", err)
			}
		} else {
			return nil, fmt.Errorf("invalid firebase credentials JSON: %w", err)
		}
	}
	if projectID == "" {
		if pid, ok := credMap["project_id"].(string); ok && pid != "" {
			cfg.ProjectID = pid
		}
	}
	opt := option.WithCredentialsJSON(credsJSON)
	app, err := firebase.NewApp(ctx, cfg, opt)
	if err != nil {
		return nil, fmt.Errorf("firebase.NewApp: %w", err)
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase messaging: %w", err)
	}
	log.Info("firebase push enabled", "project", cfg.ProjectID)
	return &firebasePusher{client: client, log: log}, nil
}

func (f *firebasePusher) Send(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	// Firebase multicast limit is 500 tokens per request
	const batchSize = 500
	var allInvalid []string
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		batch := tokens[i:end]
		invalid, err := f.sendBatch(ctx, batch, title, body, data)
		if err != nil {
			// Log but continue with next batches; collect invalid anyway
			if f.log != nil {
				f.log.Error("fcm batch send failed", "error", err, "batchSize", len(batch))
			}
			// Don't return whole error otherwise caller would not clean invalid; we return error but also invalid
			// For transient errors (unavailable), we shouldn't delete tokens, so only return invalid if error is not transport
			allInvalid = append(allInvalid, invalid...)
			continue
		}
		allInvalid = append(allInvalid, invalid...)
	}
	return allInvalid, nil
}

func (f *firebasePusher) sendBatch(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
	msg := &messaging.MulticastMessage{
		Tokens: tokens,
		Notification: &messaging.Notification{
			Title: title,
			Body:  body,
		},
		Data: data,
		// Android/iOS specific could be added: high priority, etc.
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Headers: map[string]string{"apns-priority": "10"},
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{Sound: "default"},
			},
		},
	}
	// Use SendEachForMulticast via context with timeout
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := f.client.SendEachForMulticast(ctx2, msg)
	if err != nil {
		return nil, err
	}
	var invalid []string
	if resp.FailureCount == 0 {
		return nil, nil
	}
	for idx, r := range resp.Responses {
		if !r.Success {
			tok := tokens[idx]
			if r.Error != nil && isInvalidTokenError(r.Error) {
				invalid = append(invalid, tok)
				if f.log != nil {
					f.log.Warn("fcm invalid token", "token", maskToken(tok), "error", r.Error.Error())
				}
			} else {
				if f.log != nil {
					f.log.Warn("fcm send failure", "token", maskToken(tok), "error", r.Error)
				}
			}
		}
	}
	return invalid, nil
}

func isInvalidTokenError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Firebase errors contain codes like: registration-token-not-registered, invalid-registration-token, invalid-argument
	return strings.Contains(s, "registration-token-not-registered") ||
		strings.Contains(s, "invalid-registration-token") ||
		strings.Contains(s, "notregistered") ||
		strings.Contains(s, "invalid-argument") && strings.Contains(s, "token") ||
		strings.Contains(s, "not-registered") ||
		strings.Contains(s, "invalid-registration")
}

func maskToken(tok string) string {
	if len(tok) <= 8 {
		return "***"
	}
	return tok[:4] + "..." + tok[len(tok)-4:]
}

// Send push helper for server

func (s *Server) triggerPushForMessage(senderID, conversationID, messageID string, members []string) {
	if s.pusher == nil || s.pushStore == nil {
		return
	}
	// Don't block caller; run async with background context
	go s.sendPushForMessage(context.Background(), senderID, conversationID, messageID, members)
}

func (s *Server) sendPushForMessage(ctx context.Context, senderID, conversationID, messageID string, members []string) {
	// Derive recipients = members except sender
	var recipients []string
	for _, m := range members {
		if m != senderID {
			recipients = append(recipients, m)
		}
	}
	if len(recipients) == 0 {
		return
	}
	// Fetch sender display name for title
	displayName := s.fetchSenderDisplayName(ctx, senderID)
	if displayName == "" {
		displayName = "Yeni mesaj"
	}
	body := "Yeni bir mesajın var"
	data := map[string]string{
		"type":           "new_message",
		"conversationId": conversationID,
		"messageId":      messageID,
		"senderId":       senderID,
	}
	// Add timeout to overall push operation
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for _, recipient := range recipients {
		devices, err := s.pushStore.ListByAccount(ctx, recipient)
		if err != nil {
			if s.log != nil {
				s.log.Error("push: list devices failed", "recipient", recipient, "error", err)
			}
			continue
		}
		if len(devices) == 0 {
			continue
		}
		var tokens []string
		for _, d := range devices {
			if d.Token != "" {
				tokens = append(tokens, d.Token)
			}
		}
		if len(tokens) == 0 {
			continue
		}
		invalid, err := s.pusher.Send(ctx, tokens, displayName, body, data)
		if err != nil {
			if s.log != nil {
				s.log.Error("push: send failed", "recipient", recipient, "sender", senderID, "conversation", conversationID, "error", err)
			}
			// still attempt to clean invalid if any
		}
		if len(invalid) > 0 {
			if err := s.pushStore.DeleteByTokens(ctx, invalid); err != nil {
				if s.log != nil {
					s.log.Error("push: delete invalid tokens failed", "recipient", recipient, "error", err, "invalidCount", len(invalid))
				}
			} else if s.log != nil {
				s.log.Info("push: deleted invalid tokens", "recipient", recipient, "count", len(invalid))
			}
		}
		if err == nil && s.log != nil {
			s.log.Info("push: sent", "recipient", recipient, "tokens", len(tokens), "invalid", len(invalid), "conversation", conversationID, "message", messageID)
		}
	}
}

func (s *Server) fetchSenderDisplayName(ctx context.Context, senderID string) string {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var result []struct {
		DisplayName string `json:"displayName"`
	}
	err := s.db.Query(cctx, `SELECT display_name AS displayName FROM type::record($account) LIMIT 1`, map[string]any{"account": senderID}, &result)
	if err != nil {
		if s.log != nil {
			s.log.Error("push: fetch sender displayName failed", "sender", senderID, "error", err)
		}
		return ""
	}
	if len(result) > 0 && strings.TrimSpace(result[0].DisplayName) != "" {
		return result[0].DisplayName
	}
	return ""
}

func initPusher(cfg config.Config, log *slog.Logger) Pusher {
	if log == nil {
		log = slog.Default()
	}
	if !cfg.FirebaseEnabled() {
		log.Info("firebase disabled: no credentials configured")
		return &noopPusher{log: log}
	}
	creds, err := cfg.FirebaseCredentials()
	if err != nil {
		log.Error("firebase credentials decode failed", "error", err)
		return &noopPusher{log: log}
	}
	if len(creds) == 0 {
		log.Info("firebase disabled: empty credentials")
		return &noopPusher{log: log}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := newFirebasePusher(ctx, creds, cfg.FirebaseProjectID, log)
	if err != nil {
		log.Error("firebase init failed, push disabled", "error", err)
		return &noopPusher{log: log}
	}
	return p
}
