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
