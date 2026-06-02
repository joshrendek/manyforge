package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"secret", "InboundWebhookSecret", "password", "passwd",
		"token", "access_token", "refresh_token", "dkim_private_key_ref",
		"private_key", "authorization", "api_key", "apiKey", "hmac",
		"credential", "session_id",
	}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = false, want true", k)
		}
	}
	safe := []string{"business_id", "blob_key", "dkim_domain", "dkim_selector",
		"status_code", "error_code", "message_id", "topic", "provider"}
	for _, k := range safe {
		if isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = true, want false (over-redaction)", k)
		}
	}
}

func TestRedactScrubsSensitiveAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{ReplaceAttr: redactSensitive}))

	logger.Info("send attempt",
		"webhook_secret", "s3cr3t-value",
		"dkim_private_key_ref", "vault://abc123",
		"token", "tok_live_xyz",
		"business_id", "biz-42",
		"blob_key", "tenant/obj-1",
	)
	out := buf.String()

	// Secrets must be gone, replaced by the marker.
	for _, leak := range []string{"s3cr3t-value", "vault://abc123", "tok_live_xyz"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked secret %q:\n%s", leak, out)
		}
	}
	if n := strings.Count(out, "[REDACTED]"); n != 3 {
		t.Errorf("want 3 [REDACTED] markers, got %d:\n%s", n, out)
	}
	// Safe values must survive.
	for _, keep := range []string{"biz-42", "tenant/obj-1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("log dropped safe value %q:\n%s", keep, out)
		}
	}
}
