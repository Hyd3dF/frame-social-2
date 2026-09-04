package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockRateDB simulates SurrealDB for rate limiting.
type mockRateDB struct {
	mu       sync.Mutex
	states   map[string]*rateStateRow
	dedups   map[string]*dedupEntry
	messages map[string]bool // key: account + ":" + clientId
	now      func() time.Time
}

type dedupEntry struct {
	account   string
	clientId  string
	createdAt time.Time
	expiresAt time.Time
}

func newMockRateDB() *mockRateDB {
	return &mockRateDB{
		states:   make(map[string]*rateStateRow),
		dedups:   make(map[string]*dedupEntry),
		messages: make(map[string]bool),
		now:      time.Now,
	}
}

func (m *mockRateDB) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Handle DEFINE etc.
	if strings.Contains(sql, "DEFINE") {
		return nil
	}
	if strings.Contains(sql, "DELETE FROM message_dedup WHERE expires_at") {
		now := time.Now().UTC()
		if m.now != nil {
			now = m.now().UTC()
		}
		for k, v := range m.dedups {
			if v.expiresAt.Before(now) {
				delete(m.dedups, k)
			}
		}
		return nil
	}
	if strings.Contains(sql, "DELETE FROM message_rate_state WHERE") {
		now := time.Now().UTC()
		if m.now != nil {
			now = m.now().UTC()
		}
		for k, st := range m.states {
			blockedExpired := true
			if st.BlockedUntil != nil {
				if bt, err := parseSurrealTime(*st.BlockedUntil); err == nil && bt.After(now) {
					blockedExpired = false
				}
			}
			if blockedExpired && (st.Timestamps == nil || len(st.Timestamps) == 0) {
				delete(m.states, k)
			}
		}
		return nil
	}

	// Dedup check via type::record($dedupId)
	if strings.Contains(sql, "FROM type::record($dedupId)") && strings.Contains(sql, "SELECT") {
		dedupId, _ := vars["dedupId"].(string)
		if _, ok := m.dedups[dedupId]; ok {
			if dest != nil {
				// dest is *[]struct{ID string}
				if d, ok := dest.(*[]struct {
					ID string `json:"id"`
				}); ok {
					*d = []struct {
						ID string `json:"id"`
					}{{ID: dedupId}}
					return nil
				}
				// Generic handling via json
				b, _ := json.Marshal([]map[string]string{{"id": dedupId}})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		if dest != nil {
			// empty
			if d, ok := dest.(*[]struct {
				ID string `json:"id"`
			}); ok {
				*d = []struct {
					ID string `json:"id"`
				}{}
			} else {
				// try to set empty slice via reflection? Just marshal empty
				b, _ := json.Marshal([]map[string]string{})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// Message duplicate check
	if strings.Contains(sql, "FROM message WHERE sender = type::record") {
		account, _ := vars["account"].(string)
		clientId, _ := vars["clientId"].(string)
		key := account + ":" + clientId
		if m.messages[key] {
			if dest != nil {
				if d, ok := dest.(*[]recordID); ok {
					*d = []recordID{{ID: "message:dummy"}}
					return nil
				}
				b, _ := json.Marshal([]map[string]string{{"id": "message:dummy"}})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		if dest != nil {
			if d, ok := dest.(*[]recordID); ok {
				*d = []recordID{}
			} else {
				b, _ := json.Marshal([]map[string]string{})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// CREATE dedup
	if strings.Contains(sql, "CREATE") && strings.Contains(sql, "type::record($dedupId)") && strings.Contains(sql, "client_id") {
		dedupId, _ := vars["dedupId"].(string)
		if _, exists := m.dedups[dedupId]; exists {
			return fmt.Errorf("record already exists")
		}
		account, _ := vars["account"].(string)
		clientId, _ := vars["clientId"].(string)
		nowStr, _ := vars["now"].(string)
		expStr, _ := vars["exp"].(string)
		nowT, _ := parseSurrealTime(nowStr)
		expT, _ := parseSurrealTime(expStr)
		m.dedups[dedupId] = &dedupEntry{account: account, clientId: clientId, createdAt: nowT, expiresAt: expT}
		// Also mark message as existing for future dedup via message table? Not needed, dedup suffices.
		return nil
	}

	// SELECT rate state
	if strings.Contains(sql, "FROM type::record($id)") && strings.Contains(sql, "SELECT") && strings.Contains(sql, "blocked_until") {
		id, _ := vars["id"].(string)
		st, ok := m.states[id]
		if !ok {
			if dest != nil {
				if d, ok := dest.(*[]rateStateRow); ok {
					*d = []rateStateRow{}
				} else {
					b, _ := json.Marshal([]rateStateRow{})
					_ = json.Unmarshal(b, dest)
				}
			}
			return nil
		}
		if dest != nil {
			// Copy
			copySt := *st
			if d, ok := dest.(*[]rateStateRow); ok {
				*d = []rateStateRow{copySt}
			} else {
				b, _ := json.Marshal([]rateStateRow{copySt})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// CREATE rate state (both allowed and blocked)
	if strings.Contains(sql, "CREATE") && strings.Contains(sql, "type::record($id)") && strings.Contains(sql, "CONTENT") {
		// Distinguish from dedup: dedup uses $dedupId, this uses $id
		if _, hasDedupId := vars["dedupId"]; hasDedupId {
			// This is dedup, already handled above, skip
		} else {
			id, _ := vars["id"].(string)
			if _, exists := m.states[id]; exists {
				return fmt.Errorf("record already exists")
			}
			account, _ := vars["account"].(string)
			timestampsRaw := vars["timestamps"]
			var timestamps []string
			switch v := timestampsRaw.(type) {
			case []string:
				timestamps = v
			case []interface{}:
				for _, x := range v {
					if s, ok := x.(string); ok {
						timestamps = append(timestamps, s)
					}
				}
			}
			var blockedPtr *string
			if v, ok := vars["blockedUntil"]; ok {
				if s, ok := v.(string); ok && s != "" {
					blockedPtr = &s
				}
			} else if strings.Contains(sql, "blocked_until: NONE") {
				blockedPtr = nil
			}
			// If sql contains blocked_until: NONE, ensure nil
			if strings.Contains(sql, "blocked_until: NONE") {
				blockedPtr = nil
			}
			m.states[id] = &rateStateRow{ID: id, Account: account, Timestamps: timestamps, BlockedUntil: blockedPtr, Version: 1}
			return nil
		}
	}

	// UPDATE rate state (both blocked and allowed)
	if strings.Contains(sql, "UPDATE type::record($id) SET") {
		id, _ := vars["id"].(string)
		st, ok := m.states[id]
		if !ok {
			if dest != nil {
				if d, ok := dest.(*[]rateStateRow); ok {
					*d = []rateStateRow{}
				}
			}
			return nil
		}
		var oldVersion int
		if v, ok := vars["oldVersion"]; ok {
			switch x := v.(type) {
			case int:
				oldVersion = x
			case int64:
				oldVersion = int(x)
			case float64:
				oldVersion = int(x)
			}
		}
		if st.Version != oldVersion {
			if dest != nil {
				if d, ok := dest.(*[]rateStateRow); ok {
					*d = []rateStateRow{}
				}
			}
			return nil
		}
		timestampsRaw := vars["timestamps"]
		var timestamps []string
		switch v := timestampsRaw.(type) {
		case []string:
			timestamps = v
		case []interface{}:
			for _, x := range v {
				if s, ok := x.(string); ok {
					timestamps = append(timestamps, s)
				}
			}
		}
		var newVersion int
		if v, ok := vars["newVersion"]; ok {
			switch x := v.(type) {
			case int:
				newVersion = x
			case int64:
				newVersion = int(x)
			case float64:
				newVersion = int(x)
			}
		}
		// Determine blocked_until
		if strings.Contains(sql, "blocked_until = NONE") {
			st.BlockedUntil = nil
		} else if v, ok := vars["blockedUntil"]; ok {
			if s, ok := v.(string); ok {
				b := s
				st.BlockedUntil = &b
			}
		}
		st.Timestamps = timestamps
		st.Version = newVersion
		if dest != nil {
			if d, ok := dest.(*[]rateStateRow); ok {
				*d = []rateStateRow{*st}
			} else {
				b, _ := json.Marshal([]rateStateRow{*st})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// DELETE type::record
	if strings.Contains(sql, "DELETE type::record($id)") {
		id, _ := vars["id"].(string)
		delete(m.states, id)
		return nil
	}

	// Fallback: for any other query, return nil
	return nil
}

func (m *mockRateDB) Ping(ctx context.Context) error { return nil }

// Test helpers

func TestRateLimitFirst50Allowed(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:testuser1"
	for i := 0; i < 50; i++ {
		allowed, dup, _, _, err := limiter.Check(context.Background(), account, fmt.Sprintf("client-%d", i))
		if err != nil {
			t.Fatalf("check %d failed: %v", i, err)
		}
		if dup {
			t.Fatalf("unexpected dup at %d", i)
		}
		if !allowed {
			t.Fatalf("expected allowed at %d", i)
		}
	}
}

func TestRateLimit51stTriggersPenalty(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:testuser2"
	for i := 0; i < 50; i++ {
		allowed, _, _, _, _ := limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
		if !allowed {
			t.Fatalf("pre-fill %d not allowed", i)
		}
	}
	allowed, dup, retry, blockedUntil, err := limiter.Check(context.Background(), account, "client-51")
	if err != nil {
		t.Fatalf("51st check error %v", err)
	}
	if dup {
		t.Fatal("51st should not be dup")
	}
	if allowed {
		t.Fatal("51st should be blocked")
	}
	if retry != 300 {
		t.Fatalf("expected retry 300 got %d", retry)
	}
	if blockedUntil.IsZero() {
		t.Fatal("blockedUntil zero")
	}
	// Subsequent during penalty should be blocked
	for i := 0; i < 5; i++ {
		allowed, _, retry2, _, _ := limiter.Check(context.Background(), account, fmt.Sprintf("c-penalty-%d", i))
		if allowed {
			t.Fatalf("penalty %d should be blocked", i)
		}
		if retry2 <= 0 || retry2 > 300 {
			t.Fatalf("retry2 invalid %d", retry2)
		}
	}
}

func TestRateLimitPenaltyExpires(t *testing.T) {
	db := newMockRateDB()
	now := time.Now().UTC()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
	account := "account:testuser3"
	for i := 0; i < 50; i++ {
		limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
	}
	allowed, _, _, _, _ := limiter.Check(context.Background(), account, "c-51")
	if allowed {
		t.Fatal("should be blocked")
	}
	// Advance 5 minutes +1 sec
	now = now.Add(5*time.Minute + time.Second)
	limiter.now = func() time.Time { return now }
	allowed, _, _, _, err := limiter.Check(context.Background(), account, "c-after")
	if err != nil {
		t.Fatalf("after penalty error %v", err)
	}
	if !allowed {
		t.Fatal("should be allowed after penalty")
	}
}

func TestRateLimitIdempotentSameClientId(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:testuser4"
	allowed, dup, _, _, _ := limiter.Check(context.Background(), account, "client-same")
	if !allowed || dup {
		t.Fatalf("first should be allowed not dup")
	}
	// Retry same clientId should be dup and not count
	allowed, dup, _, _, _ = limiter.Check(context.Background(), account, "client-same")
	if !allowed || !dup {
		t.Fatalf("second should be dup allowed, got allowed=%v dup=%v", allowed, dup)
	}
	// Fill up to 49 more distinct, should still allow 49 (since dup not counted)
	for i := 0; i < 49; i++ {
		allowed, dup, _, _, _ := limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
		if !allowed || dup {
			t.Fatalf("fill %d failed", i)
		}
	}
	// Now we have 50 counted (1 original +49), next distinct should trigger penalty
	allowed, dup, _, _, _ = limiter.Check(context.Background(), account, "new-client-51")
	if allowed {
		t.Fatal("should be rate limited")
	}
	if dup {
		t.Fatal("should not be dup")
	}
}

func TestRateLimitParallelNotExceed(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:parallelUser"
	var wg sync.WaitGroup
	results := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			allowed, _, _, _, _ := limiter.Check(context.Background(), account, fmt.Sprintf("client-p-%d", i))
			results <- allowed
		}(i)
	}
	wg.Wait()
	close(results)
	allowedCount := 0
	for r := range results {
		if r {
			allowedCount++
		}
	}
	if allowedCount != 50 {
		t.Fatalf("expected 50 allowed, got %d", allowedCount)
	}
	// Next should be blocked
	allowed, _, _, _, _ := limiter.Check(context.Background(), account, "extra")
	if allowed {
		t.Fatal("extra should be blocked")
	}
}

func TestRateLimitIsolation(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a1 := "account:userA"
	a2 := "account:userB"
	for i := 0; i < 50; i++ {
		allowed, _, _, _, _ := limiter.Check(context.Background(), a1, fmt.Sprintf("a1-%d", i))
		if !allowed {
			t.Fatalf("a1 %d not allowed", i)
		}
	}
	// a1 next should block
	allowed, _, _, _, _ := limiter.Check(context.Background(), a1, "a1-51")
	if allowed {
		t.Fatal("a1 51 should block")
	}
	// a2 should still allow 50
	for i := 0; i < 50; i++ {
		allowed, _, _, _, _ := limiter.Check(context.Background(), a2, fmt.Sprintf("a2-%d", i))
		if !allowed {
			t.Fatalf("a2 %d not allowed", i)
		}
	}
}

func TestRateLimitNewTokenDoesNotBypass(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:sameUser"
	for i := 0; i < 50; i++ {
		limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
	}
	allowed, _, _, _, _ := limiter.Check(context.Background(), account, "c-51")
	if allowed {
		t.Fatal("should block")
	}
	// Simulate new token: same account ID, different request context
	allowed, _, _, _, _ = limiter.Check(context.Background(), account, "c-new-token")
	if allowed {
		t.Fatal("new token should still be blocked for same account")
	}
}

func TestRateLimitSurvivesRestart(t *testing.T) {
	db := newMockRateDB()
	limiter1 := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	account := "account:restartUser"
	for i := 0; i < 50; i++ {
		limiter1.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
	}
	limiter1.Check(context.Background(), account, "c-51")
	// Simulate restart: new limiter instance with same DB
	limiter2 := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	allowed, _, _, _, _ := limiter2.Check(context.Background(), account, "c-after-restart")
	if allowed {
		t.Fatal("after restart should still be blocked")
	}
}

func TestRateLimitDBUnavailableSafe(t *testing.T) {
	// DB that returns error
	unavailable := &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error {
		return fmt.Errorf("surreal returned HTTP 503")
	}}
	limiter := &surrealRateLimiter{db: unavailable, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, _, _, err := limiter.Check(context.Background(), "account:x", "client-1")
	if err == nil {
		t.Fatal("expected error")
	}
	// Server should return 503, not allow
	srv := &Server{
		db:      unavailable,
		limiter: limiter,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	srv.members.Set("conversation:abc", []string{"account:x", "account:y"})
	// Need to bypass pending etc., but we test sendMessage returns 503
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"hi","clientId":"client-12345678"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:x"))
	w := httptest.NewRecorder()
	srv.sendMessage(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d body %s", w.Code, w.Body.String())
	}
}

func TestRateLimitInvalidNotCounted(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := &Server{
		db:      &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }},
		limiter: limiter,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		members: newMemberCache(),
		pending: newPendingStore(),
		events:  newMessageEventBroker(),
	}
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	srv.persist = newPersister(srv.db, srv.pending, srv.members, srv.log)
	// Invalid body (empty) should return 400 and not count
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"","clientId":"client-12345678"}`))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "abc")
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
		w := httptest.NewRecorder()
		srv.sendMessage(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 got %d", w.Code)
		}
	}
	// Valid next should still be allowed (since invalid didn't count)
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"valid","clientId":"client-valid123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
	w := httptest.NewRecorder()
	srv.sendMessage(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d %s", w.Code, w.Body.String())
	}
}

func TestSendMessageRateLimitedResponse(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := &Server{
		db:      db,
		limiter: limiter,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		members: newMemberCache(),
		pending: newPendingStore(),
		events:  newMessageEventBroker(),
	}
	// Need mock for members: use the same db for members? But our mockRateDB doesn't handle conversation_member queries.
	// Instead, set cache so no DB query for members.
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	srv.persist = newPersister(&mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }}, srv.pending, srv.members, srv.log)

	// Fill 50
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(fmt.Sprintf(`{"body":"hi %d","clientId":"client-%08d"}`, i, i)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "abc")
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
		w := httptest.NewRecorder()
		srv.sendMessage(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("fill %d expected 201 got %d %s", i, w.Code, w.Body.String())
		}
	}
	// 51st
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"hi 51","clientId":"client-99999999"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
	w := httptest.NewRecorder()
	srv.sendMessage(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 got %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error obj %v", body)
	}
	if errObj["code"] != "message_rate_limited" {
		t.Fatalf("code %v", errObj["code"])
	}
	if _, ok := errObj["retryAfterSeconds"]; !ok {
		t.Fatal("missing retryAfterSeconds")
	}
	if _, ok := errObj["blockedUntil"]; !ok {
		t.Fatal("missing blockedUntil")
	}
	// Check that message was not persisted beyond 50 (pending is async and may have been cleared, so we check status code instead)
	// The 51st was blocked, so we ensure not 201
	if w.Code == http.StatusCreated {
		t.Fatal("51st should not be 201")
	}
}

func TestRateLimitSlidingWindowExpiry(t *testing.T) {
	db := newMockRateDB()
	now := time.Now().UTC()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
	account := "account:slidingUser"
	// Send 50 at T0
	for i := 0; i < 50; i++ {
		allowed, _, _, _, _ := limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
		if !allowed {
			t.Fatalf("initial %d not allowed", i)
		}
	}
	// Advance 61 seconds, window should slide and allow another 50 without penalty
	now = now.Add(61 * time.Second)
	limiter.now = func() time.Time { return now }
	for i := 0; i < 50; i++ {
		allowed, _, _, _, err := limiter.Check(context.Background(), account, fmt.Sprintf("c2-%d", i))
		if err != nil {
			t.Fatalf("sliding %d error %v", i, err)
		}
		if !allowed {
			t.Fatalf("sliding %d should be allowed", i)
		}
	}
	// 51st in new window should block
	allowed, _, _, _, _ := limiter.Check(context.Background(), account, "c2-51")
	if allowed {
		t.Fatal("should be blocked in new window")
	}
}

func TestRateLimitRetryAfterDynamic(t *testing.T) {
	db := newMockRateDB()
	now := time.Now().UTC()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
	account := "account:dynamicUser"
	for i := 0; i < 50; i++ {
		limiter.Check(context.Background(), account, fmt.Sprintf("c-%d", i))
	}
	allowed, _, retry1, blocked1, _ := limiter.Check(context.Background(), account, "c-51")
	if allowed || retry1 != 300 {
		t.Fatalf("first block retry %d", retry1)
	}
	// Advance 2 minutes
	now = now.Add(2 * time.Minute)
	limiter.now = func() time.Time { return now }
	allowed, _, retry2, blocked2, _ := limiter.Check(context.Background(), account, "c-penalty2")
	if allowed {
		t.Fatal("should still be blocked")
	}
	if retry2 >= retry1 {
		t.Fatalf("retry2 %d should be less than retry1 %d", retry2, retry1)
	}
	if !blocked1.Equal(blocked2) {
		t.Fatalf("blockedUntil should stay same %v vs %v", blocked1, blocked2)
	}
	// Check Retry-After header via HTTP
	srv := &Server{
		db:      db,
		limiter: limiter,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		members: newMemberCache(),
		pending: newPendingStore(),
		events:  newMessageEventBroker(),
	}
	srv.members.Set("conversation:abc", []string{account, "account:bob"})
	srv.persist = newPersister(&mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }}, srv.pending, srv.members, srv.log)
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"hi","clientId":"client-retry-test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, account))
	w := httptest.NewRecorder()
	srv.sendMessage(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 got %d", w.Code)
	}
	ra, _ := strconv.Atoi(w.Header().Get("Retry-After"))
	if ra != retry2 {
		t.Fatalf("Retry-After header %d != retry2 %d", ra, retry2)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	errObj := body["error"].(map[string]any)
	if int(errObj["retryAfterSeconds"].(float64)) != retry2 {
		t.Fatalf("body retry %v != %d", errObj["retryAfterSeconds"], retry2)
	}
}

func TestRateLimitPenaltyDoesNotReachQueue(t *testing.T) {
	db := newMockRateDB()
	limiter := &surrealRateLimiter{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := &Server{
		db:      db,
		limiter: limiter,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		members: newMemberCache(),
		pending: newPendingStore(),
		events:  newMessageEventBroker(),
	}
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	// Track persist enqueue calls via custom persister? Use mock that counts
	var enqueueCount int
	mockPersistDB := &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }}
	persist := newPersister(mockPersistDB, srv.pending, srv.members, srv.log)
	// Wrap enqueue to count (we can't easily, so check pending length after)
	srv.persist = persist
	// Fill 50
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(fmt.Sprintf(`{"body":"hi %d","clientId":"client-%08d"}`, i, i)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "abc")
		req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
		w := httptest.NewRecorder()
		srv.sendMessage(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("fill %d 201 got %d", i, w.Code)
		}
		enqueueCount++
	}
	// Wait a bit for persist to process and clear pending
	time.Sleep(50 * time.Millisecond)
	initialPending := len(srv.pending.List("conversation:abc"))
	// 51st should be blocked and not enqueue
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"blocked","clientId":"client-blocked123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), accountKey, "account:alice"))
	w := httptest.NewRecorder()
	srv.sendMessage(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 got %d", w.Code)
	}
	// Check that no new pending was added beyond initial (allow race)
	time.Sleep(20 * time.Millisecond)
	afterPending := len(srv.pending.List("conversation:abc"))
	if afterPending > initialPending {
		t.Fatalf("blocked message should not reach pending queue, before %d after %d", initialPending, afterPending)
	}
	_ = enqueueCount
}
