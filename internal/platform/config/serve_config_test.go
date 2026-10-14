package config

import (
	"strings"
	"testing"
)

// MF-017-REPLY-TOKEN-SECRET: an empty reply-token HMAC key lets anyone forge a
// reply token and inject mail into any ticket of a business (spec 017 R14).
func TestValidateServeRequiresReplyTokenSecretInProduction(t *testing.T) {
	cfg := Config{Environment: "production"}
	err := cfg.ValidateServe()
	if err == nil || !strings.Contains(err.Error(), "MANYFORGE_INBOUND_REPLY_TOKEN_SECRET") {
		t.Fatalf("ValidateServe error = %v, want a MANYFORGE_INBOUND_REPLY_TOKEN_SECRET error", err)
	}
	cfg.InboundReplyTokenSecret = []byte("a-production-reply-token-secret-0123")
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("ValidateServe with secret: %v", err)
	}
}

func TestValidateServeToleratesEmptySecretInDevelopment(t *testing.T) {
	if err := (Config{Environment: "development"}).ValidateServe(); err != nil {
		t.Fatalf("development ValidateServe: %v", err)
	}
}

// SECREVIEW-L1: a set reply-token key shorter than 32 bytes is brute-forceable
// offline from one legitimate token, so it is refused in every environment.
func TestValidateServeRejectsShortReplyTokenSecretInEveryEnvironment(t *testing.T) {
	for _, env := range []string{"production", "development", "staging", ""} {
		for _, key := range []string{"x", strings.Repeat("k", 31), "  " + strings.Repeat("k", 30) + "  "} {
			cfg := Config{Environment: env, InboundReplyTokenSecret: []byte(key)}
			err := cfg.ValidateServe()
			if err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
				t.Fatalf("env %q key len %d: ValidateServe error = %v, want a minimum-length error", env, len(key), err)
			}
		}
		cfg := Config{Environment: env, InboundReplyTokenSecret: []byte(strings.Repeat("k", 32))}
		if err := cfg.ValidateServe(); err != nil {
			t.Fatalf("env %q 32-byte key: %v", env, err)
		}
	}
}

// SECREVIEW-L1: an unset dev key is replaced by a random 32-byte key, never left empty.
func TestEnsureReplyTokenKeyGeneratesEphemeralKeyInDevelopment(t *testing.T) {
	cfg := Config{Environment: "development"}
	generated, err := cfg.EnsureReplyTokenKey()
	if err != nil || !generated {
		t.Fatalf("EnsureReplyTokenKey = %v, %v; want generated", generated, err)
	}
	if len(cfg.InboundReplyTokenSecret) != 2*MinReplyTokenKeyBytes {
		t.Fatalf("generated key length = %d, want %d (hex of %d random bytes)", len(cfg.InboundReplyTokenSecret), 2*MinReplyTokenKeyBytes, MinReplyTokenKeyBytes)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("generated key fails ValidateServe: %v", err)
	}
	// Raw random bytes used to fail ValidateServe ~5% of the time when they began
	// or ended in a whitespace byte; every generated key must pass.
	for i := 0; i < 500; i++ {
		c := Config{Environment: "development"}
		if _, err := c.EnsureReplyTokenKey(); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateServe(); err != nil {
			t.Fatalf("generated key %d fails ValidateServe: %v", i, err)
		}
	}
	other := Config{Environment: "development"}
	if _, err := other.EnsureReplyTokenKey(); err != nil {
		t.Fatal(err)
	}
	if string(other.InboundReplyTokenSecret) == string(cfg.InboundReplyTokenSecret) {
		t.Fatal("two generated keys are identical; want random")
	}
}

func TestEnsureReplyTokenKeyKeepsConfiguredKeyAndRefusesUnsetProduction(t *testing.T) {
	key := []byte(strings.Repeat("k", 40))
	cfg := Config{Environment: "development", InboundReplyTokenSecret: key}
	if generated, err := cfg.EnsureReplyTokenKey(); err != nil || generated || string(cfg.InboundReplyTokenSecret) != string(key) {
		t.Fatalf("configured key replaced: generated=%v err=%v", generated, err)
	}
	prod := Config{Environment: "production"}
	if generated, err := prod.EnsureReplyTokenKey(); err == nil || generated || len(prod.InboundReplyTokenSecret) != 0 {
		t.Fatalf("production without key: generated=%v err=%v; want refusal", generated, err)
	}
}

// The migrate Job loads config in production without the reply-token secret, so
// Load itself must keep accepting an empty secret; only the serve path refuses.
func TestLoadInProductionStillAllowsMigrateWithoutReplyTokenSecret(t *testing.T) {
	setOutboundTestEnv(t, "smtp")
	t.Setenv("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET", "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load in production without reply-token secret: %v", err)
	}
}

// A whitespace-only secret (e.g. `echo | kubectl create secret`) is as forgeable as an empty one.
func TestValidateServeRejectsWhitespaceOnlyReplyTokenSecret(t *testing.T) {
	for _, secret := range []string{"   ", "\n", " \t\r\n"} {
		cfg := Config{Environment: "production", InboundReplyTokenSecret: []byte(secret)}
		if err := cfg.ValidateServe(); err == nil {
			t.Fatalf("ValidateServe accepted whitespace-only secret %q", secret)
		}
	}
}
