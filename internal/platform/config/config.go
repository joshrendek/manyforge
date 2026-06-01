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
	SMTPAddr                string  // built-in inbound SMTP receiver listen address; empty disables it
	InboundWebhookSecret    string  // HMAC-SHA256 secret for the provider webhook signature (constant-time verified)
	InboundReplyTokenSecret []byte  // HMAC key minting/verifying the threading reply token; purpose-separated from the webhook + JWT secrets
	InboundSystemAddrSecret []byte  // HMAC key deriving the unguessable system inbound-address localpart (FR-001); purpose-separated from the reply-token/webhook/JWT secrets
	BlobURL                 string  // attachment object-storage backend (file:///… or s3://…); empty disables attachments
	InboundSystemDomain     string  // platform-hosted domain that auto-provisioned system inbound addresses live on
	DKIMKeyPath             string  // path to the default DKIM private key for verified custom sending identities
	InboundMaxBytes         int64   // max inbound message size (SMTP MaxMessageBytes + webhook body cap), FR-007/FR-020
	AttachmentMaxBytes      int64   // per-attachment size cap, FR-007
	IngestRateRPS           float64 // per-recipient inbound ingestion refill rate (loop/abuse bound, FR-018/FR-020)
	IngestRateBurst         float64 // per-recipient inbound ingestion burst allowance
	OutboundRateRPS         float64 // per-business outbound send refill rate (FR-020)
	OutboundRateBurst       float64 // per-business outbound send burst allowance

	// Outbound SMTP relay (US2/T039). SMTPHost empty ⇒ the notify worker uses the
	// dev LogSender (logs the threaded reply, honors suppression) instead of a real
	// MTA, so reply flows are completable without an outbound relay configured.
	SMTPHost string // outbound relay host; empty ⇒ LogSender
	SMTPPort int    // outbound relay port (default 587)
	SMTPUser string // SMTP AUTH username; empty ⇒ no auth
	SMTPPass string // SMTP AUTH password

	// Optional system-domain DKIM signing (FR-013 deliverability). All THREE must be
	// set to sign; otherwise outbound is sent unsigned (the locked default — DKIM is
	// not required for the system domain in dev / un-provisioned envs).
	SystemDKIMDomain        string // d= domain, e.g. the InboundSystemDomain
	SystemDKIMSelector      string // s= selector
	SystemDKIMPrivateKeyPEM string // PEM-encoded private key (ed25519 PKCS#8 or RSA); inline or via *_PATH
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
	// Reply-token HMAC key: purpose-separated from the webhook + JWT secrets so a
	// leak of one cannot forge the others. Decoded as raw bytes (the string is the
	// key material); empty in dev is tolerated (threading falls back to headers).
	cfg.InboundReplyTokenSecret = []byte(os.Getenv("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET"))
	// System inbound-address HMAC key (FR-001): derives the unguessable localpart of
	// the auto-provisioned b-…@<domain> address. Purpose-separated from the reply-token
	// key; empty in dev is tolerated (the address is still deterministic+idempotent,
	// just not key-protected against enumeration in dev).
	cfg.InboundSystemAddrSecret = []byte(os.Getenv("MANYFORGE_INBOUND_SYSTEM_ADDRESS_SECRET"))
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

	// Outbound SMTP relay (US2). Host empty ⇒ LogSender; port defaults to submission.
	cfg.SMTPHost = os.Getenv("MANYFORGE_SMTP_HOST")
	if cfg.SMTPPort, err = envInt("MANYFORGE_SMTP_PORT", 587); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_SMTP_PORT: %w", err)
	}
	cfg.SMTPUser = os.Getenv("MANYFORGE_SMTP_USER")
	cfg.SMTPPass = os.Getenv("MANYFORGE_SMTP_PASS")

	// Optional system DKIM. The private key can be supplied inline (…_PEM) or via a
	// file path (…_PEM_PATH); a malformed path is a hard config error so a configured
	// key never silently degrades to unsigned (the no-DKIM default is the empty case).
	cfg.SystemDKIMDomain = os.Getenv("MANYFORGE_SYSTEM_DKIM_DOMAIN")
	cfg.SystemDKIMSelector = os.Getenv("MANYFORGE_SYSTEM_DKIM_SELECTOR")
	cfg.SystemDKIMPrivateKeyPEM = os.Getenv("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM")
	if cfg.SystemDKIMPrivateKeyPEM == "" {
		if p := os.Getenv("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM_PATH"); p != "" {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return Config{}, fmt.Errorf("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM_PATH: %w", rerr)
			}
			cfg.SystemDKIMPrivateKeyPEM = string(b)
		}
	}

	return cfg, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
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
