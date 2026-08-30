package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Rate limiting constants as per spec.
const (
	messageRateLimit    = 50
	messageRateWindow   = 60 * time.Second
	messageRatePenalty  = 5 * time.Minute
	messageDedupTTL     = 24 * time.Hour
	maxRateLimitRetries = 5
	rateStateTable      = "message_rate_state"
	dedupTable          = "message_dedup"
)

type messageRateLimiter interface {
	Check(ctx context.Context, account, clientId string) (allowed bool, isDuplicate bool, retryAfter int, blockedUntil time.Time, err error)
}

type surrealRateLimiter struct {
	db  queryer
	log *slog.Logger
	now func() time.Time
}

type rateStateRow struct {
	ID           string   `json:"id"`
	Account      string   `json:"account"`
	Timestamps   []string `json:"timestamps"`
	BlockedUntil *string  `json:"blockedUntil"`
	Version      int      `json:"version"`
}

func newSurrealRateLimiter(db queryer, log *slog.Logger) messageRateLimiter {
	if db == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	l := &surrealRateLimiter{db: db, log: log}
	// Ensure schema in background, don't block startup.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.ensureSchema(ctx)
	}()
	go l.startCleanup(context.Background())
	return l
}

func (l *surrealRateLimiter) ensureSchema(ctx context.Context) error {
	// Best-effort schema creation. Ignore errors if already exists.
	queries := []string{
		"DEFINE TABLE IF NOT EXISTS message_rate_state SCHEMALESS",
		"DEFINE TABLE IF NOT EXISTS message_dedup SCHEMALESS",
		// Unique index for dedup via record ID is inherent, but we also define field indexes for cleanup queries.
		"DEFINE INDEX IF NOT EXISTS idx_dedup_expires ON TABLE message_dedup FIELDS expires_at",
		"DEFINE INDEX IF NOT EXISTS idx_rate_blocked ON TABLE message_rate_state FIELDS blocked_until",
	}
	for _, q := range queries {
		_ = l.db.Query(ctx, q, nil, nil)
	}
	return nil
}

func (l *surrealRateLimiter) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			// Clean expired dedup entries
			_ = l.db.Query(cctx, "DELETE FROM message_dedup WHERE expires_at < time::now()", nil, nil)
			// Clean empty rate states that are not blocked or blocked expired
			_ = l.db.Query(cctx, "DELETE FROM message_rate_state WHERE (blocked_until IS NONE OR blocked_until < time::now()) AND (timestamps IS NONE OR array::len(timestamps) == 0)", nil, nil)
			// Also clean old timestamps that are outside window? Already pruned on next request, but we can also prune via DB?
			cancel()
		}
	}
}

func rateStateRecordID(account string) string {
	parts := strings.SplitN(account, ":", 2)
	suffix := account
	if len(parts) == 2 {
		suffix = parts[1]
	}
	// Sanitize: keep only alphanum and _- for record ID part
	// Account suffix is expected to be hex/uuid, safe.
	suffix = strings.ReplaceAll(suffix, " ", "_")
	return rateStateTable + ":" + suffix
}

func dedupRecordID(account, clientId string) string {
	suffix := strings.TrimPrefix(account, "account:")
	if suffix == account {
		parts := strings.SplitN(account, ":", 2)
		if len(parts) == 2 {
			suffix = parts[1]
		} else {
			suffix = account
		}
	}
	h := sha256.Sum256([]byte(clientId))
	hash := hex.EncodeToString(h[:])
	// Use full hash to avoid collisions
	return dedupTable + ":" + suffix + "_" + hash
}

func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") || strings.Contains(s, "already contains") || strings.Contains(s, "unique") || strings.Contains(s, "duplicate") || strings.Contains(s, "record already exists")
}

func parseSurrealTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	// Try RFC3339Nano first
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Surreal may return datetime with slightly different format, try time.RFC3339Nano again with fallback
	// Try parsing as time.Time via json unmarshal? As last resort.
	var t time.Time
	if err := json.Unmarshal([]byte(`"`+s+`"`), &t); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unparseable time %q", s)
}

func (l *surrealRateLimiter) Check(ctx context.Context, account, clientId string) (bool, bool, int, time.Time, error) {
	now := time.Now().UTC()
	if l.now != nil {
		now = l.now().UTC()
	}
	window := messageRateWindow
	penalty := messageRatePenalty

	// Idempotency check via dedup table and message table. Must be before counting.
	if clientId != "" {
		dedupID := dedupRecordID(account, clientId)
		var dedupRec []struct {
			ID string `json:"id"`
		}
		err := l.db.Query(ctx, "SELECT <string>id AS id FROM type::record($dedupId) LIMIT 1", map[string]any{"dedupId": dedupID}, &dedupRec)
		if err != nil {
			return false, false, 0, time.Time{}, fmt.Errorf("dedup lookup failed: %w", err)
		}
		if len(dedupRec) > 0 {
			l.log.Debug("message dedup hit", "account", account, "clientIdHash", dedupID)
			return true, true, 0, time.Time{}, nil
		}
		// Also check message table for already persisted messages (covers pre-dedup or cross-instance without dedup)
		var msgDup []recordID
		err = l.db.Query(ctx, "SELECT <string>id AS id FROM message WHERE sender = type::record($account) AND client_id = $clientId LIMIT 1", map[string]any{"account": account, "clientId": clientId}, &msgDup)
		if err != nil {
			return false, false, 0, time.Time{}, fmt.Errorf("message dedup lookup failed: %w", err)
		}
		if len(msgDup) > 0 {
			// Create dedup entry for future fast path (best effort)
			_ = l.db.Query(ctx, "CREATE ONLY type::record($dedupId) CONTENT { account: type::record($account), client_id: $clientId, created_at: <datetime>$now, expires_at: <datetime>$exp }", map[string]any{
				"dedupId":  dedupID,
				"account":  account,
				"clientId": clientId,
				"now":      now.Format(time.RFC3339Nano),
				"exp":      now.Add(messageDedupTTL).Format(time.RFC3339Nano),
			}, nil)
			return true, true, 0, time.Time{}, nil
		}
	}

	stateID := rateStateRecordID(account)

	for attempt := 0; attempt < maxRateLimitRetries; attempt++ {
		// Use request ctx but with timeout to avoid hanging
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var states []rateStateRow
		err := l.db.Query(qctx, "SELECT <string>id AS id, <string>account AS account, timestamps AS timestamps, <string>blocked_until AS blockedUntil, version AS version FROM type::record($id)", map[string]any{"id": stateID}, &states)
		cancel()
		if err != nil {
			return false, false, 0, time.Time{}, fmt.Errorf("rate state fetch failed: %w", err)
		}
		var st *rateStateRow
		if len(states) > 0 {
			st = &states[0]
		}

		// Check if currently blocked
		if st != nil && st.BlockedUntil != nil && *st.BlockedUntil != "" {
			bt, err := parseSurrealTime(*st.BlockedUntil)
			if err == nil && bt.After(now) {
				retry := int(math.Ceil(bt.Sub(now).Seconds()))
				if retry < 1 {
					retry = 1
				}
				l.log.Debug("message rate blocked", "account", account, "blockedUntil", bt, "retryAfter", retry)
				return false, false, retry, bt, nil
			}
			// If blocked_until is parseable but not after now, it's expired -> treat as not blocked and will be cleared below
		}

		// Build filtered timestamps within window
		var filtered []string
		if st != nil && len(st.Timestamps) > 0 {
			for _, tsStr := range st.Timestamps {
				if tsStr == "" {
					continue
				}
				t, err := parseSurrealTime(tsStr)
				if err != nil {
					continue
				}
				if t.After(now.Add(-window)) {
					filtered = append(filtered, tsStr)
				}
			}
		}

		// If blocked expired and filtered empty, we can delete the state to clean up
		if st != nil && st.BlockedUntil != nil && *st.BlockedUntil != "" {
			if bt, err := parseSurrealTime(*st.BlockedUntil); err == nil && !bt.After(now) {
				if len(filtered) == 0 {
					// Clean up empty expired state
					_ = l.db.Query(ctx, "DELETE type::record($id)", map[string]any{"id": stateID}, nil)
					// Treat as no state for this request
					st = nil
					filtered = nil
				}
			}
		}

		// Check if limit exceeded -> trigger penalty
		if len(filtered) >= messageRateLimit {
			blockedUntil := now.Add(penalty)
			blockedStr := blockedUntil.Format(time.RFC3339Nano)
			if st == nil {
				err := l.db.Query(ctx, "CREATE ONLY type::record($id) CONTENT { account: type::record($account), timestamps: $timestamps, blocked_until: <datetime>$blockedUntil, version: 1, updated_at: time::now() }", map[string]any{
					"id":           stateID,
					"account":      account,
					"timestamps":   filtered,
					"blockedUntil": blockedStr,
				}, nil)
				if err != nil {
					if isConflictError(err) {
						// Concurrent create, retry
						continue
					}
					return false, false, 0, time.Time{}, fmt.Errorf("create blocked state failed: %w", err)
				}
				l.log.Warn("message rate penalty triggered", "account", account, "blockedUntil", blockedUntil)
				return false, false, int(penalty.Seconds()), blockedUntil, nil
			}
			// Update existing with CAS on version
			var updated []rateStateRow
			err := l.db.Query(ctx, "UPDATE type::record($id) SET blocked_until = <datetime>$blockedUntil, timestamps = $timestamps, version = $newVersion, updated_at = time::now() WHERE version = $oldVersion RETURN AFTER", map[string]any{
				"id":           stateID,
				"blockedUntil": blockedStr,
				"timestamps":   filtered,
				"newVersion":   st.Version + 1,
				"oldVersion":   st.Version,
			}, &updated)
			if err != nil {
				return false, false, 0, time.Time{}, fmt.Errorf("update blocked state failed: %w", err)
			}
			if len(updated) == 0 {
				// Version conflict, retry
				continue
			}
			l.log.Warn("message rate penalty triggered", "account", account, "blockedUntil", blockedUntil)
			return false, false, int(penalty.Seconds()), blockedUntil, nil
		}

		// Allow: append current timestamp
		newTimestamps := append(filtered, now.Format(time.RFC3339Nano))

		if st == nil {
			err := l.db.Query(ctx, "CREATE ONLY type::record($id) CONTENT { account: type::record($account), timestamps: $timestamps, blocked_until: NONE, version: 1, updated_at: time::now() }", map[string]any{
				"id":         stateID,
				"account":    account,
				"timestamps": newTimestamps,
			}, nil)
			if err != nil {
				if isConflictError(err) {
					continue
				}
				return false, false, 0, time.Time{}, fmt.Errorf("create rate state failed: %w", err)
			}
		} else {
			var updated []rateStateRow
			err := l.db.Query(ctx, "UPDATE type::record($id) SET timestamps = $timestamps, blocked_until = NONE, version = $newVersion, updated_at = time::now() WHERE version = $oldVersion RETURN AFTER", map[string]any{
				"id":         stateID,
				"timestamps": newTimestamps,
				"newVersion": st.Version + 1,
				"oldVersion": st.Version,
			}, &updated)
			if err != nil {
				return false, false, 0, time.Time{}, fmt.Errorf("update rate state failed: %w", err)
			}
			if len(updated) == 0 {
				continue
			}
		}

		// Success: create dedup entry for idempotency
		if clientId != "" {
			dedupID := dedupRecordID(account, clientId)
			_ = l.db.Query(ctx, "CREATE ONLY type::record($dedupId) CONTENT { account: type::record($account), client_id: $clientId, created_at: <datetime>$now, expires_at: <datetime>$exp }", map[string]any{
				"dedupId":  dedupID,
				"account":  account,
				"clientId": clientId,
				"now":      now.Format(time.RFC3339Nano),
				"exp":      now.Add(messageDedupTTL).Format(time.RFC3339Nano),
			}, nil)
		}

		l.log.Debug("message rate allowed", "account", account, "count", len(newTimestamps))
		return true, false, 0, time.Time{}, nil
	}

	return false, false, 0, time.Time{}, fmt.Errorf("rate limit max retries exceeded")
}

func respondRateLimited(w http.ResponseWriter, retryAfter int, blockedUntil time.Time) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":              "message_rate_limited",
			"message":           "Jūs sūtāt ziņojumus pārāk ātri. Lūdzu, uzgaidiet.",
			"retryAfterSeconds": retryAfter,
			"blockedUntil":      blockedUntil.UTC().Format(time.RFC3339Nano),
		},
	})
}
