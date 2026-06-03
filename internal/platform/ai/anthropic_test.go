package ai

import (
	"encoding/json"
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
