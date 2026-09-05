package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
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
	ch            chan persistJob
	db            queryer
	pending       *pendingStore
	log           *slog.Logger
	lastCreatedAt time.Time
	mu            sync.Mutex
}

func newPersister(db queryer, pending *pendingStore, _ *memberCache, log *slog.Logger) *persister {
	if log == nil {
		log = slog.Default()
	}
	p := &persister{
		ch:      make(chan persistJob, 10000),
		db:      db,
		pending: pending,
		log:     log,
	}
	go p.loop()
	return p
}

// accept reserves both RAM stores together, so every accepted message is
// immediately visible and has a durable-write slot in the same order.
func (p *persister) accept(j *persistJob, view *messageView) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()
	if !now.After(p.lastCreatedAt) {
		now = p.lastCreatedAt.Add(time.Nanosecond)
	}
	j.createdAt = now
	view.CreatedAt = now.Format(time.RFC3339Nano)
	if len(p.ch) == cap(p.ch) || !p.pending.TryAppend(j.conversation, *view) {
		return false
	}
	select {
	case p.ch <- *j:
		p.lastCreatedAt = now
		return true
	default:
		p.pending.Remove(j.conversation, j.messageID)
		return false
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
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.doPersist(ctx, job)
		cancel()
		if err == nil {
			return
		}
		p.log.Error("persist failed, retrying", "attempt", attempt, "conversation", job.conversation, "message", job.messageID, "error", err)
		select {
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (p *persister) DoPersist(ctx context.Context, job persistJob) error {
	return p.doPersist(ctx, job)
}

func (p *persister) doPersist(ctx context.Context, job persistJob) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	createdAtStr := job.createdAt.Format(time.RFC3339Nano)
	err := p.db.Query(ctx, `
BEGIN TRANSACTION;
LET $deletion = SELECT mode FROM type::record($tombstone) LIMIT 1;
LET $retracted = array::len($deletion) > 0 AND $deletion[0].mode = 'retracted';
LET $removed = array::len($deletion) > 0 AND $deletion[0].mode = 'everyone';
IF $removed = false {
LET $existing = SELECT * FROM type::record($mid) LIMIT 1;
IF array::len($existing) = 0 {
 LET $msg = CREATE ONLY type::record($mid) CONTENT {
   conversation: type::record($conversation), sender: type::record($sender),
   client_id: <string>$client_id, body: IF $retracted THEN NONE ELSE $body END,
   kind: IF $retracted THEN 'deleted' ELSE 'text' END, created_at: <datetime>$createdAt,
   deleted_at: IF $retracted THEN time::now() ELSE NONE END,
   deleted_mode: IF $retracted THEN 'retracted' ELSE NONE END
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
};
};
COMMIT TRANSACTION;
`, map[string]any{
		"mid":          job.messageID,
		"conversation": job.conversation,
		"sender":       job.sender,
		"client_id":    job.clientID,
		"body":         job.body,
		"createdAt":    createdAtStr,
		"reply_to":     job.replyToID,
		"has_reply":    job.replyToID != "",
		"tombstone":    messageTombstoneID(job.messageID),
	}, nil)
	if err != nil {
		return err
	}
	p.pending.Remove(job.conversation, job.messageID)
	return nil
}
