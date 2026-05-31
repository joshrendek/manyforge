// Package config loads runtime configuration from the environment (12-factor).
// Secrets are never hard-coded; see .env.example for the full set.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds runtime configuration.
type Config struct {
	Addr             string        // HTTP listen address
	DatabaseURL      string        // PostgreSQL DSN for the app role (non-superuser, non-BYPASSRLS)
	AccessTokenTTL   time.Duration // EdDSA access-token lifetime
	TrustedProxyCIDR string        // CIDR allowed to set the client IP via X-Forwarded-For
	JWTIssuer        string        // expected/stamped token issuer
	JWTAudience      string        // expected/stamped token audience
	RateLimitRPS     float64       // per-IP token refill rate for abuse-sensitive routes (FR-029)
	RateLimitBurst   float64       // per-IP burst allowance for abuse-sensitive routes
}

// Load reads configuration from the environment, applying safe local-dev
// defaults. It errors only on malformed values.
func Load() (Config, error) {
	cfg := Config{
		Addr:             env("MANYFORGE_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("MANYFORGE_DATABASE_URL"),
		TrustedProxyCIDR: os.Getenv("MANYFORGE_TRUSTED_PROXY_CIDR"),
		JWTIssuer:        env("MANYFORGE_JWT_ISSUER", "manyforge"),
		JWTAudience:      env("MANYFORGE_JWT_AUDIENCE", "manyforge-api"),
	}

	ttl, err := envDuration("MANYFORGE_ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_ACCESS_TOKEN_TTL: %w", err)
	}
	cfg.AccessTokenTTL = ttl

	if cfg.RateLimitRPS, err = envFloat("MANYFORGE_RATELIMIT_RPS", 5); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_RATELIMIT_RPS: %w", err)
	}
	if cfg.RateLimitBurst, err = envFloat("MANYFORGE_RATELIMIT_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_RATELIMIT_BURST: %w", err)
	}

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

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseFloat(v, 64)
}
