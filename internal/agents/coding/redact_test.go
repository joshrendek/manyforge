package coding

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestRedactSecrets_ExactKnown(t *testing.T) {
	out := redactSecrets("auth failed for key abcd1234efgh5678 now", "abcd1234efgh5678")
	if strings.Contains(out, "abcd1234efgh5678") {
		t.Fatalf("known secret not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected marker: %s", out)
	}
}

func TestRedactSecrets_ShortKnownIgnored(t *testing.T) {
	// A <8-char known value must not be replaced (would mangle normal text).
	if out := redactSecrets("the cat sat", "cat"); out != "the cat sat" {
		t.Fatalf("short known should be ignored: %s", out)
	}
}

func TestRedactSecrets_Patterns(t *testing.T) {
	for _, c := range []string{
		"key sk-abcdefghij0123456789ABCD end",
		"token ghp_0123456789abcdefghij0123456789abcdef end",
		"Authorization: Bearer abcdef0123456789xyz",
	} {
		if out := redactSecrets(c); !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("pattern not redacted: %q -> %q", c, out)
		}
	}
}

func TestRedactSecrets_NoFalsePositive(t *testing.T) {
	in := "The function returns nil on error; consider wrapping it."
	if out := redactSecrets(in); out != in {
		t.Fatalf("false positive redaction: %q", out)
	}
}

func TestRedactDoc(t *testing.T) {
	secret := "sk-LIVEKEY0123456789abcdef"
	doc := FindingsDoc{
		Summary:  "leaked " + secret,
		Findings: []connectors.Finding{{Title: "t " + secret, Detail: "d " + secret}},
	}
	redactDoc(&doc, secret)
	if strings.Contains(doc.Summary, secret) ||
		strings.Contains(doc.Findings[0].Title, secret) ||
		strings.Contains(doc.Findings[0].Detail, secret) {
		t.Fatalf("doc not fully redacted: %+v", doc)
	}
}
