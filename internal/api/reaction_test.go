package api

import (
	"context"
	"encoding/json"
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

// reactionMockDB simulates DB for reaction tests.
type reactionMockDB struct {
	mu        sync.Mutex
	members   map[string][]string          // conversationID -> accountIDs
	messages  map[string]messageView       // messageID -> messageView (with conversation)
	reactions map[string]map[string]string // messageID -> accountID -> emoji
	sessions  map[string]string
}

func newReactionMockDB() *reactionMockDB {
	return &reactionMockDB{
		members:   make(map[string][]string),
		messages:  make(map[string]messageView),
		reactions: make(map[string]map[string]string),
		sessions:  make(map[string]string),
	}
}

func (m *reactionMockDB) Ping(ctx context.Context) error { return nil }

func (m *reactionMockDB) Query(ctx context.Context, sql string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Handle health ping
	if strings.Contains(sql, "RETURN {ok:true}") {
		return nil
	}

	// Resolve message conversation: SELECT <string>conversation AS conversation FROM type::record($message)
	if strings.Contains(sql, "SELECT <string>conversation AS conversation FROM type::record($message)") {
		msgID, _ := vars["message"].(string)
		// Normalize
		if !strings.HasPrefix(msgID, "message:") {
			msgID = "message:" + msgID
		}
		msg, ok := m.messages[msgID]
		if !ok {
			if dest != nil {
				b, _ := json.Marshal([]struct {
					Conversation string `json:"conversation"`
				}{})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		if dest != nil {
			b, _ := json.Marshal([]struct {
				Conversation string `json:"conversation"`
			}{{Conversation: msg.Conversation}})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// Check membership: SELECT id FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation)
	if strings.Contains(sql, "FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation)") {
		account, _ := vars["account"].(string)
		conv, _ := vars["conversation"].(string)
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		if !strings.HasPrefix(conv, "conversation:") {
			conv = "conversation:" + conv
		}
		members := m.members[conv]
		found := false
		for _, mem := range members {
			if mem == account {
				found = true
				break
			}
		}
		if dest != nil {
			if found {
				b, _ := json.Marshal([]recordID{{ID: "member:1"}})
				_ = json.Unmarshal(b, dest)
			} else {
				b, _ := json.Marshal([]recordID{})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// For getMembersCached: SELECT <string>in AS id FROM conversation_member WHERE out = type::record($conversation)
	if strings.Contains(sql, "SELECT <string>in AS id FROM conversation_member WHERE out = type::record($conversation)") {
		conv, _ := vars["conversation"].(string)
		if !strings.HasPrefix(conv, "conversation:") {
			conv = "conversation:" + conv
		}
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

	// PUT reaction: DELETE then RELATE
	if strings.Contains(sql, "DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message);") && strings.Contains(sql, "RELATE") {
		account, _ := vars["account"].(string)
		message, _ := vars["message"].(string)
		emoji, _ := vars["emoji"].(string)
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		if !strings.HasPrefix(message, "message:") {
			message = "message:" + message
		}
		if _, ok := m.reactions[message]; !ok {
			m.reactions[message] = make(map[string]string)
		}
		// Delete existing (any emoji) for this user
		delete(m.reactions[message], account)
		// Add new
		m.reactions[message][account] = emoji
		return nil
	}

	// DELETE reaction: DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji;
	if strings.Contains(sql, "DELETE message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji") {
		account, _ := vars["account"].(string)
		message, _ := vars["message"].(string)
		emoji, _ := vars["emoji"].(string)
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		if !strings.HasPrefix(message, "message:") {
			message = "message:" + message
		}
		if m.reactions[message] != nil {
			if cur, ok := m.reactions[message][account]; ok && cur == emoji {
				delete(m.reactions[message], account)
			}
		}
		return nil
	}

	// Old PUT check (for backward compat, if still used): IF array::len(SELECT id FROM message_reaction WHERE in = ... AND emoji = $emoji) =0 { RELATE }
	if strings.Contains(sql, "FROM message_reaction WHERE in = type::record($account) AND out = type::record($message) AND emoji = $emoji") && strings.Contains(sql, "RELATE") {
		account, _ := vars["account"].(string)
		message, _ := vars["message"].(string)
		emoji, _ := vars["emoji"].(string)
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		if !strings.HasPrefix(message, "message:") {
			message = "message:" + message
		}
		if _, ok := m.reactions[message]; !ok {
			m.reactions[message] = make(map[string]string)
		}
		if m.reactions[message][account] == emoji {
			// Already exists, do nothing
			return nil
		}
		// For old logic, we would just add, but we want to replace any existing
		m.reactions[message][account] = emoji
		return nil
	}

	// listMessages: SELECT ... FROM message WHERE conversation = type::record($conversation) AND created_at < <datetime>$before
	if strings.Contains(sql, "FROM message WHERE conversation = type::record($conversation)") {
		conv, _ := vars["conversation"].(string)
		account, _ := vars["account"].(string)
		if !strings.HasPrefix(conv, "conversation:") {
			conv = "conversation:" + conv
		}
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		beforeStr, _ := vars["before"].(string)
		limit, _ := vars["limit"].(int)
		var before time.Time
		if beforeStr != "" {
			before, _ = time.Parse(time.RFC3339Nano, beforeStr)
		} else {
			before = time.Now().UTC().Add(time.Hour)
		}
		var filtered []messageView
		for _, msg := range m.messages {
			if msg.Conversation != conv {
				continue
			}
			t, _ := time.Parse(time.RFC3339Nano, msg.CreatedAt)
			if !t.Before(before) {
				continue
			}
			// Build reactions for this message
			var reacts []reactionView
			if reactionsForMsg, ok := m.reactions[msg.ID]; ok {
				// Aggregate by emoji
				countByEmoji := make(map[string]int)
				mineByEmoji := make(map[string]bool)
				for acc, em := range reactionsForMsg {
					countByEmoji[em]++
					if acc == account {
						mineByEmoji[em] = true
					}
				}
				for em, cnt := range countByEmoji {
					reacts = append(reacts, reactionView{Emoji: em, Count: cnt, Mine: mineByEmoji[em]})
				}
			}
			// Copy message with reactions
			copyMsg := msg
			copyMsg.Reactions = reacts
			// Saved and Status are handled elsewhere, keep as is
			filtered = append(filtered, copyMsg)
		}
		// Sort DESC by CreatedAt
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
			if d, ok := dest.(*[]messageView); ok {
				*d = filtered
			} else {
				b, _ := json.Marshal(filtered)
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// For listMessages read_receipts_enabled
	if strings.Contains(sql, "read_receipts_enabled") {
		if dest != nil {
			b, _ := json.Marshal([]struct {
				Enabled bool `json:"enabled"`
			}{{Enabled: true}})
			_ = json.Unmarshal(b, dest)
		}
		return nil
	}

	// For save/unsave/updateReceipt etc., we can just return nil
	if strings.Contains(sql, "FROM message WHERE id = type::record($message)") && strings.Contains(sql, "conversation_member") {
		// This is old canAccessMessage fallback, but we now use resolve + isMember, so not needed
		// Just return true if message exists and member
		msgID, _ := vars["message"].(string)
		account, _ := vars["account"].(string)
		if !strings.HasPrefix(msgID, "message:") {
			msgID = "message:" + msgID
		}
		if !strings.HasPrefix(account, "account:") {
			account = "account:" + account
		}
		msg, ok := m.messages[msgID]
		if !ok {
			if dest != nil {
				b, _ := json.Marshal([]recordID{})
				_ = json.Unmarshal(b, dest)
			}
			return nil
		}
		members := m.members[msg.Conversation]
		found := false
		for _, mem := range members {
			if mem == account {
				found = true
				break
			}
		}
		if dest != nil {
			if found {
				b, _ := json.Marshal([]recordID{{ID: msgID}})
				_ = json.Unmarshal(b, dest)
			} else {
				b, _ := json.Marshal([]recordID{})
				_ = json.Unmarshal(b, dest)
			}
		}
		return nil
	}

	// Fallback for other queries: try to handle SELECT conversation from message
	if strings.Contains(sql, "SELECT <string>conversation AS conversation FROM type::record($message)") {
		// Already handled above, but fallback
		return nil
	}

	// Default: return empty
	if dest != nil {
		b, _ := json.Marshal([]interface{}{})
		_ = json.Unmarshal(b, dest)
	}
	return nil
}

func TestPutReactionIncomingMessage(t *testing.T) {
	db := newReactionMockDB()
	conv := "conversation:7vr5rjry1tchwqlw0afs"
	msgID := "message:fd9d759f7949f4890e2083ec"
	alice := "account:alice"
	bob := "account:bob"
	db.members[conv] = []string{alice, bob}
	body := "hello"
	created := time.Now().UTC().Format(time.RFC3339Nano)
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: alice, Body: &body, ClientID: "client1", Kind: "text", CreatedAt: created, Status: "sent"}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := newMemberCache()
	ps := newPendingStore()
	srv := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger, members: mc, pending: ps}
	srv.persist = newPersister(db, ps, mc, logger)
	readLimiter := newEndpointLimiter(1000, time.Minute, "read", logger)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(readLimiter.middleware(accountKeyFunc("read"), http.HandlerFunc(srv.listMessages))))
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Bob reads conversation history to ensure visibility
	req := httptest.NewRequest("GET", "/v1/conversations/7vr5rjry1tchwqlw0afs/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "7vr5rjry1tchwqlw0afs")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("listMessages %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Messages) != 1 {
		t.Fatalf("expected 1 message got %d", len(listResp.Messages))
	}

	// Bob reacts to incoming message (from Alice)
	req = httptest.NewRequest("PUT", "/v1/messages/fd9d759f7949f4890e2083ec/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "fd9d759f7949f4890e2083ec")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("putReaction incoming expected 204 got %d %s", w.Code, w.Body.String())
	}

	// Verify reaction persisted and visible via listMessages with mine:true
	req = httptest.NewRequest("GET", "/v1/conversations/7vr5rjry1tchwqlw0afs/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "7vr5rjry1tchwqlw0afs")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Messages) != 1 || len(listResp.Messages[0].Reactions) != 1 {
		t.Fatalf("expected 1 reaction got %+v", listResp.Messages[0].Reactions)
	}
	if listResp.Messages[0].Reactions[0].Emoji != "😃" || listResp.Messages[0].Reactions[0].Count != 1 || !listResp.Messages[0].Reactions[0].Mine {
		t.Fatalf("reaction mismatch %+v", listResp.Messages[0].Reactions[0])
	}
}

func TestPutReactionOutgoingMessage(t *testing.T) {
	db := newReactionMockDB()
	conv := "conversation:7vr5rjry1tchwqlw0afs"
	msgID := "message:fd9d759f7949f4890e2083ec"
	alice := "account:alice"
	bob := "account:bob"
	db.members[conv] = []string{alice, bob}
	body := "hello from bob"
	created := time.Now().UTC().Format(time.RFC3339Nano)
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: bob, Body: &body, ClientID: "client1", Kind: "text", CreatedAt: created, Status: "sent"}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	members := newMemberCache()
	pending := newPendingStore()
	srv := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger, members: members, pending: pending}
	srv.persist = newPersister(db, pending, members, logger)

	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	req := httptest.NewRequest("PUT", "/v1/messages/fd9d759f7949f4890e2083ec/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "fd9d759f7949f4890e2083ec")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("putReaction outgoing expected 204 got %d %s", w.Code, w.Body.String())
	}
}

func TestPutReactionReplaceAndRemove(t *testing.T) {
	db := newReactionMockDB()
	conv := "conversation:abc"
	msgID := "message:msg1"
	alice := "account:alice"
	bob := "account:bob"
	db.members[conv] = []string{alice, bob}
	body := "hi"
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: alice, Body: &body, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{cfg: cfg, db: db, events: newMessageEventBroker(), log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	mux.Handle("DELETE /v1/messages/{id}/reactions/{emoji}", srv.requireAuth(http.HandlerFunc(srv.deleteReaction)))
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.listMessages)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))
	// Need to mock listMessages reading, so ensure getMembersCached works - set members via DB mock already
	// Also need to handle that listMessages uses getMembersCached which will query DB mock for members - we have that.

	// Put first emoji
	req := httptest.NewRequest("PUT", "/v1/messages/msg1/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "msg1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("first put %d", w.Code)
	}
	// Replace with different emoji
	req = httptest.NewRequest("PUT", "/v1/messages/msg1/reactions", strings.NewReader(`{"emoji":"😂"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "msg1")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("replace put %d", w.Code)
	}
	// Verify only one reaction, with new emoji
	req = httptest.NewRequest("GET", "/v1/conversations/abc/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "abc")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages) != 1 || len(resp.Messages[0].Reactions) != 1 || resp.Messages[0].Reactions[0].Emoji != "😂" {
		t.Fatalf("expected replaced reaction 😂 got %+v", resp.Messages[0].Reactions)
	}
	// Delete current emoji
	req = httptest.NewRequest("DELETE", "/v1/messages/msg1/reactions/%F0%9F%98%82", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "msg1")
	req.SetPathValue("emoji", "😂")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("delete %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/v1/conversations/abc/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "abc")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages[0].Reactions) != 0 {
		t.Fatalf("expected no reactions after delete got %+v", resp.Messages[0].Reactions)
	}
}

func TestPutReactionNonMember403(t *testing.T) {
	db := newReactionMockDB()
	conv := "conversation:abc"
	msgID := "message:msg1"
	alice := "account:alice"
	bob := "account:bob"
	eve := "account:eve"
	db.members[conv] = []string{alice, bob}
	body := "hi"
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: alice, Body: &body, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenEve, _ := security.AccessToken(cfg.JWTSecret, eve, 15)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{cfg: cfg, db: db, log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	req := httptest.NewRequest("PUT", "/v1/messages/msg1/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenEve)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "msg1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expected 403 got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Fatalf("expected forbidden code %s", w.Body.String())
	}
}

func TestPutReactionNotFound404(t *testing.T) {
	db := newReactionMockDB()
	conv := "conversation:abc"
	alice := "account:alice"
	bob := "account:bob"
	db.members[conv] = []string{alice, bob}
	// No message

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{cfg: cfg, db: db, log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	req := httptest.NewRequest("PUT", "/v1/messages/nonexistent123/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "nonexistent123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404 got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "message_not_found") {
		t.Fatalf("expected message_not_found %s", w.Body.String())
	}
}

func TestPutReactionRawAndPrefixedIDs(t *testing.T) {
	db := newReactionMockDB()
	convRaw := "7vr5rjry1tchwqlw0afs"
	convPrefixed := "conversation:7vr5rjry1tchwqlw0afs"
	msgRaw := "fd9d759f7949f4890e2083ec"
	msgPrefixed := "message:fd9d759f7949f4890e2083ec"
	alice := "account:alice"
	bob := "account:bob"
	db.members[convPrefixed] = []string{alice, bob}
	body := "hello"
	created := time.Now().UTC().Format(time.RFC3339Nano)
	db.messages[msgPrefixed] = messageView{ID: msgPrefixed, Conversation: convPrefixed, SenderID: alice, Body: &body, CreatedAt: created}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{cfg: cfg, db: db, log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.listMessages)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// Test raw ID
	req := httptest.NewRequest("PUT", "/v1/messages/"+msgRaw+"/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", msgRaw)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("raw id expected 204 got %d %s", w.Code, w.Body.String())
	}
	// Test prefixed ID
	req = httptest.NewRequest("PUT", "/v1/messages/"+msgPrefixed+"/reactions", strings.NewReader(`{"emoji":"😂"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", msgPrefixed)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("prefixed id expected 204 got %d %s", w.Code, w.Body.String())
	}
	// Verify via raw conversation ID
	req = httptest.NewRequest("GET", "/v1/conversations/"+convRaw+"/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", convRaw)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list raw conv %d", w.Code)
	}
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages) == 0 || len(resp.Messages[0].Reactions) == 0 {
		t.Fatalf("expected reaction via raw conv")
	}
	// Verify via prefixed conversation ID
	req = httptest.NewRequest("GET", "/v1/conversations/"+convPrefixed+"/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", convPrefixed)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list prefixed conv %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages) == 0 || len(resp.Messages[0].Reactions) == 0 {
		t.Fatalf("expected reaction via prefixed conv")
	}
	// Both should be same
}

func TestProductionReactionRequest(t *testing.T) {
	// Reproduce exact production request
	db := newReactionMockDB()
	conv := "conversation:7vr5rjry1tchwqlw0afs"
	msgID := "message:fd9d759f7949f4890e2083ec"
	alice := "account:alice"
	bob := "account:bob"
	// Simulate that alice is the actual oroto31 owner, bob is the tester? But we need membership: both are members
	db.members[conv] = []string{alice, bob}
	body := "test"
	created := time.Now().UTC().Format(time.RFC3339Nano)
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: alice, Body: &body, CreatedAt: created}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	// Bob is the one reacting (incoming message from alice)
	tokenBob, _ := security.AccessToken(cfg.JWTSecret, bob, 15)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{cfg: cfg, db: db, log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	mux.Handle("POST /v1/conversations/{id}/read", srv.requireAuth(http.HandlerFunc(srv.readConversation)))
	mux.Handle("GET /v1/conversations/{id}/messages", srv.requireAuth(http.HandlerFunc(srv.listMessages)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	// First, mark as read should succeed (proves membership works for read)
	req := httptest.NewRequest("POST", "/v1/conversations/7vr5rjry1tchwqlw0afs/read", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "7vr5rjry1tchwqlw0afs")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	// read may return 204 or 500 if DB mock not handling receipts, but should not be 403
	if w.Code == 403 {
		t.Fatalf("read should not be 403, got %d %s", w.Code, w.Body.String())
	}

	// Now exact reaction request from production
	req = httptest.NewRequest("PUT", "/v1/messages/fd9d759f7949f4890e2083ec/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "fd9d759f7949f4890e2083ec")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("production reaction expected 204 got %d %s", w.Code, w.Body.String())
	}

	// Verify persisted and visible
	req = httptest.NewRequest("GET", "/v1/conversations/7vr5rjry1tchwqlw0afs/messages?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.SetPathValue("id", "7vr5rjry1tchwqlw0afs")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := false
	for _, m := range resp.Messages {
		if m.ID == msgID {
			found = true
			if len(m.Reactions) != 1 || !m.Reactions[0].Mine || m.Reactions[0].Count != 1 {
				t.Fatalf("reaction not persisted correctly %+v", m.Reactions)
			}
		}
	}
	if !found {
		t.Fatal("message not found after reaction")
	}
}

func TestReactionDoesNotLogTokens(t *testing.T) {
	// Ensure that logging for 403/404 does not contain tokens
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	db := newReactionMockDB()
	conv := "conversation:abc"
	msgID := "message:msg1"
	alice := "account:alice"
	eve := "account:eve"
	db.members[conv] = []string{alice}
	body := "hi"
	db.messages[msgID] = messageView{ID: msgID, Conversation: conv, SenderID: alice, Body: &body, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}

	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	tokenEve, _ := security.AccessToken(cfg.JWTSecret, eve, 15)
	// Create a refresh token to ensure it's not logged
	refresh, _ := security.RefreshToken()

	srv := &Server{cfg: cfg, db: db, log: logger, members: newMemberCache(), pending: newPendingStore()}
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/messages/{id}/reactions", srv.requireAuth(http.HandlerFunc(srv.putReaction)))
	handler := recoverer(logger, securityHeaders(requestIDMiddleware(mux)))

	req := httptest.NewRequest("PUT", "/v1/messages/msg1/reactions", strings.NewReader(`{"emoji":"😃"}`))
	req.Header.Set("Authorization", "Bearer "+tokenEve)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "msg1")
	// Add a fake refresh token header to try to trick logging
	req.Header.Set("X-Refresh-Token", refresh)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	logged := buf.String()
	if strings.Contains(logged, tokenEve) || strings.Contains(logged, refresh) {
		t.Fatalf("log should not contain tokens, got %s", logged)
	}
	if !strings.Contains(logged, "account_id") || !strings.Contains(logged, "message_id") {
		// Should contain structured fields
		t.Logf("log output: %s", logged)
	}
}
