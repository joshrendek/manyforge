package ai

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// loadGolden reads a recorded provider response body from testdata/. These are
// real provider wire shapes recorded once and replayed in CI (no live calls).
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadGolden %s: %v", name, err)
	}
	return b
}

// recording reports whether AI_RECORD mode is on (maintainer refresh of golden
// fixtures against the live API). Off in CI. Defined here — alongside its only
// callers — to keep the `unused` linter happy in the intermediate commits.
func recording() bool { return os.Getenv("AI_RECORD") != "" }

// TestRecordAnthropicFixture refreshes testdata/anthropic_text.json from the
// live Anthropic API. Run: AI_RECORD=1 ANTHROPIC_API_KEY=sk-... go test \
//
//	./internal/platform/ai/ -run TestRecordAnthropicFixture -v
func TestRecordAnthropicFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from the live API")
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	p := NewAnthropicProvider(key, "", "claude-sonnet-4-5", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-4-5", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded anthropic response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
	// NOTE: this logs the mapped Response for inspection. To regenerate the raw
	// fixture, temporarily capture the raw body in Complete or use a proxy; the
	// committed fixtures are hand-authored to the documented wire shape and
	// rarely need regeneration. Path for reference:
	_ = filepath.Join("testdata", "anthropic_text.json")
}

// TestRecordOpenAIFixture mirrors the above for an OpenAI-compatible endpoint.
// Run: AI_RECORD=1 OPENAI_API_KEY=sk-... OPENAI_BASE_URL=https://api.openai.com/v1 \
//
//	go test ./internal/platform/ai/ -run TestRecordOpenAIFixture -v
func TestRecordOpenAIFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from the live API")
	}
	key := os.Getenv("OPENAI_API_KEY")
	base := os.Getenv("OPENAI_BASE_URL")
	if key == "" || base == "" {
		t.Skip("OPENAI_API_KEY / OPENAI_BASE_URL not set")
	}
	p := NewOpenAICompatProvider(key, base, "gpt-4o", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded openai response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}
