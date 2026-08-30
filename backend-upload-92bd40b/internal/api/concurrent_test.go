package api

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrentMessageStorm(t *testing.T) {
	mock := &mockQueryer{
		onQuery: func(sql string, vars map[string]any, dest any) error { return nil },
	}
	srv := &Server{
		db:      mock,
		events:  newMessageEventBroker(),
		members: newMemberCache(),
		pending: newPendingStore(),
	}
	srv.members.Set("conversation:abc", []string{"account:alice", "account:bob"})
	const N = 300
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan string, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			job := newPersistJob("conversation:abc", "account:alice", fmt.Sprintf("msg %d", i), fmt.Sprintf("client-%08d", i), "")
			view := messageView{
				ID: job.messageID, ClientID: job.clientID, Conversation: job.conversation,
				SenderID: job.sender, Body: &job.body, Kind: "text", CreatedAt: job.createdAt.Format(time.RFC3339Nano), Status: "sent",
			}
			srv.pending.Append("conversation:abc", view)
			srv.events.publish([]string{"account:alice", "account:bob"}, "conversation:abc")
			if view.ID == "" {
				errs <- "empty id"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent error: %s", e)
	}
	if len(srv.pending.List("conversation:abc")) != N {
		t.Fatalf("expected %d pending, got %d", N, len(srv.pending.List("conversation:abc")))
	}
}

func TestBrokerConcurrentWakes(t *testing.T) {
	b := newMessageEventBroker()
	const waiters = 100
	done := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			b.wait(ctx, "account:x", 0)
			done <- struct{}{}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	b.publish([]string{"account:x"}, "conversation:1")
	timeout := time.After(2 * time.Second)
	received := 0
	for received < waiters {
		select {
		case <-done:
			received++
		case <-timeout:
			t.Fatalf("only %d/%d waiters woken", received, waiters)
		}
	}
}

func TestMemberCacheConcurrent(t *testing.T) {
	c := newMemberCache()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Set("conv:1", []string{"a", "b"})
		}()
		go func() {
			defer wg.Done()
			c.Get("conv:1")
			c.IsMember("conv:1", "a")
		}()
	}
	wg.Wait()
}
