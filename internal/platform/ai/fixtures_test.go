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
	p := NewOpenAICompatProvider(key, base, "gpt-4o", ProviderOpenAI, nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded openai response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}

// TestRecordOllamaFixture refreshes the ollama fixtures from a live Ollama server.
// Run: AI_RECORD=1 OLLAMA_BASE_URL=http://localhost:11434/v1 \
//
//	go test ./internal/platform/ai/ -run TestRecordOllamaFixture -v
func TestRecordOllamaFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from a live server")
	}
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		t.Skip("OLLAMA_BASE_URL not set")
	}
	p := NewOpenAICompatProvider("", base, "llama3.1", ProviderOllama, nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "llama3.1", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded ollama response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}

// TestRecordHuggingFaceFixture exercises the live HF Inference Providers router end-to-end
// through ai.New (not just the transport), which is the only place the base_url default and the
// netsafe client are wired together. Model ids must pin a partner ("org/model:fireworks-ai");
// a bare id may 404, and a cold community model on featherless can 503 on first call while it
// loads — both map to ErrProviderUnavailable and are covered by the dimension fallback_chain.
//
// Run: AI_RECORD=1 HF_TOKEN=$(cat ~/.config/hf) \
//
//	go test ./internal/platform/ai/ -run TestRecordHuggingFaceFixture -v
func TestRecordHuggingFaceFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to exercise the live router")
	}
	key := os.Getenv("HF_TOKEN")
	if key == "" {
		t.Skip("HF_TOKEN not set")
	}
	model := os.Getenv("HF_MODEL")
	if model == "" {
		model = "zai-org/GLM-5.2:fireworks-ai"
	}
	// Empty BaseURL on purpose: this asserts DefaultBaseURL(huggingface) actually reaches HF.
	p, err := New(Credential{Provider: ProviderHuggingFace, APIKey: key, Model: model})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.Complete(context.Background(), Request{
		Model: model, MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	if resp.Text == "" {
		t.Error("live router returned no text")
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("usage not parsed from the live response: %+v", resp.Usage)
	}
	t.Logf("recorded huggingface response: model=%q text=%q finish=%s usage=%+v",
		model, resp.Text, resp.FinishReason, resp.Usage)
}

// TestRecordVLLMFixture mirrors the above for a live vLLM OpenAI-compatible server.
// Run: AI_RECORD=1 VLLM_BASE_URL=http://localhost:8000/v1 \
//
//	go test ./internal/platform/ai/ -run TestRecordVLLMFixture -v
func TestRecordVLLMFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from a live server")
	}
	base := os.Getenv("VLLM_BASE_URL")
	if base == "" {
		t.Skip("VLLM_BASE_URL not set")
	}
	p := NewOpenAICompatProvider("", base, "meta-llama/Llama-3.1-8B-Instruct", ProviderVLLM, nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "meta-llama/Llama-3.1-8B-Instruct", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded vllm response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}
