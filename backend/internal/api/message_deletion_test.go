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

type deletionMockDB struct {
	mu         sync.Mutex
	hidden     map[string]map[string]bool
	members    map[string][]string
	messages   map[string]messageView
	tombstones map[string]string
}

func newDeletionMockDB() *deletionMockDB {
	return &deletionMockDB{
		hidden:     make(map[string]map[string]bool),
		members:    make(map[string][]string),
		messages:   make(map[string]messageView),
		tombstones: make(map[string]string),
	}
}

func (m *deletionMockDB) Ping(context.Context) error { return nil }

func (m *deletionMockDB) Query(_ context.Context, query string, vars map[string]any, dest any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	write := func(value any) {
		if dest == nil {
			return
		}
		encoded, _ := json.Marshal(value)
		_ = json.Unmarshal(encoded, dest)
	}
	if strings.Contains(query, "DEFINE TABLE") {
		return nil
	}
	if strings.Contains(query, "deleted_mode AS deletedMode") {
		message, _ := vars["message"].(string)
		view, ok := m.messages[message]
		if !ok {
			write([]messageDeletionTarget{})
			return nil
		}
		write([]messageDeletionTarget{{Conversation: view.Conversation, Sender: view.SenderID, DeletedMode: m.tombstones[message]}})
		return nil
	}
	if strings.Contains(query, "FROM conversation_member WHERE in = type::record($account) AND out = type::record($conversation)") {
		account, _ := vars["account"].(string)
		conversation, _ := vars["conversation"].(string)
		for _, member := range m.members[conversation] {
			if member == account {
				write([]recordID{{ID: "conversation_member:test"}})
				return nil
			}
		}
		write([]recordID{})
		return nil
	}
	if strings.Contains(query, "RELATE $account_record->message_hidden") {
		account, _ := vars["account"].(string)
		message, _ := vars["message"].(string)
		if m.hidden[account] == nil {
			m.hidden[account] = make(map[string]bool)
		}
		m.hidden[account][message] = true
		return nil
	}
	if strings.Contains(query, "LET $existing = SELECT mode FROM type::record($tombstone)") {
		message, _ := vars["message"].(string)
		mode, _ := vars["mode"].(string)
		if mode == "everyone" || m.tombstones[message] != "everyone" {
			m.tombstones[message] = mode
			if view, ok := m.messages[message]; ok {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				view.Body = nil
				view.Kind = "deleted"
				view.Deleted = true
				view.DeletedAt = &now
				m.messages[message] = view
			}
		}
		return nil
	}
	if strings.Contains(query, "FROM message WHERE conversation = type::record($conversation)") {
		account, _ := vars["account"].(string)
		conversation, _ := vars["conversation"].(string)
		var views []messageView
		for id, view := range m.messages {
			if view.Conversation != conversation || m.hidden[account][id] || m.tombstones[id] == "everyone" {
				continue
			}
			views = append(views, view)
		}
		write(views)
		return nil
	}
	if strings.Contains(query, "FROM privacy_setting") {
		write([]struct {
			Enabled bool `json:"enabled"`
		}{{Enabled: true}})
		return nil
	}
	if strings.Contains(query, "LET $deletion = SELECT mode FROM type::record($tombstone)") {
		message, _ := vars["mid"].(string)
		if m.tombstones[message] == "everyone" {
			return nil
		}
		if _, exists := m.messages[message]; !exists {
			body, _ := vars["body"].(string)
			conversation, _ := vars["conversation"].(string)
			sender, _ := vars["sender"].(string)
			if m.tombstones[message] == "retracted" {
				body = ""
			}
			view := messageView{ID: message, Conversation: conversation, SenderID: sender, Kind: "text"}
			if body != "" {
				view.Body = &body
			} else {
				view.Kind = "deleted"
				view.Deleted = true
			}
			m.messages[message] = view
		}
		return nil
	}
	return nil
}

func newDeletionServer(db *deletionMockDB) (*Server, http.Handler) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{JWTSecret: strings.Repeat("s", 32), AccessTokenMinutes: 15}
	pending := newPendingStore()
	server := &Server{
		cfg:             cfg,
		db:              db,
		events:          newMessageEventBroker(),
		log:             logger,
		members:         newMemberCache(),
		pending:         pending,
		messageDeletion: &messageDeletionStore{db: db},
	}
	server.persist = &persister{db: db, pending: pending}
	return server, server.handler()
}

func deletionRequest(handler http.Handler, method, path, account string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	token, _ := security.AccessToken(strings.Repeat("s", 32), account, 15)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func setDeletionMessage(db *deletionMockDB, server *Server, id, conversation, sender, body string, members ...string) {
	db.members[conversation] = members
	server.members.Set(conversation, members)
	db.messages[id] = messageView{ID: id, Conversation: conversation, SenderID: sender, Body: &body, Kind: "text", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func listDeletionMessages(t *testing.T, handler http.Handler, account, conversation string) []messageView {
	t.Helper()
	response := deletionRequest(handler, http.MethodGet, "/v1/conversations/"+strings.TrimPrefix(conversation, "conversation:")+"/messages", account)
	if response.Code != http.StatusOK {
		t.Fatalf("list messages: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Messages []messageView `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}

func TestMessageDeleteForMeIsPrivateAndIdempotent(t *testing.T) {
	db := newDeletionMockDB()
	server, handler := newDeletionServer(db)
	setDeletionMessage(db, server, "message:one", "conversation:one", "account:alice", "secret", "account:alice", "account:bob")

	for range 2 {
		response := deletionRequest(handler, http.MethodDelete, "/v1/messages/one/for-me", "account:alice")
		if response.Code != http.StatusNoContent {
			t.Fatalf("delete for me: %d %s", response.Code, response.Body.String())
		}
	}
	if !db.hidden["account:alice"]["message:one"] || db.messages["message:one"].Body == nil {
		t.Fatal("delete for me must hide only Alice's view")
	}
	if got := listDeletionMessages(t, handler, "account:alice", "conversation:one"); len(got) != 0 {
		t.Fatalf("Alice still sees %d messages", len(got))
	}
	if got := listDeletionMessages(t, handler, "account:bob", "conversation:one"); len(got) != 1 || got[0].Body == nil {
		t.Fatalf("Bob should retain the message: %#v", got)
	}
	if event, connected := server.events.wait(context.Background(), "account:bob", 0); !connected || len(event.ConversationIDs) != 1 {
		t.Fatalf("delete should publish a conversation event: %#v", event)
	}
}

func TestMessageDeleteForEveryoneRequiresSenderAndHidesMessage(t *testing.T) {
	db := newDeletionMockDB()
	server, handler := newDeletionServer(db)
	setDeletionMessage(db, server, "message:one", "conversation:one", "account:alice", "secret", "account:alice", "account:bob")

	if response := deletionRequest(handler, http.MethodDelete, "/v1/messages/one/for-everyone", "account:bob"); response.Code != http.StatusForbidden {
		t.Fatalf("non-sender deletion: %d %s", response.Code, response.Body.String())
	}
	for range 2 {
		if response := deletionRequest(handler, http.MethodDelete, "/v1/messages/one/for-everyone", "account:alice"); response.Code != http.StatusNoContent {
			t.Fatalf("sender deletion: %d %s", response.Code, response.Body.String())
		}
	}
	if db.messages["message:one"].Body != nil || db.tombstones["message:one"] != "everyone" {
		t.Fatal("global deletion must scrub the body and retain only a tombstone")
	}
	for _, account := range []string{"account:alice", "account:bob"} {
		if got := listDeletionMessages(t, handler, account, "conversation:one"); len(got) != 0 {
			t.Fatalf("globally deleted message returned to %s: %#v", account, got)
		}
	}
}

func TestMessageRetractReturnsSafePlaceholderAndPreservesReplies(t *testing.T) {
	db := newDeletionMockDB()
	server, handler := newDeletionServer(db)
	setDeletionMessage(db, server, "message:one", "conversation:one", "account:alice", "secret", "account:alice", "account:bob")
	replyBody := "reply"
	db.messages["message:reply"] = messageView{ID: "message:reply", Conversation: "conversation:one", SenderID: "account:bob", Body: &replyBody, Kind: "text", ReplyTo: &replyView{ID: "message:one"}}

	if response := deletionRequest(handler, http.MethodPost, "/v1/messages/one/retract", "account:bob"); response.Code != http.StatusForbidden {
		t.Fatalf("non-sender retract: %d", response.Code)
	}
	if response := deletionRequest(handler, http.MethodPost, "/v1/messages/one/retract", "account:alice"); response.Code != http.StatusNoContent {
		t.Fatalf("retract: %d %s", response.Code, response.Body.String())
	}
	messages := listDeletionMessages(t, handler, "account:bob", "conversation:one")
	if len(messages) != 2 || messages[0].ID != "message:one" && messages[1].ID != "message:one" {
		t.Fatalf("retracted record and reply should remain: %#v", messages)
	}
	var retracted messageView
	for _, message := range messages {
		if message.ID == "message:one" {
			retracted = message
		}
	}
	if retracted.Kind != "deleted" || !retracted.Deleted || retracted.Body != nil || retracted.DeletedAt == nil {
		t.Fatalf("unsafe retract response: %#v", retracted)
	}
	if db.messages["message:reply"].ReplyTo == nil {
		t.Fatal("retract must not break replies")
	}
}

func TestMessageDeletionRejectsNonMembersAndPreventsPendingReplay(t *testing.T) {
	db := newDeletionMockDB()
	server, handler := newDeletionServer(db)
	setDeletionMessage(db, server, "message:one", "conversation:one", "account:alice", "secret", "account:alice", "account:bob")
	if response := deletionRequest(handler, http.MethodDelete, "/v1/messages/one/for-me", "account:eve"); response.Code != http.StatusForbidden {
		t.Fatalf("non-member deletion: %d", response.Code)
	}

	pendingBody := "pending secret"
	pending := messageView{ID: "message:pending", Conversation: "conversation:one", SenderID: "account:alice", Body: &pendingBody, Kind: "text"}
	if !server.pending.TryAppend("conversation:one", pending) {
		t.Fatal("set pending message")
	}
	if response := deletionRequest(handler, http.MethodDelete, "/v1/messages/pending/for-everyone", "account:alice"); response.Code != http.StatusNoContent {
		t.Fatalf("pending deletion: %d %s", response.Code, response.Body.String())
	}
	if _, found := server.pending.Find("message:pending"); found {
		t.Fatal("globally deleted pending message remained visible")
	}
	job := newPersistJob("conversation:one", "account:alice", pendingBody, "client-pending", "")
	job.messageID = "message:pending"
	if err := server.persist.DoPersist(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, recreated := db.messages["message:pending"]; recreated {
		t.Fatal("deleted pending message was recreated by persistence")
	}

	retractedBody := "pending retract"
	if !server.pending.TryAppend("conversation:one", messageView{ID: "message:pending-retract", Conversation: "conversation:one", SenderID: "account:alice", Body: &retractedBody, Kind: "text"}) {
		t.Fatal("set pending retraction")
	}
	if response := deletionRequest(handler, http.MethodPost, "/v1/messages/pending-retract/retract", "account:alice"); response.Code != http.StatusNoContent {
		t.Fatalf("pending retract: %d %s", response.Code, response.Body.String())
	}
	view, found := server.pending.Find("message:pending-retract")
	if !found || view.Body != nil || !view.Deleted || view.Kind != "deleted" {
		t.Fatalf("pending retract was not made safe: %#v", view)
	}
	job = newPersistJob("conversation:one", "account:alice", retractedBody, "client-pending-retract", "")
	job.messageID = "message:pending-retract"
	if err := server.persist.DoPersist(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if recreated := db.messages["message:pending-retract"]; recreated.Body != nil || recreated.Kind != "deleted" {
		t.Fatalf("retracted pending message was restored: %#v", recreated)
	}
}

func TestConcurrentMessageDeletionIsIdempotent(t *testing.T) {
	db := newDeletionMockDB()
	server, handler := newDeletionServer(db)
	setDeletionMessage(db, server, "message:one", "conversation:one", "account:alice", "secret", "account:alice", "account:bob")
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			response := deletionRequest(handler, http.MethodPost, "/v1/messages/one/retract", "account:alice")
			if response.Code != http.StatusNoContent {
				t.Errorf("concurrent retract: %d %s", response.Code, response.Body.String())
			}
		}()
	}
	group.Wait()
	message := db.messages["message:one"]
	if message.Body != nil || !message.Deleted || message.Kind != "deleted" {
		t.Fatalf("concurrent retract leaked content: %#v", message)
	}
}
