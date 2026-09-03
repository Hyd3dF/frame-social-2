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

// pushMockDB simulates SurrealDB for push_device operations.
type pushMockDB struct {
	mu           sync.Mutex
	byID         map[string]pushDevice
	byAcctDevice map[string]string // key account|deviceId -> id
	accounts     map[string]string // account -> displayName
}

func newPushMockDB() *pushMockDB {
	return &pushMockDB{
		byID:         make(map[string]pushDevice),
		byAcctDevice: make(map[string]string),
		accounts:     make(map[string]string),
	}
}

func (m *pushMockDB) Ping(ctx context.Context) error { return nil }

func (m *pushMockDB) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(sql, "DEFINE") {
		return nil
	}
	// Display name fetch
	if strings.Contains(sql, "SELECT display_name AS displayName FROM type::record($account)") {
		acct, _ := vars["account"].(string)
		name := m.accounts[acct]
		if name == "" {
			// fallback to account suffix
			parts := strings.SplitN(acct, ":", 2)
			if len(parts) == 2 {
				name = parts[1]
			} else {
				name = "Test User"
			}
		}
		if dest != nil {
			b, _ := json.Marshal([]map[string]string{{"displayName": name}})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}
	// Delete by account+deviceId: DELETE must be checked before SELECT (both contain same FROM substring)
	if strings.Contains(sql, "DELETE FROM push_device WHERE account = type::record($account) AND device_id = $deviceId") {
		acct, _ := vars["account"].(string)
		devID, _ := vars["deviceId"].(string)
		key := acct + "|" + devID
		if id, ok := m.byAcctDevice[key]; ok {
			delete(m.byID, id)
			delete(m.byAcctDevice, key)
		}
		return nil
	}
	// Upsert check: SELECT from push_device where account and device_id
	if strings.Contains(sql, "FROM push_device WHERE account = type::record($account) AND device_id = $deviceId") {
		acct, _ := vars["account"].(string)
		devID, _ := vars["deviceId"].(string)
		key := acct + "|" + devID
		if id, ok := m.byAcctDevice[key]; ok {
			dev := m.byID[id]
			if dest != nil {
				b, _ := json.Marshal([]pushDevice{dev})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		if dest != nil {
			b, _ := json.Marshal([]pushDevice{})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}
	// List by account: SELECT ... FROM push_device WHERE account = type::record($account)
	if strings.Contains(sql, "FROM push_device WHERE account = type::record($account)") && !strings.Contains(sql, "device_id = $deviceId") {
		acct, _ := vars["account"].(string)
		var list []pushDevice
		for _, dev := range m.byID {
			if dev.Account == acct {
				list = append(list, dev)
			}
		}
		if dest != nil {
			b, _ := json.Marshal(list)
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}
	// Delete by tokens: DELETE FROM push_device WHERE token IN $tokens
	if strings.Contains(sql, "DELETE FROM push_device WHERE token IN $tokens") {
		raw, _ := vars["tokens"]
		var tokens []string
		switch v := raw.(type) {
		case []string:
			tokens = v
		case []interface{}:
			for _, x := range v {
				if s, ok := x.(string); ok {
					tokens = append(tokens, s)
				}
			}
		}
		set := make(map[string]bool)
		for _, t := range tokens {
			set[t] = true
		}
		for id, dev := range m.byID {
			if set[dev.Token] {
				key := dev.Account + "|" + dev.DeviceID
				delete(m.byID, id)
				delete(m.byAcctDevice, key)
			}
		}
		return nil
	}
	// UPDATE type::record($id) SET token = $fcmToken, platform = $platform.
	// "token" is a protected SurrealDB bind-variable name.
	if strings.Contains(sql, "UPDATE type::record($id) SET token = $fcmToken") {
		id, _ := vars["id"].(string)
		token, _ := vars["fcmToken"].(string)
		platform, _ := vars["platform"].(string)
		if dev, ok := m.byID[id]; ok {
			dev.Token = token
			dev.Platform = platform
			dev.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			m.byID[id] = dev
			if dest != nil {
				// RETURN AFTER
				b, _ := json.Marshal([]pushDevice{dev})
				_ = json.Unmarshal(b, dest)
			}
		} else if dest != nil {
			b, _ := json.Marshal([]pushDevice{})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}
	// CREATE push_device CONTENT
	if strings.Contains(sql, "CREATE push_device CONTENT") {
		acct, _ := vars["account"].(string)
		token, _ := vars["fcmToken"].(string)
		platform, _ := vars["platform"].(string)
		devID, _ := vars["deviceId"].(string)
		key := acct + "|" + devID
		if _, exists := m.byAcctDevice[key]; exists {
			return fmt.Errorf("record already exists")
		}
		// Generate deterministic ID
		suffix := strings.TrimPrefix(acct, "account:")
		if suffix == acct {
			suffix = acct
		}
		// replace spaces
		suffix = strings.ReplaceAll(suffix, " ", "_")
		devIDSan := strings.ReplaceAll(devID, " ", "_")
		id := "push_device:" + suffix + "_" + devIDSan
		// ensure unique
		if _, exists := m.byID[id]; exists {
			id = fmt.Sprintf("push_device:%s_%s_%d", suffix, devIDSan, len(m.byID)+1)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		dev := pushDevice{ID: id, Account: acct, Token: token, Platform: platform, DeviceID: devID, CreatedAt: now, UpdatedAt: now}
		m.byID[id] = dev
		m.byAcctDevice[key] = id
		if dest != nil {
			b, _ := json.Marshal([]pushDevice{dev})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}
	// Fallback for other queries (message persist, dedup, etc.) just return nil/empty
	if dest != nil {
		// try to set empty slice if dest is slice
		// Use json to set empty
		b, _ := json.Marshal([]interface{}{})
		_ = json.Unmarshal(b, dest)
	}
	return nil
}

// Helper to create Server with push mocks
func newPushTestServer(db *pushMockDB, pusher Pusher) *Server {
	if db == nil {
		db = newPushMockDB()
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{
		cfg:       config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15},
		db:        db,
		events:    newMessageEventBroker(),
		log:       logger,
		members:   mc,
		pending:   ps,
		pushStore: newPushStore(db, logger),
		pusher:    pusher,
	}
	// avoid full New's limiter/persist initialization for simplicity
	srv.persist = newPersister(db, ps, mc, logger)
	srv.limiter = &surrealRateLimiter{db: &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error { return nil }}, log: logger}
	return srv
}

func TestValidatePushDeviceInput(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		platform string
		deviceID string
		wantCode string
	}{
		{"valid ios", strings.Repeat("a", 20), "ios", "device-12345678", ""},
		{"valid android", strings.Repeat("b", 100), "android", "device-ABCDEFGH", ""},
		{"valid web", strings.Repeat("c", 50), "web", "device-web-1234567890", ""},
		{"empty token", "", "ios", "device-12345678", "invalid_token"},
		{"short token", "short", "ios", "device-12345678", "invalid_token"},
		{"long token", strings.Repeat("x", 5000), "ios", "device-12345678", "invalid_token"},
		{"token with space", "token with space abcdefghijkl", "ios", "device-12345678", "invalid_token"},
		{"invalid platform", strings.Repeat("a", 20), "windows", "device-12345678", "invalid_platform"},
		{"empty platform", strings.Repeat("a", 20), "", "device-12345678", "invalid_platform"},
		{"short deviceId", strings.Repeat("a", 20), "ios", "short", "invalid_device"},
		{"long deviceId", strings.Repeat("a", 20), "ios", strings.Repeat("d", 300), "invalid_device"},
		{"empty deviceId", strings.Repeat("a", 20), "ios", "", "invalid_device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := validatePushDeviceInput(tc.token, tc.platform, tc.deviceID)
			if code != tc.wantCode {
				t.Fatalf("validatePushDeviceInput got code %q want %q", code, tc.wantCode)
			}
		})
	}
}

func TestPutPushDeviceSuccess(t *testing.T) {
	db := newPushMockDB()
	pusher := &mockPusher{}
	srv := newPushTestServer(db, pusher)
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)

	mux := http.NewServeMux()
	readLimiter := newEndpointLimiter(1000, time.Minute, "read", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux.Handle("PUT /v1/me/push-devices", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.putPushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))

	body := fmt.Sprintf(`{"token":"%s","platform":"ios","deviceId":"device-12345678"}`, strings.Repeat("t", 30))
	req := httptest.NewRequest("PUT", "/v1/me/push-devices", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d %s", w.Code, w.Body.String())
	}
	var resp pushDeviceView
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %v", err)
	}
	if resp.DeviceID != "device-12345678" || resp.Platform != "ios" || resp.Token != strings.Repeat("t", 30) {
		t.Fatalf("resp mismatch %+v", resp)
	}
	if resp.CreatedAt == "" || resp.UpdatedAt == "" {
		t.Fatalf("missing timestamps %+v", resp)
	}
	// Verify stored
	devices, _ := srv.pushStore.ListByAccount(context.Background(), "account:alice")
	if len(devices) != 1 || devices[0].Token != strings.Repeat("t", 30) {
		t.Fatalf("stored devices %+v", devices)
	}
}

func TestPutPushDeviceUpdateToken(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	token, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	mux := http.NewServeMux()
	rl := newEndpointLimiter(1000, time.Minute, "read", srv.log)
	mux.Handle("PUT /v1/me/push-devices", srv.requireAuth(rl.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.putPushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))

	first := strings.Repeat("a", 30)
	second := strings.Repeat("b", 35)
	for _, tok := range []string{first, second} {
		body := fmt.Sprintf(`{"token":"%s","platform":"android","deviceId":"device-update-123"}`, tok)
		req := httptest.NewRequest("PUT", "/v1/me/push-devices", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("put %d %s", w.Code, w.Body.String())
		}
	}
	devices, _ := srv.pushStore.ListByAccount(context.Background(), "account:alice")
	if len(devices) != 1 {
		t.Fatalf("expected 1 device after update got %d", len(devices))
	}
	if devices[0].Token != second {
		t.Fatalf("expected token updated to %q got %q", second, devices[0].Token)
	}
	if devices[0].Platform != "android" {
		t.Fatalf("platform %q", devices[0].Platform)
	}
}

func TestPutPushDeviceInvalidInputs(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	access, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	mux := http.NewServeMux()
	rl := newEndpointLimiter(1000, time.Minute, "read", srv.log)
	mux.Handle("PUT /v1/me/push-devices", srv.requireAuth(rl.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.putPushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))

	cases := []struct {
		name string
		body string
		code string
	}{
		{"short token", `{"token":"short","platform":"ios","deviceId":"device-12345678"}`, "invalid_token"},
		{"invalid platform", fmt.Sprintf(`{"token":"%s","platform":"windows","deviceId":"device-12345678"}`, strings.Repeat("x", 30)), "invalid_platform"},
		{"short device", fmt.Sprintf(`{"token":"%s","platform":"ios","deviceId":"short"}`, strings.Repeat("x", 30)), "invalid_device"},
		{"missing fields", `{"platform":"ios"}`, "invalid_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/v1/me/push-devices", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+access)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 400 {
				t.Fatalf("expected 400 got %d %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected code %q in body %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestPutPushDeviceUnauthorized(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	mux := http.NewServeMux()
	rl := newEndpointLimiter(1000, time.Minute, "read", srv.log)
	mux.Handle("PUT /v1/me/push-devices", srv.requireAuth(rl.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.putPushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))
	body := fmt.Sprintf(`{"token":"%s","platform":"ios","deviceId":"device-12345678"}`, strings.Repeat("t", 30))
	req := httptest.NewRequest("PUT", "/v1/me/push-devices", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 got %d", w.Code)
	}
}

func TestDeletePushDeviceSuccess(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	// Pre-insert
	_, _ = srv.pushStore.Upsert(context.Background(), "account:alice", "device-del-123", strings.Repeat("t", 30), "ios")
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	access, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	mux := http.NewServeMux()
	rl := newEndpointLimiter(1000, time.Minute, "read", srv.log)
	mux.Handle("DELETE /v1/me/push-devices/{deviceId}", srv.requireAuth(rl.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.deletePushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))
	req := httptest.NewRequest("DELETE", "/v1/me/push-devices/device-del-123", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.SetPathValue("deviceId", "device-del-123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("expected 204 got %d %s", w.Code, w.Body.String())
	}
	devices, _ := srv.pushStore.ListByAccount(context.Background(), "account:alice")
	if len(devices) != 0 {
		t.Fatalf("expected 0 devices after delete got %d", len(devices))
	}
}

func TestDeletePushDeviceIdempotent(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	access, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	mux := http.NewServeMux()
	rl := newEndpointLimiter(1000, time.Minute, "read", srv.log)
	mux.Handle("DELETE /v1/me/push-devices/{deviceId}", srv.requireAuth(rl.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.deletePushDevice))))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))
	req := httptest.NewRequest("DELETE", "/v1/me/push-devices/nonexistent-device-1234", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.SetPathValue("deviceId", "nonexistent-device-1234")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("expected 204 for idempotent delete got %d", w.Code)
	}
}

func TestPushStoreMultipleDevices(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	ctx := context.Background()
	_, _ = srv.pushStore.Upsert(ctx, "account:alice", "device-11111111", strings.Repeat("a", 30), "ios")
	_, _ = srv.pushStore.Upsert(ctx, "account:alice", "device-22222222", strings.Repeat("b", 30), "android")
	_, _ = srv.pushStore.Upsert(ctx, "account:alice", "device-33333333", strings.Repeat("c", 30), "web")
	devices, err := srv.pushStore.ListByAccount(ctx, "account:alice")
	if err != nil {
		t.Fatalf("list %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("expected 3 devices got %d %+v", len(devices), devices)
	}
	// Ensure distinct deviceIds
	ids := make(map[string]bool)
	for _, d := range devices {
		ids[d.DeviceID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("distinct ids %v", ids)
	}
}

func TestPushOnNewMessageSendsToRecipient(t *testing.T) {
	db := newPushMockDB()
	db.accounts["account:alice"] = "Alice Display"
	db.accounts["account:bob"] = "Bob Display"
	mockP := &mockPusher{}
	srv := newPushTestServer(db, mockP)
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	// Bob has one device
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-12345", strings.Repeat("tok", 10), "ios")
	// Alice also has device but should not receive
	_, _ = srv.pushStore.Upsert(context.Background(), "account:alice", "device-alice-1234", strings.Repeat("tok2", 10), "android")

	// Directly call sendPushForMessage synchronously for determinism
	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:abc", "message:xyz", []string{"account:alice", "account:bob"})

	calls := mockP.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 push call got %d %+v", len(calls), calls)
	}
	call := calls[0]
	if len(call.Tokens) != 1 || call.Tokens[0] != strings.Repeat("tok", 10) {
		t.Fatalf("tokens %v", call.Tokens)
	}
	if call.Title != "Alice Display" {
		t.Fatalf("title %q want Alice Display", call.Title)
	}
	if call.Body != "Yeni bir mesajın var" {
		t.Fatalf("body %q", call.Body)
	}
	if call.Data["type"] != "new_message" || call.Data["conversationId"] != "conversation:abc" || call.Data["messageId"] != "message:xyz" || call.Data["senderId"] != "account:alice" {
		t.Fatalf("data %+v", call.Data)
	}
}

func TestPushNotSentToSender(t *testing.T) {
	db := newPushMockDB()
	mockP := &mockPusher{}
	srv := newPushTestServer(db, mockP)
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	_, _ = srv.pushStore.Upsert(context.Background(), "account:alice", "device-alice-123456", strings.Repeat("a", 30), "ios")
	// No bob device -> should not send at all (but also verify alice not sent)
	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:abc", "message:1", []string{"account:alice", "account:bob"})
	if len(mockP.Calls()) != 0 {
		t.Fatalf("should not send when recipient has no devices, calls %v", mockP.Calls())
	}
	// Now add bob device, alice still has device but should only send to bob
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-12345678", strings.Repeat("b", 30), "android")
	mockP2 := &mockPusher{}
	srv.pusher = mockP2
	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:abc", "message:2", []string{"account:alice", "account:bob"})
	calls := mockP2.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call")
	}
	// Ensure not sending alice token
	for _, tok := range calls[0].Tokens {
		if tok == strings.Repeat("a", 30) {
			t.Fatalf("should not send to sender token")
		}
	}
}

func TestPushMultipleDevicesAllNotified(t *testing.T) {
	db := newPushMockDB()
	db.accounts["account:alice"] = "Alice"
	mockP := &mockPusher{}
	srv := newPushTestServer(db, mockP)
	srv.members.Set("conversation:group1", []string{"account:alice", "account:bob", "account:carol"})
	// Bob has 2 devices, Carol has 1
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-1-12345", strings.Repeat("b1", 15), "ios")
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-2-12345", strings.Repeat("b2", 15), "android")
	_, _ = srv.pushStore.Upsert(context.Background(), "account:carol", "device-carol-1-1234", strings.Repeat("c1", 15), "web")

	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:group1", "message:multi", []string{"account:alice", "account:bob", "account:carol"})
	calls := mockP.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one batched call got %d", len(calls))
	}
	// Collect token counts
	totalTokens := 0
	for _, c := range calls {
		totalTokens += len(c.Tokens)
	}
	if totalTokens != 3 {
		t.Fatalf("expected 3 tokens total got %d calls %+v", totalTokens, calls)
	}
}

func TestPushInvalidTokenCleanup(t *testing.T) {
	db := newPushMockDB()
	db.accounts["account:alice"] = "Alice"
	tokValid := strings.Repeat("v", 30)
	tokInvalid := strings.Repeat("i", 30)
	mockP := &mockPusher{
		sendFunc: func(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
			// Mark second token as invalid
			return []string{tokInvalid}, nil
		},
	}
	srv := newPushTestServer(db, mockP)
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-valid-1234", tokValid, "ios")
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-invalid-12", tokInvalid, "ios")

	// Ensure both stored
	devs, _ := srv.pushStore.ListByAccount(context.Background(), "account:bob")
	if len(devs) != 2 {
		t.Fatalf("pre 2 devices")
	}
	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:abc", "message:clean", []string{"account:alice", "account:bob"})

	// After push, invalid should be deleted
	devs, _ = srv.pushStore.ListByAccount(context.Background(), "account:bob")
	if len(devs) != 1 {
		t.Fatalf("expected 1 device after cleanup got %d %+v", len(devs), devs)
	}
	if devs[0].Token != tokValid {
		t.Fatalf("remaining token should be valid %q", devs[0].Token)
	}
}

func TestPushFailureDoesNotFailMessage(t *testing.T) {
	db := newPushMockDB()
	// Setup mock pusher that always fails
	failingPusher := &mockPusher{
		sendFunc: func(ctx context.Context, tokens []string, title, body string, data map[string]string) ([]string, error) {
			return nil, fmt.Errorf("fcm unavailable")
		},
	}
	srv := newPushTestServer(db, failingPusher)
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-123456789", strings.Repeat("t", 30), "ios")

	// Send message via HTTP; should still succeed 201 despite push failure (push is async but we test direct sendPush path logs error)
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	// Use Server's sendMessage which triggers async push; we need to ensure message persists even if push fails synchronously in helper
	// So test sendPushForMessage directly: it should log error but not panic and should not return error that propagates to message flow
	// Just ensuring it doesn't panic and handles error
	srv.sendPushForMessage(context.Background(), "account:alice", "conversation:abc", "message:failtest", []string{"account:alice", "account:bob"})
	// No assertion on error; just that db still has device (not deleted since no invalid)
	devs, _ := srv.pushStore.ListByAccount(context.Background(), "account:bob")
	if len(devs) != 1 {
		t.Fatalf("device should still exist after transient failure")
	}

	// Now test HTTP message flow with failing pusher injected
	srv.pusher = failingPusher
	// Need to bypass rate limiter for message: use mock that allows
	// Create a request
	mux := http.NewServeMux()
	mux.Handle("POST /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.sendMessage)))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))
	access, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"hello push fail","clientId":"client-12345678"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("message should succeed 201 even with push failure, got %d %s", w.Code, w.Body.String())
	}
	// Wait a bit for async push to be attempted (but failing)
	time.Sleep(100 * time.Millisecond)
	// Message still persisted: check mock db that message handling didn't error
	// We already ensured 201, so pass
}

func TestPushIntegrationViaSendMessage(t *testing.T) {
	db := newPushMockDB()
	db.accounts["account:alice"] = "Alice Wonderland"
	mockP := &mockPusher{}
	srv := newPushTestServer(db, mockP)
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	_, _ = srv.pushStore.Upsert(context.Background(), "account:bob", "device-bob-integration", strings.Repeat("z", 30), "ios")

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.sendMessage)))
	handler := recoverer(srv.log, securityHeaders(requestIDMiddleware(mux)))
	access, _ := security.AccessToken(cfg.JWTSecret, "account:alice", 15)
	req := httptest.NewRequest("POST", "/v1/conversations/abc/messages", strings.NewReader(`{"body":"integration hello","clientId":"client-integration-123"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("sendMessage expected 201 got %d %s", w.Code, w.Body.String())
	}
	// Wait for async push
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mockP.Calls()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := mockP.Calls()
	if len(calls) == 0 {
		t.Fatalf("expected push after message, got none")
	}
	found := false
	for _, c := range calls {
		if c.Data["type"] == "new_message" && c.Data["conversationId"] == "conversation:abc" && c.Data["senderId"] == "account:alice" && c.Title == "Alice Wonderland" {
			found = true
			if c.Body != "Yeni bir mesajın var" {
				t.Fatalf("body %q", c.Body)
			}
		}
	}
	if !found {
		t.Fatalf("payload not correct %+v", calls)
	}
}

func TestPushQueueSaturationDoesNotBlockMessageDelivery(t *testing.T) {
	srv := newPushTestServer(newPushMockDB(), &mockPusher{})
	srv.pushQueue = make(chan pushJob, 1)
	srv.pushQueue <- pushJob{}
	start := time.Now()
	srv.triggerPushForMessage("account:alice", "conversation:abc", "message:one", []string{"account:alice", "account:bob"})
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a full push queue must not block message delivery")
	}
}

func TestConfigFirebaseEnabled(t *testing.T) {
	// No env
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_BASE64", "")
	t.Setenv("FIREBASE_PROJECT_ID", "")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))
	t.Setenv("OTP_PEPPER", strings.Repeat("p", 32))
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.com")
	t.Setenv("SURREAL_DATABASE", "app")
	t.Setenv("SURREAL_NAMESPACE", "ns")
	t.Setenv("SURREAL_PASSWORD", "pass")
	t.Setenv("SURREAL_PROXY_TOKEN", strings.Repeat("x", 64))
	t.Setenv("SURREAL_URL", "https://surreal.example.com")
	t.Setenv("SURREAL_USERNAME", "user")
	t.Setenv("APP_ENV", "development")
	t.Setenv("OTP_MODE", "development")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load %v", err)
	}
	if cfg.FirebaseEnabled() {
		t.Fatalf("should be disabled when no creds")
	}
	// With JSON
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", `{"type":"service_account","project_id":"test"}`)
	cfg, _ = config.Load()
	if !cfg.FirebaseEnabled() {
		t.Fatal("should be enabled with JSON")
	}
	creds, err := cfg.FirebaseCredentials()
	if err != nil || len(creds) == 0 {
		t.Fatalf("creds %v %v", creds, err)
	}
	// With base64
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_BASE64", "eyJ0eXBlIjoic2VydmljZV9hY2NvdW50In0=") // {"type":"service_account"}
	cfg, _ = config.Load()
	if !cfg.FirebaseEnabled() {
		t.Fatal("base64 should enable")
	}
	creds, err = cfg.FirebaseCredentials()
	if err != nil {
		t.Fatalf("base64 decode %v", err)
	}
	var m map[string]string
	_ = json.Unmarshal(creds, &m)
	if m["type"] != "service_account" {
		t.Fatalf("decoded %s", string(creds))
	}
}

func TestPushDeleteByTokens(t *testing.T) {
	db := newPushMockDB()
	srv := newPushTestServer(db, &mockPusher{})
	ctx := context.Background()
	_, _ = srv.pushStore.Upsert(ctx, "account:bob", "dev1-12345678", strings.Repeat("a", 30), "ios")
	_, _ = srv.pushStore.Upsert(ctx, "account:bob", "dev2-12345678", strings.Repeat("b", 30), "android")
	_, _ = srv.pushStore.Upsert(ctx, "account:carol", "dev3-12345678", strings.Repeat("c", 30), "web")
	err := srv.pushStore.DeleteByTokens(ctx, []string{strings.Repeat("a", 30), strings.Repeat("c", 30)})
	if err != nil {
		t.Fatalf("delete %v", err)
	}
	remainingBob, _ := srv.pushStore.ListByAccount(ctx, "account:bob")
	if len(remainingBob) != 1 || remainingBob[0].Token != strings.Repeat("b", 30) {
		t.Fatalf("bob remaining %+v", remainingBob)
	}
	remainingCarol, _ := srv.pushStore.ListByAccount(ctx, "account:carol")
	if len(remainingCarol) != 0 {
		t.Fatalf("carol should be deleted %+v", remainingCarol)
	}
}

func TestInitPusherNoopWhenDisabled(t *testing.T) {
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_BASE64", "")
	cfg := config.Config{FirebaseCredentialsJSON: "", FirebaseCredentialsBase64: ""}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := initPusher(cfg, logger)
	if _, ok := p.(*noopPusher); !ok {
		t.Fatalf("expected noop when disabled got %T", p)
	}
	// Should handle send
	_, err := p.Send(context.Background(), []string{strings.Repeat("t", 30)}, "Title", "Body", nil)
	if err != nil {
		t.Fatalf("noop send err %v", err)
	}
}

func TestIsInvalidTokenError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"registration-token-not-registered", true},
		{"invalid-registration-token", true},
		{"Requested entity was not found. registration-token-not-registered", true},
		{"NotRegistered", true},
		{"invalid-argument: token is invalid", true},
		{"unavailable: internal", false},
		{"quota exceeded", false},
		{"", false},
	}
	for _, c := range cases {
		got := isInvalidTokenError(fmt.Errorf("%s", c.err))
		if got != c.want {
			t.Fatalf("isInvalidTokenError(%q)=%v want %v", c.err, got, c.want)
		}
	}
	if isInvalidTokenError(nil) {
		t.Fatalf("nil should be false")
	}
}
