package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	AccessTokenMinutes int
	AppEnv             string
	HTTPAddr           string
	JWTSecret          string
	OTPMode            string
	OTPPepper          string
	PublicBaseURL      string
	RefreshTokenDays   int
	SurrealDatabase    string
	SurrealNamespace   string
	SurrealPassword    string
	SurrealURL         string
	SurrealUsername    string
}

func Load() (Config, error) {
	cfg := Config{
		AccessTokenMinutes: intEnv("ACCESS_TOKEN_MINUTES", 15),
		AppEnv:             env("APP_ENV", "development"),
		HTTPAddr:           env("HTTP_ADDR", ":3000"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		OTPMode:            env("OTP_MODE", "development"),
		OTPPepper:          os.Getenv("OTP_PEPPER"),
		PublicBaseURL:      os.Getenv("PUBLIC_BASE_URL"),
		RefreshTokenDays:   intEnv("REFRESH_TOKEN_DAYS", 30),
		SurrealDatabase:    os.Getenv("SURREAL_DATABASE"),
		SurrealNamespace:   os.Getenv("SURREAL_NAMESPACE"),
		SurrealPassword:    os.Getenv("SURREAL_PASSWORD"),
		SurrealURL:         os.Getenv("SURREAL_URL"),
		SurrealUsername:    os.Getenv("SURREAL_USERNAME"),
	}
	missing := missingRequired(map[string]string{
		"JWT_SECRET":        cfg.JWTSecret,
		"OTP_PEPPER":        cfg.OTPPepper,
		"PUBLIC_BASE_URL":   cfg.PublicBaseURL,
		"SURREAL_DATABASE":  cfg.SurrealDatabase,
		"SURREAL_NAMESPACE": cfg.SurrealNamespace,
		"SURREAL_PASSWORD":  cfg.SurrealPassword,
		"SURREAL_URL":       cfg.SurrealURL,
		"SURREAL_USERNAME":  cfg.SurrealUsername,
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

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
