package agents

import (
	"testing"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestModelRowToAIModel(t *testing.T) {
	row := dbgen.ListModelPricingRow{
		ModelID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMtok: 300, OutputCentsPerMtok: 1500, SupportsTools: true,
	}
	got := modelRowToAIModel(row)
	want := ai.Model{
		ID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	}
	if got != want {
		t.Fatalf("modelRowToAIModel = %+v, want %+v", got, want)
	}
	if c := got.CostCents(ai.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Fatalf("CostCents = %d, want 1800", c)
	}
}
