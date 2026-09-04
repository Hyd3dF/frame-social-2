package api

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCleanMessageBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "object replacement character", input: "Ne yapıyon aslan\uFFFC", want: "Ne yapıyon aslan"},
		{name: "zero width no break space", input: "mer\uFEFFhaba", want: "merhaba"},
		{name: "control characters", input: "a\u0000b\u0007c", want: "abc"},
		{name: "newlines and tabs", input: "  ilk\n\tikinci  ", want: "ilk\n\tikinci"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanMessageBody(test.input); got != test.want {
				t.Fatalf("cleanMessageBody() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPendingStoreRejectsWithoutEvictingAcceptedMessages(t *testing.T) {
	store := newPendingStore()
	store.limit = 2
	first := messageView{ID: "message:first"}
	second := messageView{ID: "message:second"}
	if !store.TryAppend("conversation:one", first) || !store.TryAppend("conversation:one", second) {
		t.Fatal("accepted messages should fit in the pending store")
	}
	if store.TryAppend("conversation:one", messageView{ID: "message:third"}) {
		t.Fatal("a full pending store must reject rather than evict an accepted message")
	}
	got := store.List("conversation:one")
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("pending messages changed after saturation: %#v", got)
	}
}

func TestPersistUsesIdempotentTransaction(t *testing.T) {
	var query string
	p := &persister{
		db: &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error {
			query = sql
			return nil
		}},
		pending: newPendingStore(),
	}
	if err := p.DoPersist(context.Background(), newPersistJob("conversation:one", "account:one", "body", "client-12345678", "")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	for _, fragment := range []string{"BEGIN TRANSACTION", "SELECT * FROM type::record($mid)", "CREATE ONLY type::record($mid) CONTENT", "COMMIT TRANSACTION"} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("persist query missing %q", fragment)
		}
	}
}

func TestPersistAdmissionOrdersMessageTimestamps(t *testing.T) {
	p := &persister{ch: make(chan persistJob, 2), pending: newPendingStore()}
	first := newPersistJob("conversation:one", "account:one", "first", "client-first", "")
	second := newPersistJob("conversation:one", "account:one", "second", "client-second", "")
	first.createdAt = time.Time{}
	second.createdAt = time.Time{}
	firstView := messageView{ID: first.messageID}
	secondView := messageView{ID: second.messageID}
	if !p.accept(&first, &firstView) || !p.accept(&second, &secondView) {
		t.Fatal("message admission failed")
	}
	if !second.createdAt.After(first.createdAt) || secondView.CreatedAt <= firstView.CreatedAt {
		t.Fatalf("timestamps are not ordered: %s then %s", firstView.CreatedAt, secondView.CreatedAt)
	}
}

// TestPersistUsesDeployedCompatibleCreate replays the production outage where
// every persist failed: the deployed SurrealDB rejects CREATE with a bare
// string variable (CREATE ONLY $mid) — record IDs must go through
// type::record($mid), the idiom used everywhere else in this codebase.
func TestPersistUsesDeployedCompatibleCreate(t *testing.T) {
	p := &persister{
		db: &mockQueryer{onQuery: func(sql string, vars map[string]any, dest any) error {
			// Emulate the deployed parser: a bare $variable in CREATE
			// position is not a table/record reference.
			for _, line := range strings.Split(sql, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "CREATE ONLY $") || strings.HasPrefix(trimmed, "CREATE $") {
					t.Errorf("persist query rejected by deployed SurrealDB: %q", trimmed)
					return context.DeadlineExceeded // any error aborts DoPersist
				}
			}
			return nil
		}},
		pending: newPendingStore(),
	}
	job := newPersistJob("conversation:one", "account:one", "body", "client-12345678", "")
	if !p.pending.TryAppend(job.conversation, messageView{ID: job.messageID}) {
		t.Fatal("setup: pending append failed")
	}
	if err := p.DoPersist(context.Background(), job); err != nil {
		t.Fatalf("persist against deployed parser rules: %v", err)
	}
	if got := p.pending.List("conversation:one"); len(got) != 0 {
		t.Fatalf("persisted job must leave pending, got %d", len(got))
	}
}
