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

func TestParseAnthropicResponse_Text(t *testing.T) {
	body := []byte(`{
		"content":[{"type":"text","text":"hello there"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":8}
	}`)
	resp, err := parseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "hello there" || resp.FinishReason != FinishStop {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("want no tool calls, got %+v", resp.ToolCalls)
	}
}

func TestParseAnthropicResponse_ToolUse(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"text","text":"looking"},
			{"type":"tool_use","id":"toolu_9","name":"get_ticket","input":{"id":"t-42"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":30,"output_tokens":15}
	}`)
	resp, err := parseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "looking" || resp.FinishReason != FinishToolUse {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_9" || resp.ToolCalls[0].Name != "get_ticket" || string(resp.ToolCalls[0].Args) != `{"id":"t-42"}` {
		t.Fatalf("tool call wrong: %+v", resp.ToolCalls)
	}
}

func TestParseAnthropicResponse_StopReasons(t *testing.T) {
	cases := map[string]FinishReason{
		"end_turn":      FinishStop,
		"stop_sequence": FinishStop,
		"tool_use":      FinishToolUse,
		"max_tokens":    FinishLength,
		"surprise":      FinishOther,
	}
	for raw, want := range cases {
		body := []byte(`{"content":[{"type":"text","text":"x"}],"stop_reason":"` + raw + `","usage":{"input_tokens":1,"output_tokens":1}}`)
		resp, err := parseAnthropicResponse(body)
		if err != nil {
			t.Fatalf("%s: parse: %v", raw, err)
		}
		if resp.FinishReason != want {
			t.Errorf("stop_reason %q -> %q, want %q", raw, resp.FinishReason, want)
		}
	}
}

func TestBuildAnthropicRequest(t *testing.T) {
	req := Request{
		Model:       "claude-sonnet-4-5",
		System:      "you are helpful",
		MaxTokens:   1024,
		Temperature: 0.2,
		Tools: []ToolDef{{
			Name: "get_ticket", Description: "fetch a ticket",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Text: "look up t-42"},
			{Role: RoleAssistant, Text: "ok", ToolCalls: []ToolCall{
				{ID: "toolu_1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t-42"}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "toolu_1", Content: "open", IsError: false},
			}},
		},
	}

	out := buildAnthropicRequest(req, "claude-sonnet-4-5")

	if out.Model != "claude-sonnet-4-5" || out.MaxTokens != 1024 || out.System != "you are helpful" {
		t.Fatalf("scalar fields wrong: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "get_ticket" || string(out.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools wrong: %+v", out.Tools)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	// user text block
	if out.Messages[0].Role != "user" || len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Type != "text" || out.Messages[0].Content[0].Text != "look up t-42" {
		t.Fatalf("user msg wrong: %+v", out.Messages[0])
	}
	// assistant: text block + tool_use block
	if out.Messages[1].Role != "assistant" || len(out.Messages[1].Content) != 2 {
		t.Fatalf("assistant msg wrong: %+v", out.Messages[1])
	}
	if out.Messages[1].Content[1].Type != "tool_use" || out.Messages[1].Content[1].ID != "toolu_1" || out.Messages[1].Content[1].Name != "get_ticket" || string(out.Messages[1].Content[1].Input) != `{"id":"t-42"}` {
		t.Fatalf("tool_use block wrong: %+v", out.Messages[1].Content[1])
	}
	// tool result -> user message with tool_result block
	if out.Messages[2].Role != "user" || out.Messages[2].Content[0].Type != "tool_result" || out.Messages[2].Content[0].ToolUseID != "toolu_1" || out.Messages[2].Content[0].Content != "open" || out.Messages[2].Content[0].IsError {
		t.Fatalf("tool_result block wrong: %+v", out.Messages[2].Content[0])
	}
}

func TestAnthropicComplete_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		fixture  string
		wantText string
		wantTool string // tool name expected, "" for none
		wantFin  FinishReason
	}{
		{"anthropic_text.json", "Hello! How can I help with your ticket?", "", FinishStop},
		{"anthropic_tool_use.json", "Let me look that up.", "get_ticket", FinishToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			golden := loadGolden(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("path = %q, want /v1/messages", r.URL.Path)
				}
				if r.Header.Get("x-api-key") != "sk-test" {
					t.Errorf("missing x-api-key header")
				}
				if r.Header.Get("anthropic-version") == "" {
					t.Errorf("missing anthropic-version header")
				}
				body, _ := io.ReadAll(r.Body)
				var sent anthropicReq
				if err := json.Unmarshal(body, &sent); err != nil || sent.Model == "" || sent.MaxTokens == 0 {
					t.Errorf("unexpected request shape: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(golden)
			}))
			defer srv.Close()

			p := NewAnthropicProvider("sk-test", srv.URL, "claude-sonnet-4-5", srv.Client())
			resp, err := p.Complete(context.Background(), Request{
				Model: "claude-sonnet-4-5", MaxTokens: 256,
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

func TestAnthropicComplete_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"500_server_error", http.StatusInternalServerError, `{"error":{"type":"api_error","message":"boom"}}`, ErrProviderUnavailable},
		{"429_rate_limit", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, ErrProviderUnavailable},
		{"401_auth", http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"bad key"}}`, ErrBadRequest},
		{"400_prompt_too_long", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000"}}`, ErrContextLength},
		{"400_missing_field", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"messages: at least one required"}}`, ErrBadRequest},
		{"400_tool_name_too_long", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"tool name is too long"}}`, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := NewAnthropicProvider("sk-test", srv.URL, "claude-sonnet-4-5", srv.Client())
			_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: status %d body %q -> err %v, want Is(%v)", tc.name, tc.status, tc.body, err, tc.want)
			}
		})
	}
}

func TestAnthropicName(t *testing.T) {
	if NewAnthropicProvider("k", "", "m", http.DefaultClient).Name() != "anthropic" {
		t.Fatal("Name() != anthropic")
	}
}

// Security pin (CLAUDE.md): the raw upstream error body must NEVER appear in the
// error returned to the caller — only a typed sentinel + status. Leaking it would
// turn a partial SSRF / upstream into a read primitive.
func TestAnthropicComplete_DoesNotLeakUpstreamBody(t *testing.T) {
	const secret = "SUPER_SECRET_UPSTREAM_DETAIL_db_constraint_xyz"
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"type":"x","message":"`+secret+`"}}`)
		}))
		p := NewAnthropicProvider("sk-test", srv.URL, "claude-sonnet-4-5", srv.Client())
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
