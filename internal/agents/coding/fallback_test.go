package coding

import "testing"

func TestReviewFallbackModel(t *testing.T) {
	cases := map[string]string{
		"openrouter": "google/gemini-2.5-flash",
		"anthropic":  "claude-sonnet-4-6",
		"openai":     "gpt-4o-mini",
		"ollama":     "",
		"vllm":       "",
		"unknown":    "",
		"":           "",
	}
	for provider, want := range cases {
		if got := reviewFallbackModel(provider); got != want {
			t.Errorf("reviewFallbackModel(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestEffectiveReviewModel(t *testing.T) {
	type tc struct {
		provider, configured string
		attempts             int
		want                 string
	}
	for _, c := range []tc{
		{"openrouter", "google/gemini-2.5-pro", 1, "google/gemini-2.5-pro"}, // first attempt → configured
		{"openrouter", "google/gemini-2.5-pro", 2, "google/gemini-2.5-flash"}, // retry → fallback
		{"openrouter", "google/gemini-2.5-pro", 3, "google/gemini-2.5-flash"}, // later retry → fallback
		{"ollama", "qwen2.5-coder:14b", 2, "qwen2.5-coder:14b"},               // no fallback → configured
		{"openrouter", "google/gemini-2.5-flash", 2, "google/gemini-2.5-flash"}, // already fallback → unchanged
	} {
		if got := effectiveReviewModel(c.provider, c.configured, c.attempts); got != c.want {
			t.Errorf("effectiveReviewModel(%q,%q,%d) = %q, want %q", c.provider, c.configured, c.attempts, got, c.want)
		}
	}
}
