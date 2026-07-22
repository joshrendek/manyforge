package ai

import "testing"

func TestRegistryLookupAndCost(t *testing.T) {
	r := NewRegistry()
	r.Register(Model{
		ID: "claude-sonnet-4-6", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	})

	m, ok := r.Lookup("anthropic", "claude-sonnet-4-6")
	if !ok || m.Provider != "anthropic" {
		t.Fatalf("Lookup = (%+v, %v)", m, ok)
	}
	if _, ok := r.Lookup("anthropic", "nope"); ok {
		t.Fatal("unknown model must not resolve")
	}
	// 1,000,000 in + 1,000,000 out = 300 + 1500 = 1800 cents.
	if c := m.CostCents(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Errorf("CostCents = %d, want 1800", c)
	}
	// Exact (no rounding): 500k in @ 300/Mtok = 150_000_000/1e6 = 150 cents exactly.
	if c := m.CostCents(Usage{InputTokens: 500_000}); c != 150 {
		t.Errorf("CostCents(500k in) = %d, want 150", c)
	}
	// 333,333 × 300 = 99,999,900 → ceil to 100 cents (not 99).
	if c := m.CostCents(Usage{InputTokens: 333_333}); c != 100 {
		t.Errorf("CostCents(333333 in) = %d, want 100", c)
	}
}

// TestRegistryProviderScoped: the same model_id under two providers resolves to two
// DISTINCT rows (Lookup is provider-aware), so a $0 openai_codex 'gpt-5' can no longer
// shadow a metered same-named model of another provider. manyforge-6fx.2.
func TestRegistryProviderScoped(t *testing.T) {
	r := NewRegistry()
	r.Register(Model{ID: "gpt-5", Provider: "openai_codex", InputCentsPerMTok: 0, OutputCentsPerMTok: 0})
	r.Register(Model{ID: "gpt-5", Provider: "openai", InputCentsPerMTok: 125, OutputCentsPerMTok: 1000, SupportsTools: true})

	if codex, ok := r.Lookup("openai_codex", "gpt-5"); !ok || codex.InputCentsPerMTok != 0 {
		t.Fatalf("codex gpt-5 = (%+v, %v), want the $0 row", codex, ok)
	}
	metered, ok := r.Lookup("openai", "gpt-5")
	if !ok || metered.InputCentsPerMTok != 125 {
		t.Fatalf("openai gpt-5 = (%+v, %v), want the metered row (no codex $0 shadow)", metered, ok)
	}
	if _, ok := r.Lookup("anthropic", "gpt-5"); ok {
		t.Fatal("gpt-5 must not resolve under an unrelated provider")
	}
	// Two providers claiming one model_id coexist — the sole-model_id PK could not hold both.
	if n := r.Len(); n != 2 {
		t.Fatalf("registry len = %d, want 2 (both provider rows retained)", n)
	}
}
