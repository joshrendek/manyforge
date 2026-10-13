package config

import (
	"os"
	"slices"
	"testing"
)

// SECREVIEW-M6: Authentication-Results are trusted only from configured
// authserv-ids. Unset defaults to Resend's receiving hop (Amazon SES).
func TestInboundTrustedAuthservIDsDefault(t *testing.T) {
	// t.Setenv first so the original value is restored, then truly unset it.
	t.Setenv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS", "")
	if err := os.Unsetenv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !slices.Equal(cfg.InboundTrustedAuthservIDs, []string{"amazonses.com"}) {
		t.Fatalf("default trusted authserv-ids = %q, want [amazonses.com]", cfg.InboundTrustedAuthservIDs)
	}
}

func TestInboundTrustedAuthservIDsOverride(t *testing.T) {
	t.Setenv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS", " AmazonSES.com, mx.Example.test ,,amazonses.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"amazonses.com", "mx.example.test"}; !slices.Equal(cfg.InboundTrustedAuthservIDs, want) {
		t.Fatalf("trusted authserv-ids = %q, want %q (trimmed, lower-cased, de-duplicated)", cfg.InboundTrustedAuthservIDs, want)
	}
}

// An explicitly empty value means "trust none": every verdict is unknown.
func TestInboundTrustedAuthservIDsExplicitlyEmptyTrustsNone(t *testing.T) {
	t.Setenv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.InboundTrustedAuthservIDs) != 0 {
		t.Fatalf("trusted authserv-ids = %q, want none", cfg.InboundTrustedAuthservIDs)
	}
}

func TestInboundTrustedAuthservIDsRejectsMalformed(t *testing.T) {
	for _, v := range []string{"amazonses.com; spf=pass", "two words", "(comment)"} {
		t.Setenv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS", v)
		if _, err := Load(); err == nil {
			t.Fatalf("%q: expected a malformed authserv-id to fail", v)
		}
	}
}
