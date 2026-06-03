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
	// Exact (no rounding): 500k in @ 300/Mtok = 150_000_000/1e6 = 150 cents exactly.
	if c := m.CostCents(Usage{InputTokens: 500_000}); c != 150 {
		t.Errorf("CostCents(500k in) = %d, want 150", c)
	}
	// 333,333 × 300 = 99,999,900 → ceil to 100 cents (not 99).
	if c := m.CostCents(Usage{InputTokens: 333_333}); c != 100 {
		t.Errorf("CostCents(333333 in) = %d, want 100", c)
	}
}
