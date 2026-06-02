package observability

import (
	"log/slog"
	"strings"
)

// redactedMarker replaces the value of any credential-bearing attribute.
const redactedMarker = "[REDACTED]"

// sensitiveSubstrings is the credential denylist matched (case-insensitively) as a
// substring of an attribute KEY. Deliberately curated to avoid over-matching benign
// keys: bare "key" and "code" are EXCLUDED (they would scrub blob_key / status_code).
// "private_key" catches dkim_private_key_ref. Extend this list, never loosen it.
var sensitiveSubstrings = []string{
	"secret", "password", "passwd", "token", "private_key",
	"authorization", "api_key", "apikey", "hmac", "credential", "session_id",
}

// isSensitiveKey reports whether an attribute key names a credential-bearing value.
func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveSubstrings {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactSensitive is a slog ReplaceAttr hook: it replaces the value of any
// sensitive-keyed attribute with the redaction marker. slog invokes ReplaceAttr for
// every non-group attribute, including those nested inside groups, so this is a
// structural guard — a future careless log call cannot leak a secret by key. The
// built-in time/level/msg attrs (their keys are not in the denylist) pass through.
func redactSensitive(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		a.Value = slog.StringValue(redactedMarker)
	}
	return a
}
