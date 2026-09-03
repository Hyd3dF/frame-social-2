package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type pushDevice struct {
	ID        string `json:"id"`
	Account   string `json:"account"`
	Token     string `json:"token"`
	Platform  string `json:"platform"`
	DeviceID  string `json:"deviceId"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// pushStore manages SurrealDB push_device table.
type pushStore struct {
	db  queryer
	log *slog.Logger
}

func newPushStore(db queryer, log *slog.Logger) *pushStore {
	if log == nil {
		log = slog.Default()
	}
	ps := &pushStore{db: db, log: log}
	if db != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = ps.ensureSchema(ctx)
		}()
	}
	return ps
}

func (s *pushStore) ensureSchema(ctx context.Context) error {
	queries := []string{
		"DEFINE TABLE IF NOT EXISTS push_device SCHEMALESS",
		"DEFINE INDEX IF NOT EXISTS idx_push_device_account_device ON TABLE push_device FIELDS account, device_id UNIQUE",
		"DEFINE INDEX IF NOT EXISTS idx_push_device_token ON TABLE push_device FIELDS token",
	}
	for _, q := range queries {
		_ = s.db.Query(ctx, q, nil, nil)
	}
	return nil
}

// Upsert creates or updates a device by (account, device_id).
func (s *pushStore) Upsert(ctx context.Context, account, deviceID, token, platform string) (*pushDevice, error) {
	account = normalizeRecordID(account, "account")
	if !validRecord(account, "account") {
		return nil, errInvalidAccount
	}
	deviceID = strings.TrimSpace(deviceID)
	token = strings.TrimSpace(token)
	platform = strings.ToLower(strings.TrimSpace(platform))

	// Check existing
	var existing []pushDevice
	err := s.db.Query(ctx, `SELECT <string>id AS id, <string>account AS account, token, platform, device_id AS deviceId, <string>created_at AS createdAt, <string>updated_at AS updatedAt FROM push_device WHERE account = type::record($account) AND device_id = $deviceId LIMIT 1`, map[string]any{
		"account":  account,
		"deviceId": deviceID,
	}, &existing)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		id := existing[0].ID
		var updated []pushDevice
		err = s.db.Query(ctx, `UPDATE type::record($id) SET token = $fcmToken, platform = $platform, updated_at = time::now() RETURN AFTER`, map[string]any{
			"id":       id,
			"fcmToken": token,
			"platform": platform,
		}, &updated)
		if err != nil {
			return nil, err
		}
		if len(updated) > 0 {
			return &updated[0], nil
		}
		// Fallback fetch
		var refetched []pushDevice
		_ = s.db.Query(ctx, `SELECT <string>id AS id, <string>account AS account, token, platform, device_id AS deviceId, <string>created_at AS createdAt, <string>updated_at AS updatedAt FROM push_device WHERE account = type::record($account) AND device_id = $deviceId LIMIT 1`, map[string]any{
			"account":  account,
			"deviceId": deviceID,
		}, &refetched)
		if len(refetched) > 0 {
			return &refetched[0], nil
		}
		return &existing[0], nil
	}
	// Create
	var created []pushDevice
	err = s.db.Query(ctx, `CREATE push_device CONTENT { account: type::record($account), token: $fcmToken, platform: $platform, device_id: $deviceId, created_at: time::now(), updated_at: time::now() } RETURN AFTER`, map[string]any{
		"account":  account,
		"deviceId": deviceID,
		"fcmToken": token,
		"platform": platform,
	}, &created)
	if err != nil {
		// Handle race: if unique violation, try update again
		if isConflictError(err) {
			var retry []pushDevice
			err2 := s.db.Query(ctx, `SELECT <string>id AS id FROM push_device WHERE account = type::record($account) AND device_id = $deviceId LIMIT 1`, map[string]any{"account": account, "deviceId": deviceID}, &retry)
			if err2 == nil && len(retry) > 0 {
				var updated []pushDevice
				_ = s.db.Query(ctx, `UPDATE type::record($id) SET token = $fcmToken, platform = $platform, updated_at = time::now() RETURN AFTER`, map[string]any{"id": retry[0].ID, "fcmToken": token, "platform": platform}, &updated)
				if len(updated) > 0 {
					return &updated[0], nil
				}
			}
		}
		return nil, err
	}
	if len(created) > 0 {
		return &created[0], nil
	}
	return nil, nil
}

var errInvalidAccount = contextError("invalid_account")

type contextError string

func (e contextError) Error() string { return string(e) }

// Delete removes a device by account+deviceId. Idempotent.
func (s *pushStore) Delete(ctx context.Context, account, deviceID string) error {
	account = normalizeRecordID(account, "account")
	deviceID = strings.TrimSpace(deviceID)
	return s.db.Query(ctx, `DELETE FROM push_device WHERE account = type::record($account) AND device_id = $deviceId`, map[string]any{
		"account":  account,
		"deviceId": deviceID,
	}, nil)
}

// ListByAccount returns all push devices for an account.
func (s *pushStore) ListByAccount(ctx context.Context, account string) ([]pushDevice, error) {
	account = normalizeRecordID(account, "account")
	var devices []pushDevice
	err := s.db.Query(ctx, `SELECT <string>id AS id, <string>account AS account, token, platform, device_id AS deviceId, <string>created_at AS createdAt, <string>updated_at AS updatedAt FROM push_device WHERE account = type::record($account)`, map[string]any{
		"account": account,
	}, &devices)
	if err != nil {
		return nil, err
	}
	if devices == nil {
		devices = []pushDevice{}
	}
	return devices, nil
}

// DeleteByTokens deletes devices whose token is in the given list. Used for cleaning invalid FCM tokens.
func (s *pushStore) DeleteByTokens(ctx context.Context, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	// Filter empty
	cleaned := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return s.db.Query(ctx, `DELETE FROM push_device WHERE token IN $tokens`, map[string]any{
		"tokens": cleaned,
	}, nil)
}

// Validate push device input

func validatePushDeviceInput(token, platform, deviceID string) (string, string) {
	token = strings.TrimSpace(token)
	platform = strings.ToLower(strings.TrimSpace(platform))
	deviceID = strings.TrimSpace(deviceID)
	if token == "" || len(token) < 10 || len(token) > 4096 {
		return "invalid_token", "Geçerli bir FCM token gönderin."
	}
	// Basic token characters: no spaces, no control chars
	if strings.Contains(token, " ") || strings.Contains(token, "\n") || strings.Contains(token, "\t") {
		return "invalid_token", "FCM token geçersiz."
	}
	if platform != "ios" && platform != "android" && platform != "web" {
		return "invalid_platform", "Platform geçersiz. ios, android veya web olmalı."
	}
	if len(deviceID) < 8 || len(deviceID) > 200 {
		return "invalid_device", "Cihaz kimliği geçersiz."
	}
	return "", ""
}

type pushDeviceView struct {
	CreatedAt string `json:"createdAt"`
	DeviceID  string `json:"deviceId"`
	Platform  string `json:"platform"`
	Token     string `json:"token"`
	UpdatedAt string `json:"updatedAt"`
}

func (s *Server) putPushDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppVersion *string `json:"appVersion"`
		DeviceID   string  `json:"deviceId"`
		Platform   string  `json:"platform"`
		Token      string  `json:"token"`
	}
	if !decode(w, r, &input) {
		return
	}
	if code, msg := validatePushDeviceInput(input.Token, input.Platform, input.DeviceID); code != "" {
		respondError(w, http.StatusBadRequest, code, msg)
		return
	}
	acct := accountID(r)
	// Ensure pushStore exists
	if s.pushStore == nil {
		s.pushStore = newPushStore(s.db, s.log)
	}
	dev, err := s.pushStore.Upsert(r.Context(), acct, input.DeviceID, input.Token, input.Platform)
	if err != nil {
		s.databaseError(w, "upsert push device", err)
		return
	}
	if dev == nil {
		// Fallback: return input as view
		now := time.Now().UTC().Format(time.RFC3339Nano)
		respondJSON(w, http.StatusOK, pushDeviceView{
			DeviceID:  input.DeviceID,
			Platform:  strings.ToLower(strings.TrimSpace(input.Platform)),
			Token:     strings.TrimSpace(input.Token),
			CreatedAt: now,
			UpdatedAt: now,
		})
		return
	}
	respondJSON(w, http.StatusOK, pushDeviceView{
		DeviceID:  dev.DeviceID,
		Platform:  dev.Platform,
		Token:     dev.Token,
		CreatedAt: dev.CreatedAt,
		UpdatedAt: dev.UpdatedAt,
	})
}

func (s *Server) deletePushDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("deviceId")
	if deviceID == "" {
		// Fallback for case where router uses :id? Also try id
		deviceID = r.PathValue("id")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || len(deviceID) < 1 || len(deviceID) > 200 {
		respondError(w, http.StatusBadRequest, "invalid_device", "Cihaz kimliği geçersiz.")
		return
	}
	acct := accountID(r)
	if s.pushStore == nil {
		s.pushStore = newPushStore(s.db, s.log)
	}
	if err := s.pushStore.Delete(r.Context(), acct, deviceID); err != nil {
		s.databaseError(w, "delete push device", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
