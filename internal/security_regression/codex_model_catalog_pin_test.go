package security_regression

import (
	"strings"
	"testing"
)

// TestCodexModelCatalogIsCurrent pins that the openai_codex model catalog is seeded with the
// current models OpenAI's ChatGPT Codex backend actually serves and that the retired gpt-5-codex /
// gpt-5 presets are removed. Migration 0097 shipped gpt-5-codex/gpt-5, which OpenAI retired, so the
// picker listed models that no longer exist. The seeded values (gpt-5.4, gpt-5.4-mini) were
// verified against OpenAI's live per-plan endpoint (GET chatgpt.com/backend-api/codex/models) with
// a real ChatGPT token — not guessed.
//
// This list is per-plan and drifts; when it changes, update this pin + the migration in the same
// change — or land the live per-plan catalog fetch (manyforge follow-up), which removes the need to
// hardcode it at all.
func TestCodexModelCatalogIsCurrent(t *testing.T) {
	up := mustRead(t, "../../migrations/0098_codex_model_catalog_refresh.up.sql")
	// The full per-plan set a current client_version returns (verified live): the gpt-5.6 flagship
	// family plus the gpt-5.5 / gpt-5.4 tail. Spot-check the ends of that range.
	for _, want := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini"} {
		if !strings.Contains(up, want) {
			t.Errorf("0098 must seed current codex model %q — pin broken, update in the same change if intentional", want)
		}
	}
	// The retired slugs must be actively removed, not just shadowed by an ON CONFLICT no-op.
	if !strings.Contains(up, "DELETE FROM model_pricing") {
		t.Error("0098 must DELETE the retired openai_codex presets (gpt-5-codex, gpt-5) — pin broken, update in the same change if intentional")
	}
}
