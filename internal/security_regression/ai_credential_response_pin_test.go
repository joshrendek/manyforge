package security_regression

import (
	"os"
	"strings"
	"testing"
)

// FINDING: manyforge-1kv — the AI credential HTTP response must never carry the
// API key (sealed_key_ref). Source-level pin: the credentialResp response DTO
// must declare no json key/secret field, and the route group must stay
// agents.configure-gated.
//
// NOTE on scoping: the handler legitimately accepts `json:"api_key"` on the
// write-only createCredential *input* struct (the secret is written in, never
// read back). A file-wide ban would false-fail on that legitimate input field,
// so the secret-absence assertion is scoped to the credentialResp struct body
// (the read-back DTO) — that is the surface that must never serialize a secret.

func TestAICredentialResponseHasNoKeyField(t *testing.T) {
	src, err := os.ReadFile("../agents/credential_handler.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	s := string(src)

	// Isolate the credentialResp struct body — the response DTO surface.
	const marker = "type credentialResp struct {"
	start := strings.Index(s, marker)
	if start < 0 {
		t.Fatalf("credentialResp response DTO no longer exists; cannot pin secret-omission")
	}
	rest := s[start+len(marker):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("could not find end of credentialResp struct body")
	}
	body := rest[:end]

	for _, banned := range []string{`json:"api_key"`, `json:"sealed_key_ref"`, `json:"key"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("credentialResp response DTO must not serialize a secret field, found %q", banned)
		}
	}
}

func TestAICredentialRouteStaysAgentsConfigureGated(t *testing.T) {
	src, err := os.ReadFile("../../cmd/manyforge/main.go")
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "h.credentials.ProtectedRoutes") {
		t.Fatal("credentials handler is no longer mounted")
	}
	if !strings.Contains(s, "cg.Use(h.agentsConfigure)") {
		t.Fatal("credentials route group is no longer gated on agents.configure")
	}
}
