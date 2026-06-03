package ai

import "testing"

func TestRegistryLookupAndCost(t *testing.T) {
	r := NewRegistry()
	r.Register(Model{
		ID: "claude-sonnet-4-6", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	})

	m, ok := r.Lookup("claude-sonnet-4-6")
	if !ok || m.Provider != "anthropic" {
		t.Fatalf("Lookup = (%+v, %v)", m, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unknown model must not resolve")
	}
	// 1,000,000 in + 1,000,000 out = 300 + 1500 = 1800 cents.
	if c := m.CostCents(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Errorf("CostCents = %d, want 1800", c)
	}
	// Rounds up a partial: 500k in @ 300/Mtok = 150 cents.
	if c := m.CostCents(Usage{InputTokens: 500_000}); c != 150 {
		t.Errorf("CostCents(500k in) = %d, want 150", c)
	}
}
