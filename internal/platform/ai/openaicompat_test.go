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

	out := buildOpenAIRequest(req, "gpt-4o")

	if out.Model != "gpt-4o" || out.MaxTokens != 512 {
		t.Fatalf("scalars wrong: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "get_ticket" || string(out.Tools[0].Function.Parameters) != `{"type":"object"}` {
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

			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", srv.Client())
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
	p := NewOpenAICompatProvider("", srv.URL+"/v1", "llama3", srv.Client())
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
			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", srv.Client())
			_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: status %d -> err %v, want Is(%v)", tc.name, tc.status, err, tc.want)
			}
		})
	}
}

func TestOpenAIName(t *testing.T) {
	if NewOpenAICompatProvider("k", "http://x/v1", "m", http.DefaultClient).Name() != "openai-compat" {
		t.Fatal("Name() != openai-compat")
	}
}
