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
	for _, fragment := range []string{"BEGIN TRANSACTION", "SELECT * FROM type::record($mid)", "CREATE ONLY $mid CONTENT", "COMMIT TRANSACTION"} {
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
