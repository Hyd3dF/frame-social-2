package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type accountEventState struct {
	changes map[string]uint64
	version uint64
	waiters map[chan struct{}]struct{}
}

type messageEventBroker struct {
	accounts map[string]*accountEventState
	mu       sync.Mutex
}

type messageEventView struct {
	ConversationIDs []string `json:"conversationIds"`
	Resync          bool     `json:"resync"`
	Version         uint64   `json:"version"`
}

func newMessageEventBroker() *messageEventBroker {
	return &messageEventBroker{accounts: make(map[string]*accountEventState)}
}

func (b *messageEventBroker) publish(accounts []string, conversation string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if _, exists := seen[account]; exists {
			continue
		}
		seen[account] = struct{}{}
		state := b.state(account)
		state.version++
		state.changes[conversation] = state.version
		for waiter := range state.waiters {
			close(waiter)
			delete(state.waiters, waiter)
		}
	}
}

func (b *messageEventBroker) wait(ctx context.Context, account string, after uint64) (messageEventView, bool) {
	for {
		b.mu.Lock()
		state := b.state(account)
		if after > state.version {
			view := messageEventView{Resync: true, Version: state.version}
			b.mu.Unlock()
			return view, true
		}
		if state.version > after {
			ids := make([]string, 0, len(state.changes))
			for conversation, version := range state.changes {
				if version > after {
					ids = append(ids, conversation)
				}
			}
			view := messageEventView{ConversationIDs: ids, Version: state.version}
			b.mu.Unlock()
			return view, true
		}
		waiter := make(chan struct{})
		state.waiters[waiter] = struct{}{}
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			b.mu.Lock()
			delete(b.state(account).waiters, waiter)
			version := b.state(account).version
			b.mu.Unlock()
			return messageEventView{Version: version}, false
		case <-waiter:
		}
	}
}

func (b *messageEventBroker) state(account string) *accountEventState {
	state := b.accounts[account]
	if state == nil {
		state = &accountEventState{changes: make(map[string]uint64), waiters: make(map[chan struct{}]struct{})}
		b.accounts[account] = state
	}
	return state
}

func (s *Server) messageEvents(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		respondError(w, http.StatusBadRequest, "invalid_event_cursor", "Senkronizasyon bilgisi geçersiz.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	view, connected := s.events.wait(ctx, accountID(r), after)
	if !connected && r.Context().Err() != nil {
		return
	}
	respondJSON(w, http.StatusOK, view)
}

func (s *Server) messageEventsStream(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		respondError(w, http.StatusBadRequest, "invalid_event_cursor", "Senkronizasyon bilgisi geçersiz.")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "internal_error", "Streaming not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	acct := accountID(r)
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		view, connected := s.events.wait(ctx, acct, after)
		cancel()
		if !connected && r.Context().Err() != nil {
			return
		}
		data, _ := json.Marshal(view)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
		after = view.Version
		if view.Resync {
			return
		}
	}
}

func (s *Server) publishConversation(ctx context.Context, conversation string) {
	if members, ok := s.members.Get(conversation); ok {
		s.events.publish(members, conversation)
		return
	}
	var members []recordID
	if err := s.db.Query(ctx, `SELECT <string>in AS id FROM conversation_member
WHERE out = type::record($conversation) AND left_at IS NONE;`, map[string]any{"conversation": conversation}, &members); err != nil {
		s.log.Error("conversation event members failed", "conversation", conversation, "error", err)
		return
	}
	accounts := make([]string, 0, len(members))
	for _, member := range members {
		accounts = append(accounts, member.ID)
	}
	s.members.Set(conversation, accounts)
	s.events.publish(accounts, conversation)
}
