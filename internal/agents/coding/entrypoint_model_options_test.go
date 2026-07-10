package coding

import (
	"os"
	"strings"
	"testing"
)

// The sandbox entrypoint sets an output-token budget per opencode branch. The two numbers are
// tuned for different classes of model and MUST NOT be collapsed:
//
//   - builtin (openrouter/anthropic/openai): 32000. A reasoning model burns ~9k reasoning
//     tokens before emitting a character; an 8192 cap truncates the findings JSON mid-answer
//     and ParseFindings fails (manyforge-6h1).
//   - compat (vllm/ollama): 8192, tuned for on-host small models, some of which reject a
//     max_tokens above their configured context.
//
// huggingface is compat-mechanism but reaches the HF router's frontier reasoning models, so it
// needs the builtin branch's budget. It shipped with 8192 and would have truncated on the very
// first review with the obvious model (zai-org/GLM-5.2:fireworks-ai). See manyforge-bhx.
func TestEntrypointCompatBudgetIsProviderAware(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/sandbox/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	entry := string(b)

	for _, want := range []string{
		"huggingface) COMPAT_MAX_TOKENS=32000 ;;",
		"*)           COMPAT_MAX_TOKENS=8192 ;;",
		`"max_tokens": '"${COMPAT_MAX_TOKENS}"'`,
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("entrypoint.sh must contain %q — the compat branch's output-token budget is\n"+
				"provider-aware on purpose (manyforge-6h1/manyforge-bhx); update this pin only if\n"+
				"the refactor is intentional", want)
		}
	}

	// A literal 8192 must never reappear inside the compat config block: that is exactly the
	// regression this pin exists to catch.
	if strings.Contains(entry, `"max_tokens": 8192 }`) {
		t.Error("entrypoint.sh hardcodes max_tokens 8192 in the compat provider config again — " +
			"a huggingface reasoning model will truncate its findings JSON (manyforge-6h1)")
	}

	// The built-in branch keeps its own generous budget.
	if !strings.Contains(entry, `MODEL_OPTIONS='"max_tokens": 32000'`) {
		t.Error("entrypoint.sh: the built-in branch must keep max_tokens 32000 (manyforge-6h1)")
	}
}
