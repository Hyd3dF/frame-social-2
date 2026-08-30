package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

type persistJob struct {
	conversation string
	sender       string
	body         string
	clientID     string
	replyToID    string
	createdAt    time.Time
	messageID    string
}

func newMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "message:" + hex.EncodeToString(b)
}

func newPersistJob(conversation, sender, body, clientID, replyToID string) persistJob {
	return persistJob{
		conversation: conversation,
		sender:       sender,
		body:         body,
		clientID:     clientID,
		replyToID:    replyToID,
		createdAt:    time.Now().UTC(),
		messageID:    newMessageID(),
	}
}

type persister struct {
	ch      chan persistJob
	db      queryer
	pending *pendingStore
	cache   *memberCache
	log     *slog.Logger
}

func newPersister(db queryer, pending *pendingStore, cache *memberCache, log *slog.Logger) *persister {
	p := &persister{
		ch:      make(chan persistJob, 10000),
		db:      db,
		pending: pending,
		cache:   cache,
		log:     log,
	}
	go p.loop()
	return p
}

func (p *persister) enqueue(j persistJob) {
	select {
	case p.ch <- j:
	default:
		p.log.Error("persist queue full, dropping job", "conversation", j.conversation, "clientId", j.clientID)
	}
}

func (p *persister) loop() {
	for job := range p.ch {
		p.persistWithRetry(job)
	}
}

func (p *persister) persistWithRetry(job persistJob) {
	backoff := 200 * time.Millisecond
	const maxBackoff = 10 * time.Second
	for attempt := 0; attempt < 12; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.doPersist(ctx, job)
		cancel()
		if err == nil {
			return
		}
		p.log.Error("persist failed, retrying", "attempt", attempt+1, "conversation", job.conversation, "error", err)
		select {
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	p.log.Error("persist permanently failed, keeping in pending store for client retry", "conversation", job.conversation, "clientId", job.clientID)
}

func (p *persister) DoPersist(ctx context.Context, job persistJob) error {
	return p.doPersist(ctx, job)
}

func (p *persister) doPersist(ctx context.Context, job persistJob) error {
	createdAtStr := job.createdAt.Format(time.RFC3339Nano)
	err := p.db.Query(ctx, `
LET $msg = CREATE $mid CONTENT {
 conversation: type::record($conversation), sender: type::record($sender),
 client_id: $client_id, body: $body, kind: 'text', created_at: <datetime>$createdAt
};
IF $has_reply {
 LET $orig = SELECT * FROM type::record($reply_to) WHERE conversation = type::record($conversation) LIMIT 1;
 IF array::len($orig) > 0 {
  LET $orig_rec = $orig[0];
  RELATE $orig_rec->message_reply->$msg CONTENT { replied_by: type::record($sender) };
 };
};
LET $recipients = SELECT VALUE in FROM conversation_member WHERE out = type::record($conversation) AND in != type::record($sender) AND left_at IS NONE;
FOR $r IN $recipients {
 CREATE message_receipt CONTENT { message: $msg.id, conversation: type::record($conversation), recipient: $r, status: 'sent' };
};
UPDATE type::record($conversation) SET last_message = $msg.id, updated_at = time::now();
`, map[string]any{
		"mid":          job.messageID,
		"conversation": job.conversation,
		"sender":       job.sender,
		"client_id":    job.clientID,
		"body":         job.body,
		"createdAt":    createdAtStr,
		"reply_to":     job.replyToID,
		"has_reply":    job.replyToID != "",
	}, nil)
	if err != nil {
		return err
	}
	p.pending.Remove(job.conversation, job.messageID)
	return nil
}
