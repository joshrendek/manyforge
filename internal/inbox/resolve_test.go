package inbox

import "testing"

// TestNormalizeRecipientPreservesPlusTokenCase pins the second root cause of
// manyforge-btv: the reply token is case-sensitive base64url (RawURLEncoding), so
// normalizeRecipient must lowercase ONLY the routing key (local-part + domain) and
// return the plus/VERP segment with its case intact. Lowercasing the token corrupts
// it, VerifyReplyToken (via hintTicket) rejects it, and the reply-token threading
// fallback dies before the id ever reaches SQL.
func TestNormalizeRecipientPreservesPlusTokenCase(t *testing.T) {
	// Mixed-case payload using the exact alphabet SignReplyToken emits: base64url
	// (A–Z a–z 0–9 - _) with the "id.sig" dot separator.
	const token = "AbCdEf-_12.GhIjKl-_34"
	norm, got := normalizeRecipient("Support+" + token + "@Inbound.Example.COM")

	if got != token {
		t.Errorf("plusToken = %q, want %q (token case must be preserved verbatim)", got, token)
	}
	if want := "support@inbound.example.com"; norm != want {
		t.Errorf("normalized = %q, want %q (routing key must be lowercased)", norm, want)
	}
}

// TestNormalizeRecipientLowercasesRoutingKey is a characterization test for the
// no-token path: an address with no '+' segment lowercases the whole routing key
// and returns an empty token. This behavior must survive the case-preservation fix.
func TestNormalizeRecipientLowercasesRoutingKey(t *testing.T) {
	norm, token := normalizeRecipient("  Support@Inbound.Example.COM  ")
	if token != "" {
		t.Errorf("plusToken = %q, want empty (no '+' segment)", token)
	}
	if want := "support@inbound.example.com"; norm != want {
		t.Errorf("normalized = %q, want %q", norm, want)
	}
}
