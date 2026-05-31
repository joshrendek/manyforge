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

	// Support desk (spec 002).
	SMTPAddr             string  // built-in inbound SMTP receiver listen address; empty disables it
	InboundWebhookSecret string  // HMAC-SHA256 secret for the provider webhook signature (constant-time verified)
	BlobURL              string  // attachment object-storage backend (file:///… or s3://…); empty disables attachments
	InboundSystemDomain  string  // platform-hosted domain that auto-provisioned system inbound addresses live on
	DKIMKeyPath          string  // path to the default DKIM private key for verified custom sending identities
	InboundMaxBytes      int64   // max inbound message size (SMTP MaxMessageBytes + webhook body cap), FR-007/FR-020
	AttachmentMaxBytes   int64   // per-attachment size cap, FR-007
	IngestRateRPS        float64 // per-recipient inbound ingestion refill rate (loop/abuse bound, FR-018/FR-020)
	IngestRateBurst      float64 // per-recipient inbound ingestion burst allowance
	OutboundRateRPS      float64 // per-business outbound send refill rate (FR-020)
	OutboundRateBurst    float64 // per-business outbound send burst allowance
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

	// Support desk (spec 002).
	cfg.SMTPAddr = os.Getenv("MANYFORGE_SMTP_ADDR")
	cfg.InboundWebhookSecret = os.Getenv("MANYFORGE_INBOUND_WEBHOOK_SECRET")
	cfg.BlobURL = os.Getenv("MANYFORGE_BLOB_URL")
	cfg.InboundSystemDomain = env("MANYFORGE_INBOUND_SYSTEM_DOMAIN", "inbound.localhost")
	cfg.DKIMKeyPath = os.Getenv("MANYFORGE_DKIM_KEY_PATH")

	if cfg.InboundMaxBytes, err = envInt64("MANYFORGE_INBOUND_MAX_BYTES", 30<<20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INBOUND_MAX_BYTES: %w", err)
	}
	if cfg.AttachmentMaxBytes, err = envInt64("MANYFORGE_ATTACHMENT_MAX_BYTES", 25<<20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_ATTACHMENT_MAX_BYTES: %w", err)
	}
	if cfg.IngestRateRPS, err = envFloat("MANYFORGE_INGEST_RATE_RPS", 2); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INGEST_RATE_RPS: %w", err)
	}
	if cfg.IngestRateBurst, err = envFloat("MANYFORGE_INGEST_RATE_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INGEST_RATE_BURST: %w", err)
	}
	if cfg.OutboundRateRPS, err = envFloat("MANYFORGE_OUTBOUND_RATE_RPS", 2); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_OUTBOUND_RATE_RPS: %w", err)
	}
	if cfg.OutboundRateBurst, err = envFloat("MANYFORGE_OUTBOUND_RATE_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_OUTBOUND_RATE_BURST: %w", err)
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

func envInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseInt(v, 10, 64)
}
