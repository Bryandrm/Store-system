// Package config reads runtime settings from the environment.
//
// One file, no framework, no config file format. Everything the process needs
// arrives as an environment variable, which is what both docker compose and a
// systemd unit hand it naturally.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bryandrm/store-system/internal/auth"
)

// Config is the complete runtime configuration.
type Config struct {
	// DatabaseURL is the connection string for the application role. In
	// production it points at store_app, never at the migrator.
	DatabaseURL string

	// MigrationURL runs the migrations. It needs a role that can create
	// objects, so it is separate from DatabaseURL by design.
	MigrationURL string

	Addr string

	// AllowedOrigins are the exact origins the PWA is served from. Never "*":
	// the API is on a different host than the frontend by design.
	AllowedOrigins []string

	// TrustProxy tells the rate limiter whether X-Forwarded-For can be
	// believed. True behind Caddy, false when the API is directly reachable.
	// Getting it wrong the permissive way lets anyone bypass the login limit.
	TrustProxy bool

	// Production switches logging to JSON and is the flag any future
	// environment-dependent behaviour should hang off.
	Production bool

	ShutdownGrace time.Duration

	// Login rate limits. Production never sets these; the end-to-end suite
	// does, because it logs in far more often in a minute than a human would
	// in a week. See internal/auth/ratelimit.go.
	LoginPerIPPerMinute     int
	LoginPerUsernamePerHour int
}

// Load reads the environment and validates it.
//
// It fails loudly at startup rather than defaulting: a server that silently
// comes up pointing at the wrong database is worse than one that refuses to.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		MigrationURL:   os.Getenv("MIGRATION_DATABASE_URL"),
		Addr:           envOr("ADDR", ":8080"),
		AllowedOrigins: splitAndTrim(os.Getenv("ALLOWED_ORIGINS")),
		TrustProxy:     envBool("TRUST_PROXY", false),
		Production:     envBool("PRODUCTION", false),
		ShutdownGrace:  15 * time.Second,

		LoginPerIPPerMinute:     envInt("LOGIN_RATE_PER_IP_PER_MINUTE", auth.DefaultPerIPPerMinute),
		LoginPerUsernamePerHour: envInt("LOGIN_RATE_PER_USER_PER_HOUR", auth.DefaultPerUsernamePerHour),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}
	// Migrations may run with the same role in development, where the compose
	// user owns everything anyway.
	if cfg.MigrationURL == "" {
		cfg.MigrationURL = cfg.DatabaseURL
	}

	// An empty origin list would mean the PWA cannot talk to the API at all,
	// which is a misconfiguration that is otherwise very confusing to debug
	// from the browser console.
	if len(cfg.AllowedOrigins) == 0 {
		return Config{}, fmt.Errorf("config: ALLOWED_ORIGINS is required " +
			"(comma-separated, e.g. https://tienda.example.com)")
	}
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			return Config{}, fmt.Errorf("config: ALLOWED_ORIGINS must not contain '*'")
		}
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func splitAndTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
