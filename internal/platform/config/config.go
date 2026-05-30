// Package config loads runtime configuration from the environment (12-factor).
// Secrets are never hard-coded; see .env.example for the full set.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds runtime configuration.
type Config struct {
	Addr             string        // HTTP listen address
	DatabaseURL      string        // PostgreSQL DSN for the app role (non-superuser, non-BYPASSRLS)
	AccessTokenTTL   time.Duration // EdDSA access-token lifetime
	TrustedProxyCIDR string        // CIDR allowed to set the client IP via X-Forwarded-For
}

// Load reads configuration from the environment, applying safe local-dev
// defaults. It errors only on malformed values.
func Load() (Config, error) {
	cfg := Config{
		Addr:             env("MANYFORGE_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("MANYFORGE_DATABASE_URL"),
		TrustedProxyCIDR: os.Getenv("MANYFORGE_TRUSTED_PROXY_CIDR"),
	}

	ttl, err := envDuration("MANYFORGE_ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_ACCESS_TOKEN_TTL: %w", err)
	}
	cfg.AccessTokenTTL = ttl

	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return time.ParseDuration(v)
}
