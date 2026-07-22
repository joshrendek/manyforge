// Finding: manyforge-6fx.2 — model_pricing pricing key must be provider-aware.
// The original sole model_id PRIMARY KEY (0038) let one model_id be claimed globally by any
// provider (0097 first seeded ('gpt-5','openai_codex',0,0); 0098 later swapped the codex catalog
// to gpt-5.4/5.5/5.6-*, all $0): a metered same-named model of another provider would be dropped
// by ON CONFLICT (model_id) DO NOTHING and any run with that id would resolve to the $0 codex row.
// The fix widens the DB PK to (provider, model_id) and keys ai.Registry the same way. These pins
// fail loudly if either half is reverted (they must move together: a composite DB PK with a
// provider-blind in-memory registry would collide non-deterministically).
package security_regression

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/ai"
)

// TestPin_ModelPricingCompositePK: the schema and migration 0099 must key pricing on
// (provider, model_id), not model_id alone.
func TestPin_ModelPricingCompositePK(t *testing.T) {
	schema := mustRead(t, "../../db/schema.sql")
	if !strings.Contains(schema, "PRIMARY KEY (provider, model_id)") {
		t.Error("db/schema.sql model_pricing must use PRIMARY KEY (provider, model_id) — 6fx.2 pin broken, update in the same change if intentional")
	}
	// The old sole-model_id PK must be gone from the table definition.
	if strings.Contains(schema, "model_id              text PRIMARY KEY") {
		t.Error("db/schema.sql model_pricing still declares model_id as the sole PRIMARY KEY — provider-blind pricing regression (6fx.2)")
	}
	up := mustRead(t, "../../migrations/0099_model_pricing_provider_pk.up.sql")
	if !strings.Contains(up, "PRIMARY KEY (provider, model_id)") {
		t.Error("migration 0099 must ADD PRIMARY KEY (provider, model_id) — 6fx.2 pin broken")
	}
}

// TestPin_RegistryLookupIsProviderScoped: ai.Registry must disambiguate the same model_id
// across providers, so a $0 codex model cannot shadow a metered same-named model. This is
// the runtime half of the composite-PK fix; if Lookup ever goes provider-blind again the
// two rows collide (last-write-wins) and this pin catches it.
func TestPin_RegistryLookupIsProviderScoped(t *testing.T) {
	reg := ai.NewRegistry()
	reg.Register(ai.Model{ID: "gpt-5", Provider: "openai_codex", InputCentsPerMTok: 0, OutputCentsPerMTok: 0})
	reg.Register(ai.Model{ID: "gpt-5", Provider: "openai", InputCentsPerMTok: 125, OutputCentsPerMTok: 1000})

	metered, ok := reg.Lookup("openai", "gpt-5")
	if !ok || metered.InputCentsPerMTok != 125 {
		t.Fatalf("openai gpt-5 = (%+v, %v), want the metered row — codex $0 must not shadow it (6fx.2)", metered, ok)
	}
	if reg.Len() != 2 {
		t.Fatalf("registry len = %d, want 2 — a provider-blind key would drop one row (6fx.2)", reg.Len())
	}
}
