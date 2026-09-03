package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	AccessTokenMinutes        int
	AppEnv                    string
	FirebaseCredentialsBase64 string
	FirebaseCredentialsJSON   string
	FirebaseProjectID         string
	HTTPAddr                  string
	JWTSecret                 string
	OTPMode                   string
	OTPPepper                 string
	PublicBaseURL             string
	RefreshTokenDays          int
	SurrealDatabase           string
	SurrealNamespace          string
	SurrealPassword           string
	SurrealProxyToken         string
	SurrealURL                string
	SurrealUsername           string
}

func Load() (Config, error) {
	cfg := Config{
		AccessTokenMinutes:        intEnv("ACCESS_TOKEN_MINUTES", 15),
		AppEnv:                    env("APP_ENV", "development"),
		FirebaseCredentialsBase64: envFirst("FIREBASE_SERVICE_ACCOUNT_BASE64", "FIREBASE_CREDENTIALS_BASE64", "GOOGLE_APPLICATION_CREDENTIALS_BASE64"),
		FirebaseCredentialsJSON:   envFirst("FIREBASE_SERVICE_ACCOUNT_JSON", "FIREBASE_CREDENTIALS_JSON", "GOOGLE_APPLICATION_CREDENTIALS_JSON", "FIREBASE_CREDENTIALS"),
		FirebaseProjectID:         envFirst("FIREBASE_PROJECT_ID", "FIREBASE_PROJECT", "GCLOUD_PROJECT"),
		HTTPAddr:                  listenAddr(),
		JWTSecret:                 os.Getenv("JWT_SECRET"),
		OTPMode:                   env("OTP_MODE", "development"),
		OTPPepper:                 os.Getenv("OTP_PEPPER"),
		PublicBaseURL:             os.Getenv("PUBLIC_BASE_URL"),
		RefreshTokenDays:          intEnv("REFRESH_TOKEN_DAYS", 30),
		SurrealDatabase:           os.Getenv("SURREAL_DATABASE"),
		SurrealNamespace:          os.Getenv("SURREAL_NAMESPACE"),
		SurrealPassword:           os.Getenv("SURREAL_PASSWORD"),
		SurrealProxyToken:         os.Getenv("SURREAL_PROXY_TOKEN"),
		SurrealURL:                os.Getenv("SURREAL_URL"),
		SurrealUsername:           os.Getenv("SURREAL_USERNAME"),
	}
	missing := missingRequired(map[string]string{
		"JWT_SECRET":          cfg.JWTSecret,
		"OTP_PEPPER":          cfg.OTPPepper,
		"PUBLIC_BASE_URL":     cfg.PublicBaseURL,
		"SURREAL_DATABASE":    cfg.SurrealDatabase,
		"SURREAL_NAMESPACE":   cfg.SurrealNamespace,
		"SURREAL_PASSWORD":    cfg.SurrealPassword,
		"SURREAL_PROXY_TOKEN": cfg.SurrealProxyToken,
		"SURREAL_URL":         cfg.SurrealURL,
		"SURREAL_USERNAME":    cfg.SurrealUsername,
	})
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("required environment variables are missing: %s", strings.Join(missing, ", "))
	}
	if len(cfg.JWTSecret) < 32 || len(cfg.OTPPepper) < 32 {
		return Config{}, errors.New("JWT_SECRET and OTP_PEPPER must each contain at least 32 characters")
	}
	if cfg.AppEnv == "production" && cfg.OTPMode == "development" {
		return Config{}, errors.New("OTP_MODE=development is not allowed when APP_ENV=production")
	}
	return cfg, nil
}

func missingRequired(values map[string]string) []string {
	missing := make([]string, 0)
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

func listenAddr() string {
	if value := os.Getenv("HTTP_ADDR"); value != "" {
		return value
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":3000"
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// FirebaseEnabled reports whether Firebase credentials are configured.
func (c Config) FirebaseEnabled() bool {
	return c.FirebaseCredentialsJSON != "" || c.FirebaseCredentialsBase64 != ""
}

// FirebaseCredentials returns decoded service account JSON bytes.
// It decodes FIREBASE_SERVICE_ACCOUNT_BASE64 if set, otherwise returns
// the raw JSON from FIREBASE_SERVICE_ACCOUNT_JSON. No logging of secrets.
func (c Config) FirebaseCredentials() ([]byte, error) {
	if c.FirebaseCredentialsBase64 != "" {
		// Remove all whitespace (newlines, spaces) that may be introduced by
		// copy-paste or by `base64 -w` line wrapping.
		cleaned := strings.TrimSpace(c.FirebaseCredentialsBase64)
		cleaned = strings.ReplaceAll(cleaned, "\n", "")
		cleaned = strings.ReplaceAll(cleaned, "\r", "")
		cleaned = strings.ReplaceAll(cleaned, " ", "")
		cleaned = strings.ReplaceAll(cleaned, "\t", "")
		if cleaned == "" {
			return nil, errors.New("FIREBASE_SERVICE_ACCOUNT_BASE64 is empty after trim")
		}
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			// Try RawStd without padding and URL encoding variants
			if decoded2, err2 := base64.RawStdEncoding.DecodeString(cleaned); err2 == nil {
				if len(decoded2) == 0 {
					return nil, errors.New("FIREBASE_SERVICE_ACCOUNT_BASE64 is empty after decode")
				}
				return decoded2, nil
			}
			if decoded3, err3 := base64.URLEncoding.DecodeString(cleaned); err3 == nil {
				return decoded3, nil
			}
			if decoded4, err4 := base64.RawURLEncoding.DecodeString(cleaned); err4 == nil {
				return decoded4, nil
			}
			return nil, fmt.Errorf("decode FIREBASE_SERVICE_ACCOUNT_BASE64: %w", err)
		}
		if len(decoded) == 0 {
			return nil, errors.New("FIREBASE_SERVICE_ACCOUNT_BASE64 is empty after decode")
		}
		return decoded, nil
	}
	if c.FirebaseCredentialsJSON != "" {
		return []byte(c.FirebaseCredentialsJSON), nil
	}
	return nil, nil
}

func intEnv(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
