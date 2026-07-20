package agents

import "testing"

func TestFilterCodexPro(t *testing.T) {
	in := []ModelInfo{
		{Provider: "openai_codex", ModelID: "gpt-5.4"},
		{Provider: "openai_codex", ModelID: "gpt-5.4-mini"},
		{Provider: "openai_codex", ModelID: "gpt-5.4-codex-pro"}, // dropped: -pro 403s on ChatGPT auth
		{Provider: "openai", ModelID: "gpt-4o-pro"},              // kept: non-codex -pro is fine
	}
	got := filterCodexPro(in)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ModelID)
	}
	want := []string{"gpt-5.4", "gpt-5.4-mini", "gpt-4o-pro"}
	if len(ids) != len(want) {
		t.Fatalf("filterCodexPro: got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("filterCodexPro[%d]: got %q, want %q (full: %v)", i, ids[i], want[i], ids)
		}
	}
}
