package config

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestMailingMasterKey(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("MANYFORGE_PUBLIC_BASE_URL", "https://hub.example.test")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MailingMasterKey) != 32 {
		t.Fatalf("MailingMasterKey len = %d", len(cfg.MailingMasterKey))
	}
}

func TestMailingRequiresPublicBaseURL(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err := Load(); err == nil {
		t.Fatal("expected mailing without a public base URL to fail")
	}
}

func TestMailingConfigDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.MailingRateRPS != 10 || cfg.MailingRateBurst != 50 || cfg.MailingSendBatch != 100 ||
		cfg.MailingSendEvery != 2*time.Second || cfg.MailingLease != 2*time.Minute || cfg.MailingMessageDomain != cfg.InboundSystemDomain {
		t.Fatalf("mailing defaults = %#v", cfg)
	}
	t.Setenv("MANYFORGE_MAILING_RATE_RPS", "4.5")
	t.Setenv("MANYFORGE_MAILING_RATE_BURST", "12")
	t.Setenv("MANYFORGE_MAILING_SEND_BATCH", "25")
	t.Setenv("MANYFORGE_MAILING_SEND_EVERY", "3s")
	t.Setenv("MANYFORGE_MAILING_LEASE", "90s")
	t.Setenv("MANYFORGE_MAILING_MESSAGE_DOMAIN", "mail.example.com")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load overrides: %v", err)
	}
	if cfg.MailingRateRPS != 4.5 || cfg.MailingRateBurst != 12 || cfg.MailingSendBatch != 25 ||
		cfg.MailingSendEvery != 3*time.Second || cfg.MailingLease != 90*time.Second || cfg.MailingMessageDomain != "mail.example.com" {
		t.Fatalf("mailing overrides = %#v", cfg)
	}
}

func TestMailingConfigRejectsInvalidBounds(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_SEND_BATCH", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected zero batch to fail")
	}
}

func TestMailingConfigRejectsInvalidMessageDomain(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MESSAGE_DOMAIN", "mail example.com")
	if _, err := Load(); err == nil {
		t.Fatal("expected a message domain containing spaces to fail")
	}
}

func TestMailingMasterKeyRejectsWrongLength(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 31)))
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid key error")
	}
}
