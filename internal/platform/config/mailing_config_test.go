package config

import (
	"encoding/base64"
	"strings"
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
		cfg.MailingSendEvery != 2*time.Second || cfg.MailingLease != 2*time.Minute ||
		cfg.MailingFanoutGlobal != 1000 || cfg.MailingFanoutPerCampaign != 250 ||
		cfg.MailingRollupBatch != 100 || cfg.MailingMessageDomain != cfg.InboundSystemDomain {
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

func TestProductionAllowsAPIProvidersWithoutSMTP(t *testing.T) {
	for _, provider := range []string{"resend", "ses"} {
		t.Run(provider, func(t *testing.T) {
			setOutboundTestEnv(t, provider)
			t.Setenv("MANYFORGE_OUTBOUND_FROM_EMAIL", "accounts@example.test")
			if provider == "resend" {
				t.Setenv("MANYFORGE_OUTBOUND_RESEND_API_KEY", "test-resend-key")
			} else {
				t.Setenv("MANYFORGE_OUTBOUND_SES_REGION", "us-east-1")
				t.Setenv("MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID", "test-access-key")
				t.Setenv("MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY", "test-secret-key")
			}
			// An unused relay's invalid port must not block an API-only deployment.
			t.Setenv("MANYFORGE_SMTP_PORT", "unused-invalid-port")
			if _, err := Load(); err != nil {
				t.Fatalf("API-only production configuration rejected: %v", err)
			}
		})
	}
}

func TestProductionAllowsUnavailableSMTP(t *testing.T) {
	setOutboundTestEnv(t, "smtp")
	if _, err := Load(); err != nil {
		t.Fatalf("unconfigured shared SMTP must not block per-business API mailing: %v", err)
	}
}

func TestProductionAllowsExplicitlyDisabledOutboundMail(t *testing.T) {
	for _, provider := range []string{"smtp", "resend", "ses"} {
		t.Run(provider, func(t *testing.T) {
			setOutboundTestEnv(t, provider)
			t.Setenv("MANYFORGE_OUTBOUND_MAIL_DISABLED", "true")
			t.Setenv("MANYFORGE_SMTP_HOST", "smtp.example.test")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("disabled mail must not require provider credentials or identity: %v", err)
			}
			if !cfg.OutboundMailDisabled {
				t.Fatal("provider selection must not override explicit outbound disable")
			}
		})
	}
}

func TestOutboundProviderRejectsMissingPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		provider string
		missing  string
	}{
		{"resend", "MANYFORGE_OUTBOUND_FROM_EMAIL"},
		{"resend", "MANYFORGE_OUTBOUND_RESEND_API_KEY"},
		{"ses", "MANYFORGE_OUTBOUND_FROM_EMAIL"},
		{"ses", "MANYFORGE_OUTBOUND_SES_REGION"},
		{"ses", "MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID"},
		{"ses", "MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY"},
	} {
		t.Run(tc.provider+"/"+tc.missing, func(t *testing.T) {
			setOutboundTestEnv(t, tc.provider)
			for key, value := range map[string]string{
				"MANYFORGE_OUTBOUND_FROM_EMAIL":            "private-sender@example.test",
				"MANYFORGE_OUTBOUND_RESEND_API_KEY":        "private-resend-key",
				"MANYFORGE_OUTBOUND_SES_REGION":            "us-east-1",
				"MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID":     "private-access-key",
				"MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY": "private-secret-key",
			} {
				t.Setenv(key, value)
			}
			t.Setenv(tc.missing, "")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("missing provider prerequisite: error = %v", err)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("configuration error disclosed sender or credentials")
			}
		})
	}
}

func TestOutboundProviderRejectsUnknownEvenWhenDisabled(t *testing.T) {
	for _, disabled := range []string{"false", "true"} {
		t.Run(disabled, func(t *testing.T) {
			setOutboundTestEnv(t, "https://private-provider.test/token")
			t.Setenv("MANYFORGE_OUTBOUND_MAIL_DISABLED", disabled)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "MANYFORGE_OUTBOUND_PROVIDER") {
				t.Fatalf("unknown provider: error = %v", err)
			}
			if strings.Contains(err.Error(), "private-provider") {
				t.Fatal("configuration error disclosed provider input")
			}
		})
	}
}

func TestOutboundIdentityRejectsUnsafeValuesWithoutDisclosure(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value string
	}{
		{"MANYFORGE_OUTBOUND_FROM_EMAIL", "private-invalid-email"},
		{"MANYFORGE_OUTBOUND_FROM_EMAIL", "private@example.test\r\nBcc: hidden@example.test"},
		{"MANYFORGE_OUTBOUND_FROM_EMAIL", "private-name <accounts@example.test>"},
		{"MANYFORGE_OUTBOUND_FROM_NAME", "private-name\nBcc: hidden@example.test"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			setOutboundTestEnv(t, "smtp")
			t.Setenv(tc.key, tc.value)
			// Even disabled mail must not accept a configured unsafe identity.
			t.Setenv("MANYFORGE_OUTBOUND_MAIL_DISABLED", "true")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("unsafe identity accepted: error = %v", err)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "hidden") {
				t.Fatal("configuration error disclosed identity input")
			}
		})
	}
}

func setOutboundTestEnv(t *testing.T, provider string) {
	t.Helper()
	t.Setenv("MANYFORGE_ENVIRONMENT", "production")
	t.Setenv("MANYFORGE_OUTBOUND_PROVIDER", provider)
	t.Setenv("MANYFORGE_OUTBOUND_MAIL_DISABLED", "false")
	for _, key := range []string{
		"MANYFORGE_SMTP_HOST", "MANYFORGE_SMTP_PORT", "MANYFORGE_SMTP_USER", "MANYFORGE_SMTP_PASS",
		"MANYFORGE_OUTBOUND_FROM_EMAIL", "MANYFORGE_OUTBOUND_FROM_NAME",
		"MANYFORGE_OUTBOUND_RESEND_API_KEY", "MANYFORGE_OUTBOUND_SES_REGION",
		"MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID", "MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY",
		"MANYFORGE_OUTBOUND_SES_CONFIGURATION_SET",
	} {
		t.Setenv(key, "")
	}
}

func TestOutboundMailDisabledRejectsInvalidBool(t *testing.T) {
	t.Setenv("MANYFORGE_ENVIRONMENT", "production")
	t.Setenv("MANYFORGE_SMTP_HOST", "smtp.example.test")
	t.Setenv("MANYFORGE_OUTBOUND_MAIL_DISABLED", "notabool")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MANYFORGE_OUTBOUND_MAIL_DISABLED") {
		t.Fatalf("invalid disable setting: error = %v", err)
	}
}
