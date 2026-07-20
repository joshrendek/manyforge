package security_regression

import (
	"strings"
	"testing"
)

// TestCodexModelCatalogIsCurrent pins that the openai_codex model catalog is seeded with the
// current GPT-5.6 lineup and that the retired gpt-5-codex / gpt-5 presets are removed. OpenAI
// retired those slugs — the ChatGPT Codex backend (chatgpt.com/backend-api/codex) no longer
// serves them — so migration 0097 shipped a model picker listing models that no longer exist.
//
// When OpenAI's Codex lineup changes again, add a new refresh migration AND update this pin in the
// same change — or land the live per-plan catalog fetch (manyforge follow-up) that derives the
// list from GET chatgpt.com/backend-api/codex/models and removes the need to hardcode it at all.
func TestCodexModelCatalogIsCurrent(t *testing.T) {
	up := mustRead(t, "../../migrations/0098_codex_model_catalog_refresh.up.sql")
	for _, want := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if !strings.Contains(up, want) {
			t.Errorf("0098 must seed current codex model %q — pin broken, update in the same change if intentional", want)
		}
	}
	// The retired slugs must be actively removed, not just shadowed by an ON CONFLICT no-op.
	if !strings.Contains(up, "DELETE FROM model_pricing") {
		t.Error("0098 must DELETE the retired openai_codex presets (gpt-5-codex, gpt-5) — pin broken, update in the same change if intentional")
	}
}
