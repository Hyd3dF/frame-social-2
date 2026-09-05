package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

// integrationMockDB handles conversations, messages, auth_sessions for determinism tests.
type integrationMockDB struct {
	mu            sync.Mutex
	members       map[string][]string         // conversationID -> accountIDs
	messages      map[string][]messageView    // conversationID -> messages
	conversations map[string]conversationView // not heavily used
	accounts      map[string]accountAuth
	sessions      map[string]string // refreshHash -> accountID
}

func newIntegrationMockDB() *integrationMockDB {
	return &integrationMockDB{
		members:       make(map[string][]string),
		messages:      make(map[string][]messageView),
		conversations: make(map[string]conversationView),
		accounts:      make(map[string]accountAuth),
		sessions:      make(map[string]string),
	}
}

func (m *integrationMockDB) Ping(ctx context.Context) error { return nil }

func (m *integrationMockDB) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Health ping
	if strings.Contains(sql, "RETURN {ok:true}") {
		if dest != nil {
			_ = json.Unmarshal([]byte(`{"ok":true}`), dest)
		}
		return nil
	}

	// For determinism tests, handle key queries:

	// GET /v1/me - SELECT FROM type::record($account) WHERE status = 'active'
	if strings.Contains(sql, "FROM type::record($account) WHERE status") {
		account, _ := vars["account"].(string)
		if acc, ok := m.accounts[account]; ok {
			if dest != nil {
				b, _ := json.Marshal([]accountAuth{acc})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		// Return minimal account if not in map, for any account
		if dest != nil {
			b, _ := json.Marshal([]accountAuth{{ID: account, Username: "test", DisplayName: "Test", FullName: "Test User", Phone: "+9000000000", CountryCode: "TR"}})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// getMembersCached - SELECT in FROM conversation_member WHERE out = type::record($conversation)
	if strings.Contains(sql, "FROM conversation_member WHERE out = type::record($conversation)") && strings.Contains(sql, "SELECT <string>in AS id") {
		conv, _ := vars["conversation"].(string)
		members := m.members[conv]
		if dest != nil {
			var ids []recordID
			for _, mem := range members {
				ids = append(ids, recordID{ID: mem})
			}
			b, _ := json.Marshal(ids)
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// listConversations - FROM conversation_member WHERE in = type::record($account)
	if strings.Contains(sql, "FROM conversation_member WHERE in = type::record($account) AND left_at IS NONE") && strings.Contains(sql, "SELECT <string>out.id") {
		account, _ := vars["account"].(string)
		var views []conversationView
		for convID, members := range m.members {
			for _, mem := range members {
				if mem == account {
					// Found conversation where account is member
					other := "account:other"
					for _, o := range members {
						if o != account {
							other = o
							break
						}
					}
					views = append(views, conversationView{
						ID:          convID,
						UpdatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
						OtherMember: userView{ID: other, Username: "other", DisplayName: "Other", FullName: "Other User"},
						UnreadCount: 0,
					})
					break
				}
			}
		}
		if dest != nil {
			b, _ := json.Marshal(views)
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// listMessages - SELECT ... FROM message WHERE conversation = type::record($conversation)
	if strings.Contains(sql, "FROM message WHERE conversation = type::record($conversation)") {
		conv, _ := vars["conversation"].(string)
		limit, _ := vars["limit"].(int)
		beforeStr, _ := vars["before"].(string)
		var before time.Time
		if beforeStr != "" {
			before, _ = time.Parse(time.RFC3339Nano, beforeStr)
		} else {
			before = time.Now().UTC().Add(time.Hour)
		}
		account, _ := vars["account"].(string)
		all := m.messages[conv]
		// Filter by before and order DESC, limit
		var filtered []messageView
		for _, msg := range all {
			t, _ := time.Parse(time.RFC3339Nano, msg.CreatedAt)
			if t.Before(before) {
				// Check saved, status, etc. are not needed for determinism, but we should set canonical fields
				// Ensure canonical IDs
				if !strings.HasPrefix(msg.ID, "message:") {
					continue
				}
				if !strings.HasPrefix(msg.Conversation, "conversation:") {
					continue
				}
				if !strings.HasPrefix(msg.SenderID, "account:") {
					continue
				}
				// Ensure createdAt is valid RFC3339
				if _, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err != nil {
					if _, err2 := time.Parse(time.RFC3339, msg.CreatedAt); err2 != nil {
						continue
					}
				}
				// For privacy, we need to handle status? Keep as is
				filtered = append(filtered, msg)
			}
		}
		// Sort DESC by CreatedAt (already should be, but sort)
		// Use simple sort: latest first
		for i := 0; i < len(filtered); i++ {
			for j := i + 1; j < len(filtered); j++ {
				ti, _ := time.Parse(time.RFC3339Nano, filtered[i].CreatedAt)
				tj, _ := time.Parse(time.RFC3339Nano, filtered[j].CreatedAt)
				if tj.After(ti) {
					filtered[i], filtered[j] = filtered[j], filtered[i]
				}
			}
		}
		if limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}
		if dest != nil {
			// dest is *[]messageView
			if d, ok := dest.(*[]messageView); ok {
				*d = filtered
			} else {
				b, _ := json.Marshal(filtered)
				_ = json.Unmarshal(b, dest)
			}
		}
		_ = account
		return nil
	}

	// read_receipts_enabled
	if strings.Contains(sql, "read_receipts_enabled") {
		if dest != nil {
			b, _ := json.Marshal([]struct {
				Enabled bool `json:"enabled"`
			}{{Enabled: true}})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// doPersist for sendMessage - CREATE $mid CONTENT with conversation, sender, client_id
	if strings.Contains(sql, "CREATE ONLY type::record($mid) CONTENT") && strings.Contains(sql, "conversation: type::record") {
		mid, _ := vars["mid"].(string)
		conv, _ := vars["conversation"].(string)
		sender, _ := vars["sender"].(string)
		clientID, _ := vars["client_id"].(string)
		body, _ := vars["body"].(string)
		createdAt, _ := vars["createdAt"].(string)
		// Validate canonical IDs
		if !strings.HasPrefix(mid, "message:") || !strings.HasPrefix(conv, "conversation:") || !strings.HasPrefix(sender, "account:") {
			return fmt.Errorf("invalid canonical ID")
		}
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return fmt.Errorf("invalid createdAt")
		}
		msg := messageView{
			ID:           mid,
			ClientID:     clientID,
			Conversation: conv,
			SenderID:     sender,
			Body:         &body,
			Kind:         "text",
			CreatedAt:    createdAt,
			Status:       "sent",
			Reactions:    []reactionView{},
		}
		m.messages[conv] = append(m.messages[conv], msg)
		// Also update last_message for conversation (not needed for test)
		return nil
	}

	// CREATE auth_session
	if strings.Contains(sql, "CREATE auth_session CONTENT") {
		account, _ := vars["account"].(string)
		hash, _ := vars["refresh_hash"].(string)
		m.sessions[hash] = account
		return nil
	}

	// UPDATE auth_session SET refresh_token_hash = $new_hash WHERE refresh_token_hash = $old_hash
	if strings.Contains(sql, "UPDATE auth_session SET") && strings.Contains(sql, "refresh_token_hash = $new_hash") {
		oldHash, _ := vars["old_hash"].(string)
		newHash, _ := vars["new_hash"].(string)
		if acct, ok := m.sessions[oldHash]; ok {
			delete(m.sessions, oldHash)
			m.sessions[newHash] = acct
			if dest != nil {
				b, _ := json.Marshal([]struct {
					Account   string `json:"account"`
					ExpiresAt string `json:"expiresAt"`
				}{{Account: acct, ExpiresAt: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)}})
				_ = json.Unmarshal(b, dest)
			}
		} else if dest != nil {
			b, _ := json.Marshal([]struct {
				Account   string `json:"account"`
				ExpiresAt string `json:"expiresAt"`
			}{})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// SELECT FROM auth_session WHERE refresh_token_hash = $hash
	if strings.Contains(sql, "FROM auth_session") && strings.Contains(sql, "refresh_token_hash = $hash") {
		hash, _ := vars["hash"].(string)
		if acct, ok := m.sessions[hash]; ok {
			if dest != nil {
				b, _ := json.Marshal([]struct {
					Account   string `json:"account"`
					ExpiresAt string `json:"expiresAt"`
				}{{Account: acct, ExpiresAt: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)}})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		if dest != nil {
			b, _ := json.Marshal([]struct {
				Account   string `json:"account"`
				ExpiresAt string `json:"expiresAt"`
			}{})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// SELECT for validRecord etc. - fallback return empty
	if dest != nil {
		// Try to set empty slice if dest is slice
		b, _ := json.Marshal([]interface{}{})
		_ = json.Unmarshal(b, dest)
	}
	return nil
}

func newTestServerWithMockDB(mock *integrationMockDB, logger *slog.Logger) (*Server, http.Handler) {
	cfg := config.Config{
		JWTSecret:          strings.Repeat("s", 32),
		OTPPepper:          strings.Repeat("p", 32),
		AccessTokenMinutes: 15,
		RefreshTokenDays:   30,
		OTPMode:            "development",
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// Use Server directly with custom limiters for test determinism
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	srv.limiter = &surrealRateLimiter{db: &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }}, log: logger} // not used for message creation in these tests unless needed

	// Use high limits for determinism tests to avoid 429
	mux := http.NewServeMux()
	readLimiter := newEndpointLimiter(1000, time.Minute, "read", logger)
	eventLimiter := newEndpointLimiter(1000, time.Minute, "events", logger)

	mux.HandleFunc("GET /health", srv.health)
	mux.Handle("GET /v1/me", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.me))))
	mux.Handle("GET /v1/conversations", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listConversations))))
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	mux.Handle("GET /v1/events/messages", srv.requireAuth(eventLimiter.middleware(accountKeyFunc("events"), http.HandlerFunc(srv.messageEvents))))
	mux.Handle("POST /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.sendMessage)))
	mux.HandleFunc("POST /v1/auth/refresh", srv.refresh)
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))
	// Need to set srv's limiter for message creation? Use high limit
	return srv, handler
}

func TestTwoDevicesIdenticalHistory(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:oroto31"
	other := "account:other1"
	convID := "conversation:oroto31"
	mock.members[convID] = []string{account, other}
	mock.accounts[account] = accountAuth{ID: account, Username: "oroto31", DisplayName: "Oroto", FullName: "Oroto Test", Phone: "+9000000001", CountryCode: "TR"}
	// Pre-populate 50 messages with canonical IDs
	for i := 0; i < 50; i++ {
		created := time.Now().UTC().Add(time.Duration(-50+i) * time.Minute).Format(time.RFC3339Nano)
		body := fmt.Sprintf("msg %d", i)
		msg := messageView{
			ID:           fmt.Sprintf("message:%024d", i),
			ClientID:     fmt.Sprintf("client-%08d", i),
			Conversation: convID,
			SenderID:     account,
			Body:         &body,
			Kind:         "text",
			CreatedAt:    created,
			Status:       "sent",
			Reactions:    []reactionView{},
		}
		mock.messages[convID] = append(mock.messages[convID], msg)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, handler := newTestServerWithMockDB(mock, logger)

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token1, _ := security.AccessToken(cfg.JWTSecret, account, 15)
	token2, _ := security.AccessToken(cfg.JWTSecret, account, 15)

	// Concurrent calls from two devices
	var wg sync.WaitGroup
	results := make(chan string, 6)
	doGet := func(token string) {
		defer wg.Done()
		// GET /v1/conversations
		req := httptest.NewRequest("GET", "/v1/conversations", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			results <- fmt.Sprintf("conversations %d", w.Code)
			return
		}
		// GET /v1/conversations/{id}/messages?limit=50
		req = httptest.NewRequest("GET", "/v1/conversations/oroto31/messages?limit=50", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.SetPathValue("id", "oroto31")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			results <- fmt.Sprintf("messages %d %s", w.Code, w.Body.String())
			return
		}
		var resp struct {
			Messages []messageView `json:"messages"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			results <- "unmarshal"
			return
		}
		if len(resp.Messages) != 50 {
			results <- fmt.Sprintf("len %d", len(resp.Messages))
			return
		}
		// Validate canonical IDs and RFC3339
		for _, m := range resp.Messages {
			if !strings.HasPrefix(m.ID, "message:") || !strings.HasPrefix(m.Conversation, "conversation:") || !strings.HasPrefix(m.SenderID, "account:") {
				results <- "canonical"
				return
			}
			if _, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err != nil {
				if _, err2 := time.Parse(time.RFC3339, m.CreatedAt); err2 != nil {
					results <- "time"
					return
				}
			}
		}
		// GET /v1/events/messages?after=999999 - use large after to avoid 25s long-poll wait, still tests determinism
		req = httptest.NewRequest("GET", "/v1/events/messages?after=999999", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			results <- fmt.Sprintf("events %d", w.Code)
			return
		}
		results <- "ok"
	}

	wg.Add(2)
	go doGet(token1)
	go doGet(token2)
	wg.Wait()
	close(results)
	for r := range results {
		if r != "ok" {
			t.Fatalf("two devices identical history failed: %s", r)
		}
	}
	// Also verify that both devices got identical message IDs in same order
	req1 := httptest.NewRequest("GET", "/v1/conversations/oroto31/messages?limit=50", nil)
	req1.Header.Set("Authorization", "Bearer "+token1)
	req1.SetPathValue("id", "oroto31")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	req2 := httptest.NewRequest("GET", "/v1/conversations/oroto31/messages?limit=50", nil)
	req2.Header.Set("Authorization", "Bearer "+token2)
	req2.SetPathValue("id", "oroto31")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("histories differ between devices:\n%s\nvs\n%s", w1.Body.String(), w2.Body.String())
	}
}

func TestRefreshRotationDoesNotInvalidateOther(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:refreshUser"
	mock.accounts[account] = accountAuth{ID: account, Username: "refreshUser", DisplayName: "Refresh", FullName: "Refresh User", Phone: "+9000000002", CountryCode: "TR"}
	mock.members["conversation:abc"] = []string{account, "account:other"}
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), OTPPepper: strings.Repeat("p", 32), AccessTokenMinutes: 15, RefreshTokenDays: 30, OTPMode: "development"}

	// Create two sessions
	refresh1, _ := security.RefreshToken()
	refresh2, _ := security.RefreshToken()
	hash1 := security.TokenHash(refresh1)
	hash2 := security.TokenHash(refresh2)
	mock.sessions[hash1] = account
	mock.sessions[hash2] = account

	access1, _ := security.AccessToken(cfg.JWTSecret, account, 15)
	access2, _ := security.AccessToken(cfg.JWTSecret, account, 15)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, handler := newTestServerWithMockDB(mock, logger)
	// Need to use config with same secret for handler
	srvCfg := config.Config{JWTSecret: cfg.JWTSecret, OTPPepper: cfg.OTPPepper, AccessTokenMinutes: 15, RefreshTokenDays: 30, OTPMode: "development"}
	// Recreate handler with correct cfg
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: srvCfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/me", srv.requireAuth(http.HandlerFunc(srv.me)))
	mux.HandleFunc("POST /v1/auth/refresh", srv.refresh)
	handler = recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Both devices can call /v1/me
	for _, tok := range []string{access1, access2} {
		req := httptest.NewRequest("GET", "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("initial me %d %s", w.Code, w.Body.String())
		}
	}

	// Rotate refresh1
	req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":"%s"}`, refresh1)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("refresh1 %d %s", w.Code, w.Body.String())
	}
	var tokens security.Tokens
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatalf("unmarshal tokens %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatal("empty tokens after refresh")
	}

	// Device 2 should still be able to use its access token and refresh token
	req = httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+access2)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("device2 after rotation me %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":"%s"}`, refresh2)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("device2 refresh %d %s", w.Code, w.Body.String())
	}

	// Old refresh1 should now be invalid
	req = httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":"%s"}`, refresh1)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("old refresh should be 401 got %d", w.Code)
	}
}

func TestRateLimitedUnrelatedReadNotEmpty200(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:rateUser"
	mock.accounts[account] = accountAuth{ID: account, Username: "rateUser", DisplayName: "Rate", FullName: "Rate User", Phone: "+9000000003", CountryCode: "TR"}
	mock.members["conversation:abc"] = []string{account, "account:other"}
	// One message for non-empty check
	body := "hello"
	mock.messages["conversation:abc"] = []messageView{{ID: "message:1", ClientID: "c1", Conversation: "conversation:abc", SenderID: account, Body: &body, Kind: "text", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "sent"}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, account, 15)

	// Create server with small read limiter to easily exhaust
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	readLimiter := newEndpointLimiter(5, time.Minute, "read", logger)
	eventLimiter := newEndpointLimiter(1000, time.Minute, "events", logger)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/me", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.me))))
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	mux.Handle("GET /v1/events/messages", srv.requireAuth(eventLimiter.middleware(accountKeyFunc("events"), http.HandlerFunc(srv.messageEvents))))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Exhaust readLimiter for /v1/me (5 per minute)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("warmup %d got %d", i, w.Code)
		}
	}
	// 6th should be 429
	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("expected 429 for 6th me got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on 429")
	}
	// Unrelated read should not return empty 200; it should either be 429 (if same bucket) or 200 with correct data, but never empty 200 when data exists
	// Since readLimiter is per-account per-path? Currently it's per-account per-bucket with path, so /v1/me and /v1/conversations/{id}/messages have separate buckets if per-path. To test isolation, we use per-account per-bucket sharing, so they share same bucket and will also be 429. That's not empty 200, so passes.
	// If they are per-path, then unrelated read will be 200 with data, also not empty 200.
	req = httptest.NewRequest("GET", "/v1/conversations/abc/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.SetPathValue("id", "abc")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == 200 {
		var resp struct {
			Messages []messageView `json:"messages"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Messages) == 0 {
			t.Fatalf("unrelated read returned empty 200, should not be empty when data exists; body %s", w.Body.String())
		}
		// If 200, it should have correct data
	} else if w.Code == 429 {
		if w.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After on unrelated 429")
		}
		// Also not empty 200, so passes
	} else {
		t.Fatalf("unrelated read unexpected code %d", w.Code)
	}
}

func TestEmptyConversationVsFailure(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:emptyUser"
	other := "account:otherEmpty"
	mock.accounts[account] = accountAuth{ID: account, Username: "emptyUser", DisplayName: "Empty", FullName: "Empty User", Phone: "+9000000004", CountryCode: "TR"}
	convEmpty := "conversation:empty"
	convNoAccess := "conversation:noaccess"
	mock.members[convEmpty] = []string{account, other}
	mock.members[convNoAccess] = []string{"account:third", other}
	// convEmpty has no messages, convNoAccess not member

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, account, 15)

	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	readLimiter := newEndpointLimiter(1000, time.Minute, "read", logger)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Empty conversation should be 200 with messages: []
	req := httptest.NewRequest("GET", "/v1/conversations/empty/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.SetPathValue("id", "empty")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("empty conv expected 200 got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %v", err)
	}
	if resp.Messages == nil {
		t.Fatal("messages should be [] not null")
	}
	if len(resp.Messages) != 0 {
		t.Fatalf("expected 0 messages got %d", len(resp.Messages))
	}

	// Not a member should be 403, not empty 200
	req = httptest.NewRequest("GET", "/v1/conversations/noaccess/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.SetPathValue("id", "noaccess")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == 200 {
		t.Fatalf("no access should not be 200, got %s", w.Body.String())
	}
	if w.Code < 400 {
		t.Fatalf("expected non-200 failure, got %d", w.Code)
	}
}

func TestHealthAndLongPollNotExhaustReads(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:healthUser"
	mock.accounts[account] = accountAuth{ID: account, Username: "healthUser", DisplayName: "Health", FullName: "Health User", Phone: "+9000000005", CountryCode: "TR"}
	mock.members["conversation:abc"] = []string{account, "account:other"}
	body := "hi"
	mock.messages["conversation:abc"] = []messageView{{ID: "message:1", ClientID: "c1", Conversation: "conversation:abc", SenderID: account, Body: &body, Kind: "text", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "sent"}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, account, 15)

	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	// Use separate limiters: reads 300, events 5 for test, health no limit
	readLimiter := newEndpointLimiter(300, time.Minute, "read", logger)
	eventLimiter := newEndpointLimiter(5, time.Minute, "events", logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", srv.health)
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	mux.Handle("GET /v1/events/messages", srv.requireAuth(eventLimiter.middleware(accountKeyFunc("events"), http.HandlerFunc(srv.messageEvents))))
	mux.Handle("GET /v1/me", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.me))))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Exhaust event limiter
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/v1/events/messages?after=999999", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		// events may return 200 or 200 with wait, but not 429 until 6th
		if w.Code == 429 {
			t.Fatalf("event warmup %d unexpected 429", i)
		}
	}
	req := httptest.NewRequest("GET", "/v1/events/messages?after=999999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("event 6th should be 429 got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After for events")
	}
	// Health should still be 200
	req = httptest.NewRequest("GET", "/health", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("health should be 200 even after events rate limit, got %d", w.Code)
	}
	// Ordinary read should still be 200 (not exhausted by events)
	req = httptest.NewRequest("GET", "/v1/conversations/abc/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.SetPathValue("id", "abc")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("ordinary read should not be exhausted by events, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages) == 0 {
		t.Fatal("ordinary read should have messages, not empty")
	}
	// Also GET /v1/me should still be 200
	req = httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("me should be 200 got %d", w.Code)
	}
}

func TestMessageSchemaCanonical(t *testing.T) {
	mock := newIntegrationMockDB()
	account := "account:schemaUser"
	other := "account:otherSchema"
	conv := "conversation:schema"
	mock.members[conv] = []string{account, other}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	body := "test"
	mock.messages[conv] = []messageView{{ID: "message:abc123", ClientID: "client-12345678", Conversation: conv, SenderID: account, Body: &body, Kind: "text", CreatedAt: created, Status: "sent", Reactions: []reactionView{}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, account, 15)
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: mock, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(mock, ps, mc, logger)
	readLimiter := newEndpointLimiter(1000, time.Minute, "read", logger)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	req := httptest.NewRequest("GET", "/v1/conversations/schema/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.SetPathValue("id", "schema")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 got %d", w.Code)
	}
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("expected 1 got %d", len(resp.Messages))
	}
	m := resp.Messages[0]
	if !strings.HasPrefix(m.ID, "message:") {
		t.Fatalf("id %s", m.ID)
	}
	if !strings.HasPrefix(m.Conversation, "conversation:") {
		t.Fatalf("conv %s", m.Conversation)
	}
	if !strings.HasPrefix(m.SenderID, "account:") {
		t.Fatalf("sender %s", m.SenderID)
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err != nil {
		if _, err2 := time.Parse(time.RFC3339, m.CreatedAt); err2 != nil {
			t.Fatalf("invalid createdAt %s", m.CreatedAt)
		}
	}
	// Ensure X-Request-ID present
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

// Ensure 429 always has Retry-After
func TestEvery429HasRetryAfter(t *testing.T) {
	limiter := newEndpointLimiter(1, time.Minute, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := limiter.middleware(func(r *http.Request) string { return "key" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first 200 got %d", w.Code)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("second 429 got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}
