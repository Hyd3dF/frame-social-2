package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresExternalSecretsAndDatabaseConfiguration(t *testing.T) {
	for _, key := range []string{
		"JWT_SECRET",
		"OTP_PEPPER",
		"PUBLIC_BASE_URL",
		"SURREAL_DATABASE",
		"SURREAL_NAMESPACE",
		"SURREAL_PASSWORD",
		"SURREAL_PROXY_TOKEN",
		"SURREAL_URL",
		"SURREAL_USERNAME",
	} {
		t.Setenv(key, "")
	}

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "required environment variables are missing") {
		t.Fatalf("expected missing configuration error, got %v", err)
	}
}

func TestLoadRejectsDevelopmentOTPInProduction(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("OTP_MODE", "development")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OTP_MODE=development") {
		t.Fatalf("expected production OTP mode error, got %v", err)
	}
}

func TestLoadAcceptsCompleteExternalConfiguration(t *testing.T) {
	setValidEnvironment(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected valid configuration, got %v", err)
	}
	if cfg.SurrealDatabase != "app" || cfg.HTTPAddr != ":3000" {
		t.Fatalf("unexpected configuration: %#v", cfg)
	}
}

func TestLoadUsesRenderPort(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("PORT", "10000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected valid configuration, got %v", err)
	}
	if cfg.HTTPAddr != ":10000" {
		t.Fatalf("expected Render port, got %q", cfg.HTTPAddr)
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("HTTP_ADDR", ":3000")
	t.Setenv("PORT", "")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))
	t.Setenv("OTP_MODE", "development")
	t.Setenv("OTP_PEPPER", strings.Repeat("p", 32))
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.com")
	t.Setenv("SURREAL_DATABASE", "app")
	t.Setenv("SURREAL_NAMESPACE", "example")
	t.Setenv("SURREAL_PASSWORD", "test-password")
	t.Setenv("SURREAL_PROXY_TOKEN", strings.Repeat("x", 64))
	t.Setenv("SURREAL_URL", "https://surrealdb.example.com")
	t.Setenv("SURREAL_USERNAME", "backend")
}
