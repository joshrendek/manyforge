package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildOpenAIRequest(t *testing.T) {
	req := Request{
		Model:       "gpt-4o",
		System:      "you are helpful",
		MaxTokens:   512,
		Temperature: 0.5,
		Tools: []ToolDef{{
			Name: "get_ticket", Description: "fetch a ticket",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Text: "look up t-42"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t-42"}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "call_1", Content: "open"},
			}},
		},
	}

	out := buildOpenAIRequest(req, "gpt-4o", ProviderOpenAI)

	if out.Model != "gpt-4o" || out.MaxTokens != 512 {
		t.Fatalf("scalars wrong: %+v", out)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("want 1 tool, got %+v", out.Tools)
	}
	ft, ok := out.Tools[0].(openAITool)
	if !ok || ft.Type != "function" || ft.Function.Name != "get_ticket" || string(ft.Function.Parameters) != `{"type":"object"}` {
		t.Fatalf("tools wrong: %+v", out.Tools)
	}
	// system message is prepended
	if len(out.Messages) != 4 {
		t.Fatalf("want 4 messages (system+user+assistant+tool), got %d: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "you are helpful" {
		t.Fatalf("system msg wrong: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "look up t-42" {
		t.Fatalf("user msg wrong: %+v", out.Messages[1])
	}
	// assistant tool call -> arguments is a JSON STRING
	if out.Messages[2].Role != "assistant" || len(out.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant msg wrong: %+v", out.Messages[2])
	}
	atc := out.Messages[2].ToolCalls[0]
	if atc.ID != "call_1" || atc.Type != "function" || atc.Function.Name != "get_ticket" || atc.Function.Arguments != `{"id":"t-42"}` {
		t.Fatalf("assistant tool_call wrong: %+v", atc)
	}
	// tool result -> role:tool message
	if out.Messages[3].Role != "tool" || out.Messages[3].ToolCallID != "call_1" || out.Messages[3].Content != "open" {
		t.Fatalf("tool msg wrong: %+v", out.Messages[3])
	}
}

func TestParseOpenAIResponse_Text(t *testing.T) {
	body := []byte(`{
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
	}`)
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "hi there" || resp.FinishReason != FinishStop {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

func TestParseOpenAIResponse_ToolCalls(t *testing.T) {
	body := []byte(`{
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_9","type":"function","function":{"name":"get_ticket","arguments":"{\"id\":\"t-42\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":28,"completion_tokens":14,"total_tokens":42}
	}`)
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.FinishReason != FinishToolUse {
		t.Fatalf("finish wrong: %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" || resp.ToolCalls[0].Name != "get_ticket" || string(resp.ToolCalls[0].Args) != `{"id":"t-42"}` {
		t.Fatalf("tool call wrong: %+v", resp.ToolCalls)
	}
}

func TestParseOpenAIResponse_MultipleToolCalls(t *testing.T) {
	body := []byte(`{
		"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_ticket","arguments":"{\"id\":\"t-1\"}"}},
			{"id":"call_2","type":"function","function":{"name":"set_priority","arguments":"{\"p\":\"high\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":5,"completion_tokens":5}
	}`)
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "get_ticket" || resp.ToolCalls[1].Name != "set_priority" {
		t.Fatalf("want 2 tool calls get_ticket+set_priority, got %+v", resp.ToolCalls)
	}
}

func TestParseOpenAIResponse_FinishReasons(t *testing.T) {
	cases := map[string]FinishReason{
		"stop":       FinishStop,
		"tool_calls": FinishToolUse,
		"length":     FinishLength,
		"weird":      FinishOther,
	}
	for raw, want := range cases {
		body := []byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"` + raw + `"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		resp, err := parseOpenAIResponse(body)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if resp.FinishReason != want {
			t.Errorf("finish_reason %q -> %q, want %q", raw, resp.FinishReason, want)
		}
	}
}

func TestParseOpenAIResponse_NoChoices(t *testing.T) {
	_, err := parseOpenAIResponse([]byte(`{"choices":[],"usage":{}}`))
	if err == nil {
		t.Fatal("want error on empty choices")
	}
}

func TestOpenAIComplete_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		fixture  string
		wantText string
		wantTool string
		wantFin  FinishReason
	}{
		{"openai_text.json", "Hello! How can I help with your ticket?", "", FinishStop},
		{"openai_tool_calls.json", "", "get_ticket", FinishToolUse},
		{"ollama_text.json", "Sure — I can help with that ticket.", "", FinishStop},
		{"ollama_tool_calls.json", "", "set_priority", FinishToolUse},
		{"vllm_text.json", "Happy to help with this ticket.", "", FinishStop},
		{"vllm_tool_calls.json", "", "set_status", FinishToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			golden := loadGolden(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer sk-test" {
					t.Errorf("Authorization = %q, want Bearer sk-test", r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				var sent openAIReq
				if err := json.Unmarshal(body, &sent); err != nil || sent.Model == "" || sent.MaxTokens == 0 {
					t.Errorf("unexpected request shape: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(golden)
			}))
			defer srv.Close()

			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", ProviderOpenAI, srv.Client())
			resp, err := p.Complete(context.Background(), Request{
				Model: "gpt-4o", MaxTokens: 256,
				Messages: []Message{{Role: RoleUser, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != tc.wantText || resp.FinishReason != tc.wantFin {
				t.Fatalf("resp = %+v", resp)
			}
			if tc.wantTool == "" && len(resp.ToolCalls) != 0 {
				t.Fatalf("want no tool calls, got %+v", resp.ToolCalls)
			}
			if tc.wantTool != "" && (len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != tc.wantTool) {
				t.Fatalf("want tool %q, got %+v", tc.wantTool, resp.ToolCalls)
			}
		})
	}
}

func TestOpenAIComplete_NoKeyOmitsAuth(t *testing.T) {
	// Ollama / vLLM: empty key -> no Authorization header.
	golden := loadGolden(t, "openai_text.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header for keyless provider, got %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(golden)
	}))
	defer srv.Close()
	p := NewOpenAICompatProvider("", srv.URL+"/v1", "llama3", ProviderOllama, srv.Client())
	if _, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestOpenAIComplete_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"500_server_error", http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`, ErrProviderUnavailable},
		{"429_rate_limit", http.StatusTooManyRequests, `{"error":{"message":"rate limited","type":"rate_limit"}}`, ErrProviderUnavailable},
		{"401_bad_key", http.StatusUnauthorized, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, ErrBadRequest},
		{"400_context_length", http.StatusBadRequest, `{"error":{"message":"too many tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`, ErrContextLength},
		{"400_missing_field", http.StatusBadRequest, `{"error":{"message":"missing field","type":"invalid_request_error"}}`, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", ProviderOpenAI, srv.Client())
			_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: status %d -> err %v, want Is(%v)", tc.name, tc.status, err, tc.want)
			}
		})
	}
}

func TestOpenAIName(t *testing.T) {
	if NewOpenAICompatProvider("k", "http://x/v1", "m", ProviderOpenAI, http.DefaultClient).Name() != "openai-compat" {
		t.Fatal("Name() != openai-compat")
	}
}

// toolsFromBody marshals a built request, re-decodes its "tools" array as a list
// of generic maps, and returns them so server-tool shapes can be inspected.
func toolsFromBody(t *testing.T, out openAIReq) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var decoded struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return decoded.Tools
}

func findToolType(tools []map[string]any, typ string) (map[string]any, bool) {
	for _, tl := range tools {
		if tl["type"] == typ {
			return tl, true
		}
	}
	return nil, false
}

func TestBuildOpenAIRequest_ServerTools_OpenRouterWebFetch(t *testing.T) {
	req := Request{
		Model:     "openrouter/auto",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "look it up"}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_fetch", AllowedDomains: []string{"docs.sysward.com"}},
		},
	}
	out := buildOpenAIRequest(req, "openrouter/auto", ProviderOpenRouter)
	tools := toolsFromBody(t, out)
	tl, ok := findToolType(tools, "openrouter:web_fetch")
	if !ok {
		t.Fatalf("web_fetch tool not emitted: %+v", tools)
	}
	params, ok := tl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing/not object: %+v", tl)
	}
	domains, ok := params["allowed_domains"].([]any)
	if !ok || len(domains) != 1 || domains[0] != "docs.sysward.com" {
		t.Fatalf("allowed_domains wrong: %+v", params)
	}
}

func TestBuildOpenAIRequest_ServerTools_NotEmittedForNonOpenRouter(t *testing.T) {
	req := Request{
		Model:     "gpt-4o",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "look it up"}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_fetch", AllowedDomains: []string{"docs.sysward.com"}},
		},
	}
	for _, provider := range []string{ProviderOpenAI, ProviderOllama, ProviderVLLM} {
		t.Run(provider, func(t *testing.T) {
			out := buildOpenAIRequest(req, "m", provider)
			tools := toolsFromBody(t, out)
			if _, ok := findToolType(tools, "openrouter:web_fetch"); ok {
				t.Fatalf("provider %q must NOT emit openrouter server tool: %+v", provider, tools)
			}
		})
	}
}

func TestBuildOpenAIRequest_ServerTools_WithFunctionTools(t *testing.T) {
	req := Request{
		Model:     "openrouter/auto",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "x"}},
		Tools: []ToolDef{{
			Name: "get_ticket", Description: "fetch a ticket",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_fetch", AllowedDomains: []string{"docs.sysward.com"}},
		},
	}
	out := buildOpenAIRequest(req, "openrouter/auto", ProviderOpenRouter)
	tools := toolsFromBody(t, out)
	// function tool present, shape unchanged (nested "function" object).
	fn, ok := findToolType(tools, "function")
	if !ok {
		t.Fatalf("function tool missing: %+v", tools)
	}
	fnObj, ok := fn["function"].(map[string]any)
	if !ok || fnObj["name"] != "get_ticket" {
		t.Fatalf("function tool shape changed: %+v", fn)
	}
	// server tool also present, and has NO nested "function" object.
	wf, ok := findToolType(tools, "openrouter:web_fetch")
	if !ok {
		t.Fatalf("web_fetch tool missing: %+v", tools)
	}
	if _, has := wf["function"]; has {
		t.Fatalf("server tool must not have a function object: %+v", wf)
	}
}

func TestBuildOpenAIRequest_ServerTools_WebSearch(t *testing.T) {
	req := Request{
		Model:     "openrouter/auto",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "x"}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_search"},
		},
	}
	out := buildOpenAIRequest(req, "openrouter/auto", ProviderOpenRouter)
	tools := toolsFromBody(t, out)
	tl, ok := findToolType(tools, "openrouter:web_search")
	if !ok {
		t.Fatalf("web_search tool not emitted: %+v", tools)
	}
	params, ok := tl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing: %+v", tl)
	}
	if params["engine"] != "auto" {
		t.Fatalf("engine wrong: %+v", params)
	}
	if mr, ok := params["max_results"].(float64); !ok || mr != 5 {
		t.Fatalf("max_results wrong: %+v", params)
	}
}

func TestBuildOpenAIRequest_ServerTools_WebFetchEmptyDomainsOmitted(t *testing.T) {
	req := Request{
		Model:     "openrouter/auto",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "x"}},
		ServerTools: []ServerToolDef{
			{Type: "openrouter:web_fetch"},
		},
	}
	out := buildOpenAIRequest(req, "openrouter/auto", ProviderOpenRouter)
	tools := toolsFromBody(t, out)
	tl, ok := findToolType(tools, "openrouter:web_fetch")
	if !ok {
		t.Fatalf("web_fetch tool not emitted: %+v", tools)
	}
	// emitted, but WITHOUT allowed_domains when empty.
	if params, ok := tl["parameters"].(map[string]any); ok {
		if _, has := params["allowed_domains"]; has {
			t.Fatalf("empty AllowedDomains must omit allowed_domains: %+v", params)
		}
	}
}

// Security pin (CLAUDE.md): raw upstream body must NEVER reach the caller's error.
func TestOpenAIComplete_DoesNotLeakUpstreamBody(t *testing.T) {
	const secret = "SUPER_SECRET_UPSTREAM_DETAIL_db_constraint_xyz"
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"`+secret+`","type":"x"}}`)
		}))
		p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", ProviderOpenAI, srv.Client())
		_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("status %d: error leaks upstream body: %q", status, err.Error())
		}
	}
}
