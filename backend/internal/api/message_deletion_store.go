package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type messageDeletionStore struct {
	db queryer
}

type messageDeletionTarget struct {
	Conversation string `json:"conversation"`
	DeletedMode  string `json:"deletedMode"`
	Sender       string `json:"sender"`
}

func newMessageDeletionStore(db queryer) *messageDeletionStore {
	store := &messageDeletionStore{db: db}
	if db != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = store.ensureSchema(ctx)
		}()
	}
	return store
}

func (s *messageDeletionStore) ensureSchema(ctx context.Context) error {
	for _, query := range []string{
		"DEFINE TABLE IF NOT EXISTS message_hidden SCHEMALESS",
		"DEFINE TABLE IF NOT EXISTS message_tombstone SCHEMALESS",
	} {
		if err := s.db.Query(ctx, query, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *messageDeletionStore) lookup(ctx context.Context, message string) (messageDeletionTarget, bool, error) {
	var targets []messageDeletionTarget
	err := s.db.Query(ctx, `SELECT <string>conversation AS conversation, deleted_mode AS deletedMode, <string>sender AS sender
FROM type::record($message) LIMIT 1;`, map[string]any{"message": message}, &targets)
	if err != nil {
		return messageDeletionTarget{}, false, err
	}
	if len(targets) == 0 {
		return messageDeletionTarget{}, false, nil
	}
	return targets[0], true, nil
}

func (s *messageDeletionStore) hide(ctx context.Context, account, message string) error {
	return s.db.Query(ctx, `BEGIN TRANSACTION;
LET $existing = SELECT id FROM message_hidden WHERE in = type::record($account) AND out = type::record($message) LIMIT 1;
IF array::len($existing) = 0 {
 LET $account_record = type::record($account); LET $message_record = type::record($message);
 RELATE $account_record->message_hidden->$message_record CONTENT { created_at: time::now() };
};
COMMIT TRANSACTION;`, map[string]any{"account": account, "message": message}, nil)
}

func (s *messageDeletionStore) mark(ctx context.Context, account, message, mode string) error {
	return s.db.Query(ctx, `BEGIN TRANSACTION;
LET $existing = SELECT mode FROM type::record($tombstone) LIMIT 1;
IF array::len($existing) = 0 {
 CREATE ONLY type::record($tombstone) CONTENT {
  message: type::record($message), mode: $mode, deleted_by: type::record($account), created_at: time::now()
 };
};
IF array::len($existing) > 0 AND $mode = 'everyone' {
 UPDATE type::record($tombstone) SET mode = 'everyone', deleted_by = type::record($account), updated_at = time::now();
};
IF $mode = 'everyone' OR array::len($existing) = 0 OR $existing[0].mode != 'everyone' {
 UPDATE type::record($message) SET body = NONE, kind = 'deleted', deleted_at = deleted_at ?? time::now(),
  deleted_by = type::record($account), deleted_mode = $mode
 WHERE sender = type::record($account);
};
COMMIT TRANSACTION;`, map[string]any{
		"account": account, "message": message, "mode": mode, "tombstone": messageTombstoneID(message),
	}, nil)
}

func messageTombstoneID(message string) string {
	hash := sha256.Sum256([]byte(message))
	return "message_tombstone:" + hex.EncodeToString(hash[:])
}
