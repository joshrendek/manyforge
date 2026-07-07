package security_regression

import (
	"strings"
	"testing"
)

// MF007-PIN-11: secrets must be redacted before they can reach the stored
// last_error/audit (via sandboxStderrTail) or the posted/stored review doc (via
// redactDoc), and the sandbox entrypoint must validate the provider allowlist
// (openrouter|anthropic|openai select the built-in opencode SDK; vllm|ollama are
// local OpenAI-compatible servers routed through the bundled openai-compatible
// provider — any other value still falls through to the `exit 2` rejection below).
// Source pins — a refactor that drops these must update this file in the same change.
func TestReviewOutputRedaction(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")
	if !strings.Contains(svc, "func sandboxStderrTail(stderr []byte, secrets ...string)") {
		t.Fatal("sandboxStderrTail must take secrets to redact (MF007-PIN-11)")
	}
	if !strings.Contains(svc, "redactSecrets(s, secrets...)") {
		t.Fatal("sandboxStderrTail must redact its tail via redactSecrets (MF007-PIN-11)")
	}
	if !strings.Contains(svc, "redactDoc(&doc, cred.APIKey, rc.Credential.APIToken)") {
		t.Fatal("model doc must be redacted before posting/storing (MF007-PIN-11)")
	}

	red := mustRead(t, "../agents/coding/redact.go")
	if !strings.Contains(red, "func redactSecrets(") || !strings.Contains(red, "func redactDoc(") {
		t.Fatal("redactSecrets/redactDoc must exist (MF007-PIN-11)")
	}

	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	if !strings.Contains(entry, "openrouter|anthropic|openai) LLM_LOCAL=0 ;;") {
		t.Fatal("entrypoint must validate the built-in-provider allowlist (MF007-PIN-11)")
	}
	if !strings.Contains(entry, "vllm|ollama)                 LLM_LOCAL=1 ;;") {
		t.Fatal("entrypoint must validate the local-provider allowlist (MF007-PIN-11)")
	}
	if !strings.Contains(entry, `*) echo "entrypoint: unsupported LLM_PROVIDER='${LLM_PROVIDER:-}'" >&2; exit 2 ;;`) {
		t.Fatal("entrypoint must reject any LLM_PROVIDER outside the allowlist (MF007-PIN-11)")
	}
}
