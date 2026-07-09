package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A huggingface credential points at the operator's own ZeroGPU Space, which serves an
// OpenAI-compatible /v1/chat/completions. These tests pin the two things that make that
// endpoint different from the other openai-compat providers:
//
//  1. base_url is REQUIRED (the Space host is per-user; there is no shared default), and
//  2. model ids carry a "/" and may carry a ":" (Qwen/Qwen3-14B, org/model:sub-provider),
//     which must survive to the wire verbatim.
//
// The transport itself is OpenAICompatProvider — HF returns usage.prompt_tokens /
// completion_tokens like everyone else, so there is no HF-specific parsing. See manyforge-bhx.

func TestHuggingFaceComplete_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		fixture  string
		wantText string
		wantTool string
		wantFin  FinishReason
		wantIn   int
		wantOut  int
	}{
		{"huggingface_text.json", "Hello! How can I help with your ticket?", "", FinishStop, 14, 9},
		{"huggingface_tool_calls.json", "", "get_ticket", FinishToolUse, 47, 15},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			golden := loadGolden(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer hf_test" {
					t.Errorf("Authorization = %q, want %q", got, "Bearer hf_test")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(golden)
			}))
			defer srv.Close()

			p := NewOpenAICompatProvider("hf_test", srv.URL+"/v1", "Qwen/Qwen3-14B", ProviderHuggingFace, srv.Client())
			resp, err := p.Complete(context.Background(), Request{
				Model: "Qwen/Qwen3-14B", MaxTokens: 64,
				Messages: []Message{{Role: RoleUser, Text: "review this diff"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", resp.Text, tc.wantText)
			}
			if resp.FinishReason != tc.wantFin {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tc.wantFin)
			}
			if tc.wantTool != "" {
				if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != tc.wantTool {
					t.Fatalf("ToolCalls = %+v, want one call to %q", resp.ToolCalls, tc.wantTool)
				}
			}
			if resp.Usage.InputTokens != tc.wantIn || resp.Usage.OutputTokens != tc.wantOut {
				t.Errorf("Usage = %+v, want in=%d out=%d", resp.Usage, tc.wantIn, tc.wantOut)
			}
		})
	}
}

// HF model ids are namespaced ("Qwen/Qwen3-14B") and may pin a routed sub-provider with a
// colon ("zai-org/GLM-5.2:fireworks-ai"). Neither is URL-escaped or split — the id is a JSON
// body field, not a path segment — so both must reach the wire byte-for-byte.
func TestHuggingFaceComplete_NamespacedModelIDReachesWireVerbatim(t *testing.T) {
	for _, model := range []string{"Qwen/Qwen3-14B", "zai-org/GLM-5.2:fireworks-ai"} {
		t.Run(model, func(t *testing.T) {
			var gotModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var sent struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Fatalf("request body not JSON: %v", err)
				}
				gotModel = sent.Model
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(loadGolden(t, "huggingface_text.json"))
			}))
			defer srv.Close()

			p := NewOpenAICompatProvider("hf_test", srv.URL+"/v1", model, ProviderHuggingFace, srv.Client())
			if _, err := p.Complete(context.Background(), Request{
				Model: model, MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if gotModel != model {
				t.Fatalf("model on the wire = %q, want %q (slashes/colons must not be mangled)", gotModel, model)
			}
		})
	}
}

// Unlike openrouter, huggingface has no shared endpoint to fall back to: the Space host is
// per-user. A credential without base_url must fail closed rather than silently target
// something else.
func TestNew_HuggingFaceRequiresBaseURL(t *testing.T) {
	_, err := New(Credential{Provider: ProviderHuggingFace, APIKey: "hf_test", Model: "Qwen/Qwen3-14B"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("huggingface with empty base_url err = %v, want Is(ErrBadRequest)", err)
	}
}

func TestNew_HuggingFaceUsesSuppliedBaseURL(t *testing.T) {
	const base = "https://josh-reviewbot.hf.space/v1"
	p, err := New(Credential{Provider: ProviderHuggingFace, APIKey: "hf_test", Model: "Qwen/Qwen3-14B", BaseURL: base})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	oc, ok := p.(*OpenAICompatProvider)
	if !ok {
		t.Fatalf("want the openai-compat provider, got %T", p)
	}
	if oc.baseURL != base {
		t.Fatalf("baseURL = %q, want %q", oc.baseURL, base)
	}
	if oc.providerName != ProviderHuggingFace {
		t.Fatalf("providerName = %q, want %q", oc.providerName, ProviderHuggingFace)
	}
}

// A ZeroGPU Space is a plain chat-completions server with no OpenRouter-style server-side
// tools; injecting them would 400 the request.
func TestBuildOpenAIRequest_ServerTools_NotEmittedForHuggingFace(t *testing.T) {
	req := Request{
		Model:     "Qwen/Qwen3-14B",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "look it up"}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_fetch", AllowedDomains: []string{"docs.sysward.com"}},
		},
	}
	out := buildOpenAIRequest(req, "Qwen/Qwen3-14B", ProviderHuggingFace)
	tools := toolsFromBody(t, out)
	if _, ok := findToolType(tools, "openrouter:web_fetch"); ok {
		t.Fatalf("huggingface must NOT emit openrouter server tools: %+v", tools)
	}
}
