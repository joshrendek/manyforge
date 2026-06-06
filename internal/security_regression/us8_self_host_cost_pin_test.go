// Finding: US8 / Spec 003 §2 — a self-hosted model absent from the model_pricing
// catalog costs 0 and the run proceeds (self-hosting has no marginal token cost).
// A regression that fails-loud or mischarges unknown models breaks here.
// See manyforge-deo.9.
package security_regression

import (
	"testing"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/ai"
)

func TestUS8_UnknownSelfHostModelCostsZero(t *testing.T) {
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)
	cost := agents.NewRegistryCostFn(reg, nil)
	if c := cost("qwen2.5:32b", ai.Usage{InputTokens: 50_000, OutputTokens: 50_000}); c != 0 {
		t.Fatalf("unknown self-host model cost = %d, want 0", c)
	}
}
