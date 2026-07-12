package main

import "testing"

func TestParseBootstrapAdminConfigDefaults(t *testing.T) {
	cfg, err := parseBootstrapAdminConfig(nil, func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://dev:dev@localhost/openlinker?sslmode=disable"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseBootstrapAdminConfig returned error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Fatalf("Environment = %q, want development", cfg.Environment)
	}
	if cfg.Email != localBootstrapAdminEmail {
		t.Fatalf("Email = %q, want %q", cfg.Email, localBootstrapAdminEmail)
	}
	if cfg.Password != localBootstrapAdminPassword {
		t.Fatalf("Password = %q, want default password", cfg.Password)
	}
	if cfg.DisplayName != defaultBootstrapAdminDisplayName {
		t.Fatalf("DisplayName = %q, want %q", cfg.DisplayName, defaultBootstrapAdminDisplayName)
	}
}

func TestParseBootstrapAdminConfigOverrides(t *testing.T) {
	env := map[string]string{
		"ENV":                                     "production",
		"DATABASE_URL":                            "postgres://dev:dev@localhost/openlinker?sslmode=disable",
		"OPENLINKER_BOOTSTRAP_ADMIN_EMAIL":        "Root@OpenLinker.AI",
		"OPENLINKER_BOOTSTRAP_ADMIN_PASSWORD":     "private-password",
		"OPENLINKER_BOOTSTRAP_ADMIN_DISPLAY_NAME": "Root Admin",
	}
	cfg, err := parseBootstrapAdminConfig(nil, func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatalf("parseBootstrapAdminConfig returned error: %v", err)
	}
	if cfg.Email != "root@openlinker.ai" {
		t.Fatalf("Email = %q, want normalized email", cfg.Email)
	}
	if cfg.Password != "private-password" {
		t.Fatalf("Password = %q, want env password", cfg.Password)
	}
	if cfg.DisplayName != "Root Admin" {
		t.Fatalf("DisplayName = %q, want env display name", cfg.DisplayName)
	}
}

func TestParseBootstrapAdminConfigFailsClosedOutsideLocalDevelopment(t *testing.T) {
	base := map[string]string{
		"ENV":          "production",
		"DATABASE_URL": "postgres://dev:dev@localhost/openlinker?sslmode=disable",
	}
	getenv := func(key string) string { return base[key] }

	if _, err := parseBootstrapAdminConfig(nil, getenv); err == nil || err.Error() != "OPENLINKER_BOOTSTRAP_ADMIN_EMAIL is required outside local/development/test" {
		t.Fatalf("missing email error = %v", err)
	}
	base["OPENLINKER_BOOTSTRAP_ADMIN_EMAIL"] = "admin@example.com"
	if _, err := parseBootstrapAdminConfig(nil, getenv); err == nil || err.Error() != "OPENLINKER_BOOTSTRAP_ADMIN_PASSWORD is required outside local/development/test" {
		t.Fatalf("missing password error = %v", err)
	}
	base["OPENLINKER_BOOTSTRAP_ADMIN_PASSWORD"] = localBootstrapAdminPassword
	if _, err := parseBootstrapAdminConfig(nil, getenv); err == nil || err.Error() != "OPENLINKER_BOOTSTRAP_ADMIN_PASSWORD must not use the local development default" {
		t.Fatalf("known password error = %v", err)
	}
	base["OPENLINKER_BOOTSTRAP_ADMIN_PASSWORD"] = "short-pass"
	if _, err := parseBootstrapAdminConfig(nil, getenv); err == nil || err.Error() != "admin password length must be 12-72 bytes" {
		t.Fatalf("short password error = %v", err)
	}
}

func TestParseBootstrapAdminConfigRejectsDisplayAddress(t *testing.T) {
	_, err := parseBootstrapAdminConfig([]string{"-email", "Root <root@openlinker.ai>"}, func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://dev:dev@localhost/openlinker?sslmode=disable"
		}
		return ""
	})
	if err == nil {
		t.Fatal("parseBootstrapAdminConfig returned nil error for display address")
	}
}
