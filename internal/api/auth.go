package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Hyd3dF/frame-social-2/internal/security"
)

type authRequestResponse struct {
	ChallengeID string `json:"challengeId"`
	DebugCode   string `json:"debugCode,omitempty"`
	ExpiresIn   int    `json:"expiresInSeconds"`
}

type signupRequest struct {
	CountryCode string `json:"countryCode"`
	DisplayName string `json:"displayName"`
	FullName    string `json:"fullName"`
	Phone       string `json:"phone"`
	Username    string `json:"username"`
}

type verifyRequest struct {
	ChallengeID string `json:"challengeId"`
	Code        string `json:"code"`
	DeviceID    string `json:"deviceId"`
	DeviceName  string `json:"deviceName"`
	Platform    string `json:"platform"`
}

type loginRequest struct {
	CountryCode string `json:"countryCode"`
	Phone       string `json:"phone"`
}

type tokenRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type recordID struct {
	ID string `json:"id"`
}

type accountAuth struct {
	AvatarURL   *string `json:"avatarUrl"`
	CountryCode string  `json:"countryCode"`
	DisplayName string  `json:"displayName"`
	FullName    string  `json:"fullName"`
	ID          string  `json:"id"`
	Phone       string  `json:"phone"`
	Username    string  `json:"username"`
}

type authResponse struct {
	Account accountAuth     `json:"account"`
	Tokens  security.Tokens `json:"tokens"`
}

func (s *Server) requestSignup(w http.ResponseWriter, r *http.Request) {
	var input signupRequest
	if !decode(w, r, &input) {
		return
	}
	input.FullName = strings.TrimSpace(input.FullName)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Username = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input.Username, "@")))
	if len([]rune(input.FullName)) < 2 || len([]rune(input.FullName)) > 80 || len([]rune(input.DisplayName)) < 2 || len([]rune(input.DisplayName)) > 50 || !usernamePattern.MatchString(input.Username) {
		respondError(w, http.StatusBadRequest, "invalid_profile", "Ad, görünen ad veya kullanıcı adı geçersiz.")
		return
	}
	phoneE164, err := normalizePhone(input.Phone, input.CountryCode)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_phone", "Geçerli bir telefon numarası girin.")
		return
	}
	var conflicts []recordID
	err = s.db.Query(r.Context(), `
SELECT <string>id AS id FROM account
WHERE phone_e164 = $phone OR username = $username LIMIT 1;`, map[string]any{"phone": phoneE164, "username": input.Username}, &conflicts)
	if err != nil {
		s.databaseError(w, "signup conflict lookup", err)
		return
	}
	if len(conflicts) != 0 {
		respondError(w, http.StatusConflict, "account_exists", "Telefon numarası veya kullanıcı adı zaten kullanılıyor.")
		return
	}
	code, err := security.OTP()
	if err != nil {
		s.internalError(w, "generate signup OTP", err)
		return
	}
	expires := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	var created []recordID
	err = s.db.Query(r.Context(), `
LET $challenge = CREATE ONLY signup_challenge CONTENT {
  phone_e164: $phone, country_code: $country, full_name: $full_name,
  display_name: $display_name, username: $username, otp_hash: $otp_hash,
  expires_at: <datetime>$expires, attempt_count: 0
};
RETURN [{ id: <string>$challenge.id }];`, map[string]any{
		"phone": phoneE164, "country": strings.ToUpper(input.CountryCode), "full_name": input.FullName,
		"display_name": input.DisplayName, "username": input.Username,
		"otp_hash": security.OTPHash(s.cfg.OTPPepper, phoneE164, code), "expires": expires,
	}, &created)
	if err != nil || len(created) == 0 {
		s.databaseError(w, "create signup challenge", err)
		return
	}
	response := authRequestResponse{ChallengeID: created[0].ID, ExpiresIn: 300}
	if s.cfg.OTPMode == "development" {
		response.DebugCode = code
	} else {
		s.log.Error("SMS provider is not configured", "mode", s.cfg.OTPMode)
		respondError(w, http.StatusServiceUnavailable, "sms_unavailable", "SMS servisi henüz yapılandırılmadı.")
		return
	}
	respondJSON(w, http.StatusCreated, response)
}

func (s *Server) verifySignup(w http.ResponseWriter, r *http.Request) {
	var input verifyRequest
	if !decode(w, r, &input) || !validVerifyRequest(input) || !validRecord(input.ChallengeID, "signup_challenge") {
		if input.ChallengeID != "" {
			respondError(w, http.StatusBadRequest, "invalid_request", "Doğrulama bilgileri geçersiz.")
		}
		return
	}
	var challenges []struct {
		AttemptCount int    `json:"attemptCount"`
		CountryCode  string `json:"countryCode"`
		DisplayName  string `json:"displayName"`
		ExpiresAt    string `json:"expiresAt"`
		FullName     string `json:"fullName"`
		OTPHash      string `json:"otpHash"`
		Phone        string `json:"phone"`
		Username     string `json:"username"`
	}
	err := s.db.Query(r.Context(), `
SELECT attempt_count AS attemptCount, country_code AS countryCode, display_name AS displayName,
       <string>expires_at AS expiresAt, full_name AS fullName, otp_hash AS otpHash,
       phone_e164 AS phone, username
FROM type::record($challenge)
WHERE consumed_at IS NONE AND expires_at > time::now() LIMIT 1;`, map[string]any{"challenge": input.ChallengeID}, &challenges)
	if err != nil {
		s.databaseError(w, "read signup challenge", err)
		return
	}
	if len(challenges) == 0 || challenges[0].AttemptCount >= 5 {
		respondError(w, http.StatusGone, "challenge_expired", "Doğrulama kodunun süresi dolmuş.")
		return
	}
	challenge := challenges[0]
	if !security.VerifyOTP(s.cfg.OTPPepper, challenge.Phone, input.Code, challenge.OTPHash) {
		_ = s.db.Query(r.Context(), `UPDATE type::record($challenge) SET attempt_count += 1;`, map[string]any{"challenge": input.ChallengeID}, nil)
		respondError(w, http.StatusUnauthorized, "invalid_code", "Doğrulama kodu yanlış.")
		return
	}
	var accounts []accountAuth
	err = s.db.Query(r.Context(), `
BEGIN TRANSACTION;
	LET $account = CREATE ONLY account CONTENT {
  phone_e164: $phone, country_code: $country, full_name: $full_name,
  display_name: $display_name, username: $username, phone_verified_at: time::now()
};
CREATE privacy_setting CONTENT { account: $account.id };
UPDATE type::record($challenge) SET consumed_at = time::now(), verified_at = time::now();
COMMIT TRANSACTION;
SELECT <string>id AS id, phone_e164 AS phone, country_code AS countryCode,
       full_name AS fullName, display_name AS displayName, username, avatar.public_url AS avatarUrl
FROM $account;`, map[string]any{
		"phone": challenge.Phone, "country": challenge.CountryCode, "full_name": challenge.FullName,
		"display_name": challenge.DisplayName, "username": challenge.Username, "challenge": input.ChallengeID,
	}, &accounts)
	if err != nil || len(accounts) == 0 {
		s.databaseError(w, "create account", err)
		return
	}
	tokens, err := s.createSession(r.Context(), accounts[0].ID, input)
	if err != nil {
		s.databaseError(w, "create auth session", err)
		return
	}
	respondJSON(w, http.StatusCreated, authResponse{Account: accounts[0], Tokens: tokens})
}

func (s *Server) requestLogin(w http.ResponseWriter, r *http.Request) {
	var input loginRequest
	if !decode(w, r, &input) {
		return
	}
	phoneE164, err := normalizePhone(input.Phone, input.CountryCode)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_phone", "Geçerli bir telefon numarası girin.")
		return
	}
	var accounts []recordID
	err = s.db.Query(r.Context(), `SELECT <string>id AS id FROM account WHERE phone_e164 = $phone AND status = 'active' LIMIT 1;`, map[string]any{"phone": phoneE164}, &accounts)
	if err != nil {
		s.databaseError(w, "login account lookup", err)
		return
	}
	if len(accounts) == 0 {
		respondError(w, http.StatusNotFound, "account_not_found", "Bu telefon numarasıyla kayıtlı hesap bulunamadı.")
		return
	}
	code, err := security.OTP()
	if err != nil {
		s.internalError(w, "generate login OTP", err)
		return
	}
	var created []recordID
	err = s.db.Query(r.Context(), `LET $challenge = CREATE ONLY login_challenge CONTENT {
	account: type::record($account), phone_e164: $phone, otp_hash: $otp_hash,
	expires_at: time::now() + 5m, attempt_count: 0
};
RETURN [{ id: <string>$challenge.id }];`, map[string]any{"account": accounts[0].ID, "phone": phoneE164, "otp_hash": security.OTPHash(s.cfg.OTPPepper, phoneE164, code)}, &created)
	if err != nil || len(created) == 0 {
		s.databaseError(w, "create login challenge", err)
		return
	}
	response := authRequestResponse{ChallengeID: created[0].ID, ExpiresIn: 300}
	if s.cfg.OTPMode == "development" {
		response.DebugCode = code
	} else {
		respondError(w, http.StatusServiceUnavailable, "sms_unavailable", "SMS servisi henüz yapılandırılmadı.")
		return
	}
	respondJSON(w, http.StatusCreated, response)
}

func (s *Server) verifyLogin(w http.ResponseWriter, r *http.Request) {
	var input verifyRequest
	if !decode(w, r, &input) || !validVerifyRequest(input) || !validRecord(input.ChallengeID, "login_challenge") {
		if input.ChallengeID != "" {
			respondError(w, http.StatusBadRequest, "invalid_request", "Doğrulama bilgileri geçersiz.")
		}
		return
	}
	var challenges []struct {
		Account      string `json:"account"`
		AttemptCount int    `json:"attemptCount"`
		OTPHash      string `json:"otpHash"`
		Phone        string `json:"phone"`
	}
	err := s.db.Query(r.Context(), `SELECT <string>account AS account, attempt_count AS attemptCount,
otp_hash AS otpHash, phone_e164 AS phone FROM type::record($challenge)
WHERE consumed_at IS NONE AND expires_at > time::now() LIMIT 1;`, map[string]any{"challenge": input.ChallengeID}, &challenges)
	if err != nil {
		s.databaseError(w, "read login challenge", err)
		return
	}
	if len(challenges) == 0 || challenges[0].AttemptCount >= 5 {
		respondError(w, http.StatusGone, "challenge_expired", "Doğrulama kodunun süresi dolmuş.")
		return
	}
	challenge := challenges[0]
	if !security.VerifyOTP(s.cfg.OTPPepper, challenge.Phone, input.Code, challenge.OTPHash) {
		_ = s.db.Query(r.Context(), `UPDATE type::record($challenge) SET attempt_count += 1;`, map[string]any{"challenge": input.ChallengeID}, nil)
		respondError(w, http.StatusUnauthorized, "invalid_code", "Doğrulama kodu yanlış.")
		return
	}
	var accounts []accountAuth
	err = s.db.Query(r.Context(), `UPDATE type::record($challenge) SET consumed_at = time::now(), verified_at = time::now();
SELECT <string>id AS id, phone_e164 AS phone, country_code AS countryCode,
full_name AS fullName, display_name AS displayName, username, avatar.public_url AS avatarUrl
FROM type::record($account) WHERE status = 'active';`, map[string]any{"challenge": input.ChallengeID, "account": challenge.Account}, &accounts)
	if err != nil || len(accounts) == 0 {
		s.databaseError(w, "complete login challenge", err)
		return
	}
	tokens, err := s.createSession(r.Context(), accounts[0].ID, input)
	if err != nil {
		s.databaseError(w, "create login session", err)
		return
	}
	respondJSON(w, http.StatusOK, authResponse{Account: accounts[0], Tokens: tokens})
}

func (s *Server) createSession(ctx context.Context, account string, input verifyRequest) (security.Tokens, error) {
	refresh, err := security.RefreshToken()
	if err != nil {
		return security.Tokens{}, err
	}
	expiresAt := time.Now().UTC().Add(time.Duration(s.cfg.RefreshTokenDays) * 24 * time.Hour)
err = s.db.Query(ctx, `CREATE auth_session CONTENT {
 account: type::record($account), device_id: <string>$device_id, device_name: $device_name,
 platform: $platform, refresh_token_hash: $refresh_hash, expires_at: <datetime>$expires
};`, map[string]any{
		"account": account, "device_id": input.DeviceID, "device_name": deviceName(input.DeviceName),
		"platform": input.Platform, "refresh_hash": security.TokenHash(refresh), "expires": expiresAt.Format(time.RFC3339Nano),
	}, nil)
	if err != nil {
		return security.Tokens{}, err
	}
	access, err := security.AccessToken(s.cfg.JWTSecret, account, s.cfg.AccessTokenMinutes)
	if err != nil {
		return security.Tokens{}, err
	}
	return security.Tokens{AccessToken: access, RefreshToken: refresh, RefreshTokenExpiresAt: expiresAt.Format(time.RFC3339)}, nil
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var input tokenRequest
	if !decode(w, r, &input) || input.RefreshToken == "" {
		return
	}
	newRefresh, err := security.RefreshToken()
	if err != nil {
		s.internalError(w, "rotate refresh token", err)
		return
	}
	var sessions []struct {
		Account   string `json:"account"`
		ExpiresAt string `json:"expiresAt"`
	}
	err = s.db.Query(r.Context(), `UPDATE auth_session SET
refresh_token_hash = $new_hash, last_used_at = time::now()
WHERE refresh_token_hash = $old_hash AND revoked_at IS NONE AND expires_at > time::now()
RETURN { account: <string>account, expiresAt: <string>expires_at };`, map[string]any{
		"old_hash": security.TokenHash(input.RefreshToken), "new_hash": security.TokenHash(newRefresh),
	}, &sessions)
	if err != nil {
		s.databaseError(w, "refresh session", err)
		return
	}
	if len(sessions) == 0 {
		respondError(w, http.StatusUnauthorized, "invalid_refresh_token", "Oturum yenileme anahtarı geçersiz.")
		return
	}
	access, err := security.AccessToken(s.cfg.JWTSecret, sessions[0].Account, s.cfg.AccessTokenMinutes)
	if err != nil {
		s.internalError(w, "create access token", err)
		return
	}
	respondJSON(w, http.StatusOK, security.Tokens{AccessToken: access, RefreshToken: newRefresh, RefreshTokenExpiresAt: sessions[0].ExpiresAt})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var input tokenRequest
	if !decode(w, r, &input) || input.RefreshToken == "" {
		return
	}
	if err := s.db.Query(r.Context(), `UPDATE auth_session SET revoked_at = time::now()
WHERE refresh_token_hash = $hash AND revoked_at IS NONE;`, map[string]any{"hash": security.TokenHash(input.RefreshToken)}, nil); err != nil {
		s.databaseError(w, "logout session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) databaseError(w http.ResponseWriter, operation string, err error) {
	s.log.Error("database operation failed", "operation", operation, "error", err)
	respondError(w, http.StatusInternalServerError, "database_error", "İşlem tamamlanamadı.")
}

func (s *Server) internalError(w http.ResponseWriter, operation string, err error) {
	s.log.Error("internal operation failed", "operation", operation, "error", err)
	respondError(w, http.StatusInternalServerError, "internal_error", "Beklenmeyen bir hata oluştu.")
}
