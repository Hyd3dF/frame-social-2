package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDBSlowDoesNotBlockSend(t *testing.T) {
	slow := &mockQueryer{
		latency: 200 * time.Millisecond,
		onQuery: func(sql string, vars map[string]any, dest any) error { return nil },
	}
	srv := &Server{
		db:      slow,
		events:  newMessageEventBroker(),
		log:     slogDiscard(),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	srv.persist = newPersister(slow, srv.pending, srv.members, slogDiscard())
	// No message rate limiter for this resilience check (isolated from 50/60s logic)
	srv.members.Set("conversation:slow", []string{"account:alice", "account:bob"})
	req := httptest.NewRequest("POST", "/v1/conversations/slow/messages", strings.NewReader(`{"body":"hi","clientId":"client-12345678"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "slow")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
	w := httptest.NewRecorder()
	start := time.Now()
	srv.sendMessage(w, req)
	elapsed := time.Since(start)
	if w.Code != 201 {
		t.Fatalf("expected 201 got %d body %s", w.Code, w.Body.String())
	}
	// The accepted message must return from the RAM fast path without waiting
	// for the deliberately slow database.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("sendMessage should not wait for 200ms DB latency, got %v", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(srv.pending.List("conversation:slow")) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.pending.List("conversation:slow")) != 0 {
		t.Fatalf("pending message was not cleared after background persistence")
	}
}

func TestDBUnavailableHealth503(t *testing.T) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error { return errSurrealUnavailable() },
	}
	srv := &Server{db: mock, events: newMessageEventBroker(), log: slogDiscard(), members: newMemberCache(), pending: newPendingStore()}
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.health(w, req)
	if w.Code != 503 {
		t.Fatalf("expected 503 got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "service_unavailable") {
		t.Fatal("expected service_unavailable code")
	}
}

func TestDBUnavailableListReturns500(t *testing.T) {
	mock := &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return errSurrealUnavailable() }}
	srv := &Server{db: mock, events: newMessageEventBroker(), log: slogDiscard(), members: newMemberCache(), pending: newPendingStore()}
	req := httptest.NewRequest("GET", "/v1/conversations", nil)
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
	w := httptest.NewRecorder()
	srv.listConversations(w, req)
	if w.Code != 500 {
		t.Fatalf("expected 500 got %d", w.Code)
	}
}

func TestPendingSurvivesForListMessages(t *testing.T) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error {
			if strings.Contains(sql, "FROM message") {
				if d, ok := dest.(*[]messageView); ok {
					*d = []messageView{}
				}
			}
			if strings.Contains(sql, "read_receipts_enabled") {
				if d, ok := dest.(*[]struct {
					Enabled bool `json:"enabled"`
				}); ok {
					*d = []struct {
						Enabled bool `json:"enabled"`
					}{{Enabled: true}}
				}
			}
			return nil
		},
	}
	srv := &Server{db: mock, events: newMessageEventBroker(), log: slogDiscard(), members: newMemberCache(), pending: newPendingStore()}
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	now := time.Now().UTC()
	view := messageView{ID: "message:pending1", ClientID: "client-abc", Conversation: "conversation:abc", SenderID: "account:alice", Body: strPtr("pending body"), CreatedAt: now.Format(time.RFC3339Nano), Status: "sent"}
	srv.pending.Append("conversation:abc", view)
	req := httptest.NewRequest("GET", "/v1/conversations/abc/messages?limit=10", nil)
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:bob"))
	w := httptest.NewRecorder()
	srv.listMessages(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 got %d %s", w.Code, w.Body.String())
	}
	// The recipient reads the accepted message directly from RAM while its
	// durable database write is still in flight.
	if !strings.Contains(w.Body.String(), "pending1") {
		t.Fatalf("pending message should be visible from RAM, body %s", w.Body.String())
	}
}

func errSurrealUnavailable() error { return &mockErr{s: "surreal returned HTTP 503"} }

type mockErr struct{ s string }

func (e *mockErr) Error() string { return e.s }
func strPtr(s string) *string    { return &s }
