// manyforge-9er: local providers must go through the opencode sandbox, using the
// bundled openai-compatible provider — never the direct-POST path or the Responses API.
//
// Task 8's brief proposed a single consolidated pin here, but auditing the tree first
// found that most of the named invariants are ALREADY pinned by earlier tasks in this
// epic:
//   - entrypoint accepts vllm|ollama and rejects any other LLM_PROVIDER: MF007-PIN-11
//     (mf007_review_redaction_test.go), duplicated intentionally by MF-KUBE-SANDBOX-22
//     (mf_kube_sandbox_test.go) so either pin alone still catches a regression.
//   - reviewLane no longer branches a local credential to a separate host-side call
//     (no isLocalProvider(laneCred.Provider) in reviewLane): MF008-PIN-3
//     (mf008_review_dimensions_pin_test.go). localReview itself (the direct-POST path)
//     was deleted in Task 6 with no caller left in the tree — see MF007-PIN-14's comment
//     in coding_review_pins_test.go, which documents that there is nothing left to grep
//     for; reintroducing `func localReview(` without a call site would be dead code.
//   - the shared sandbox lane's egress/SSRF guard uses privateBaseURLBlocked:
//     TestGithubPRRunJobEgressPreflightPinned (github_pr_trigger_pin_test.go).
//
// The one invariant NOT covered anywhere: that the entrypoint maps a local provider to
// opencode's bundled @ai-sdk/openai-compatible provider (Chat Completions), not the
// built-in "openai" provider (which speaks the Responses API that local servers like LM
// Studio/vLLM/Ollama don't serve). That gap is pinned below — no other pin duplicates it.
package security_regression

import (
	"strings"
	"testing"
)

func TestLocalProvidersMapToBundledOpenAICompatibleProvider(t *testing.T) {
	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	if !strings.Contains(entry, `"npm": "@ai-sdk/openai-compatible",`) {
		t.Error("entrypoint must map the local provider to the bundled @ai-sdk/openai-compatible npm provider — a local model needs Chat Completions, not the Responses API the built-in openai provider speaks")
	}
	if !strings.Contains(entry, `MODEL="local/${LLM_MODEL}"`) {
		t.Error(`entrypoint must select opencode's custom "local" provider id (MODEL="local/${LLM_MODEL}") for vllm/ollama — it must match the "local" key under config's "provider" object and auth.json, never opencode's built-in "openai" provider`)
	}
}
