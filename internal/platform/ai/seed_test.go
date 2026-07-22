package ai

import "testing"

func TestRegisterDefaults(t *testing.T) {
	r := NewRegistry()
	RegisterDefaults(r)

	for _, tc := range []struct{ provider, id string }{
		{"anthropic", "claude-sonnet-4-5"}, {"openai", "gpt-4o"}, {"openai", "gpt-4o-mini"},
	} {
		m, ok := r.Lookup(tc.provider, tc.id)
		if !ok {
			t.Fatalf("model %q not seeded", tc.id)
		}
		if m.InputCentsPerMTok <= 0 || m.OutputCentsPerMTok <= 0 {
			t.Errorf("model %q has non-positive pricing: %+v", tc.id, m)
		}
		if !m.SupportsTools {
			t.Errorf("model %q should support tools", tc.id)
		}
	}

	// Cost math sanity: 1M input + 1M output tokens == (in + out) cents-per-MTok.
	m, _ := r.Lookup("openai", "gpt-4o")
	got := m.CostCents(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	want := m.InputCentsPerMTok + m.OutputCentsPerMTok
	if got != want {
		t.Errorf("CostCents(1M,1M) = %d, want %d", got, want)
	}
}
