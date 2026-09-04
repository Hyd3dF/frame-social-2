package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/config"
	"github.com/Hyd3dF/frame-social-2/internal/security"
)

// deployedSurrealDB emulates the production server from logs.1788454942840.json:
// bare `RETURN AFTER` parses, projection `RETURN AFTER { ... }` fails with the
// exact -32000 parse error, and UPDATE returns full snake_case documents.
type deployedSurrealDB struct {
	mu       sync.Mutex
	sessions map[string]deployedSession // refreshHash -> session
	lastSQL  string
	asObject bool // return account as {"tb":"account","id":...} instead of string
}

type deployedSession struct {
	account   string
	expiresAt string
	revoked   bool
}

func (m *deployedSurrealDB) Ping(ctx context.Context) error { return nil }

func (m *deployedSurrealDB) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSQL = sql
	if strings.Contains(sql, "RETURN AFTER {") {
		return fmt.Errorf("surreal RPC -32000: Parse error: Unexpected token `{`, expected Eof\n --> [4:14]\n  |\n4 | RETURN AFTER { account: <string>account, expiresAt: <string>expires_at };\n  |              ^\n")
	}
	if strings.Contains(sql, "UPDATE auth_session SET") && strings.Contains(sql, "refresh_token_hash = $new_hash") {
		oldHash, _ := vars["old_hash"].(string)
		newHash, _ := vars["new_hash"].(string)
		sess, ok := m.sessions[oldHash]
		if !ok || sess.revoked || sess.expiresAt == "" {
			setDest(dest, `[]`)
			return nil
		}
		if exp, err := time.Parse(time.RFC3339Nano, sess.expiresAt); err == nil && !exp.After(time.Now().UTC()) {
			setDest(dest, `[]`)
			return nil
		} else if err != nil {
			if exp2, err2 := time.Parse(time.RFC3339, sess.expiresAt); err2 == nil && !exp2.After(time.Now().UTC()) {
				setDest(dest, `[]`)
				return nil
			}
		}
		delete(m.sessions, oldHash)
		m.sessions[newHash] = sess
		// Full-document shape like bare RETURN AFTER returns, incl. extra cols.
		if m.asObject {
			id := strings.TrimPrefix(sess.account, "account:")
			setDest(dest, fmt.Sprintf(`[{"id":"auth_session:x","account":{"tb":"account","id":%q},"expires_at":%q,"refresh_token_hash":%q,"revoked_at":null}]`, id, sess.expiresAt, newHash))
			return nil
		}
		setDest(dest, fmt.Sprintf(`[{"id":"auth_session:x","account":%q,"expires_at":%q,"refresh_token_hash":%q,"revoked_at":null}]`, sess.account, sess.expiresAt, newHash))
		return nil
	}
	setDest(dest, `[]`)
	return nil
}

func setDest(dest any, raw string) {
	if dest == nil {
		return
	}
	_ = json.Unmarshal([]byte(raw), dest)
}

func refreshTestServer(db *deployedSurrealDB) *Server {
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), OTPPepper: strings.Repeat("p", 32), AccessTokenMinutes: 15, RefreshTokenDays: 30, OTPMode: "development"}
	return &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: slogDiscard(), members: newMemberCache(), pending: newPendingStore()}
}

func TestRefreshUsesDeployedCompatibleSyntax(t *testing.T) {
	db := &deployedSurrealDB{sessions: map[string]deployedSession{}}
	srv := refreshTestServer(db)
	refresh, _ := security.RefreshToken()
	db.sessions[security.TokenHash(refresh)] = deployedSession{account: "account:u1", expiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":%q}`, refresh)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.refresh(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 got %d body %s (sql %q)", w.Code, w.Body.String(), db.lastSQL)
	}
	if strings.Contains(db.lastSQL, "RETURN AFTER {") {
		t.Fatalf("refresh still uses projection syntax rejected by deployed SurrealDB: %q", db.lastSQL)
	}
	if !strings.Contains(db.lastSQL, "RETURN AFTER") {
		t.Fatalf("refresh must keep bare RETURN AFTER for atomic rotation: %q", db.lastSQL)
	}
}

func TestRefreshRotationHappyPathSnakeCase(t *testing.T) {
	db := &deployedSurrealDB{sessions: map[string]deployedSession{}}
	srv := refreshTestServer(db)
	refresh, _ := security.RefreshToken()
	db.sessions[security.TokenHash(refresh)] = deployedSession{account: "account:u1", expiresAt: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)}
	req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":%q}`, refresh)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.refresh(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 got %d %s", w.Code, w.Body.String())
	}
	var tokens security.Tokens
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.RefreshToken == refresh {
		t.Fatalf("rotation must mint fresh tokens, got %+v", tokens)
	}
	if tokens.RefreshTokenExpiresAt == "" {
		t.Fatal("missing refresh expiry")
	}
	if got, _ := security.ParseAccessToken(strings.Repeat("s", 32), tokens.AccessToken); got != "account:u1" {
		t.Fatalf("access token subject %q", got)
	}
	if strings.Contains(w.Body.String(), security.TokenHash(refresh)) {
		t.Fatal("response must not expose token hashes")
	}
	// Old token is single-use.
	req = httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":%q}`, refresh)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.refresh(w, req)
	if w.Code != 401 {
		t.Fatalf("reused refresh token must be 401, got %d %s", w.Code, w.Body.String())
	}
	// New token still works (proves atomic swap stored new hash).
	req = httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":%q}`, tokens.RefreshToken)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.refresh(w, req)
	if w.Code != 200 {
		t.Fatalf("rotated token must work, got %d %s", w.Code, w.Body.String())
	}
}

func TestRefreshHandlesRecordObjectShape(t *testing.T) {
	db := &deployedSurrealDB{sessions: map[string]deployedSession{}, asObject: true}
	srv := refreshTestServer(db)
	refresh, _ := security.RefreshToken()
	db.sessions[security.TokenHash(refresh)] = deployedSession{account: "account:u1", expiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(fmt.Sprintf(`{"refreshToken":%q}`, refresh)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.refresh(w, req)
	if w.Code != 200 {
		t.Fatalf("object-shaped account must decode, got %d %s", w.Code, w.Body.String())
	}
}

func TestRefreshInvalidTokenIs401Not500(t *testing.T) {
	db := &deployedSurrealDB{sessions: map[string]deployedSession{}}
	srv := refreshTestServer(db)
	for _, body := range []string{`{"refreshToken":"bogus"}`, `{"refreshToken":""}`} {
		req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.refresh(w, req)
		if strings.Contains(body, "bogus") && w.Code != 401 {
			t.Fatalf("unknown token must be 401, got %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(body, `""`) && w.Code != 400 {
			t.Fatalf("empty token must be 400, got %d %s", w.Code, w.Body.String())
		}
		var envelope errorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code == "" {
			t.Fatalf("errors must use safe envelope, got %q err %v", w.Body.String(), err)
		}
	}
}

func TestRefreshErrorEnvelopeNeverLeaksHash(t *testing.T) {
	db := &deployedSurrealDB{sessions: map[string]deployedSession{}}
	srv := refreshTestServer(db)
	req := httptest.NewRequest("POST", "/v1/auth/refresh", strings.NewReader(`{"refreshToken":"bogus"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.refresh(w, req)
	if strings.Contains(w.Body.String(), "refresh_token_hash") || strings.Contains(w.Body.String(), "surreal") {
		t.Fatalf("error must not leak internals: %s", w.Body.String())
	}
}
