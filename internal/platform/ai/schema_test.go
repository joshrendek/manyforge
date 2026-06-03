package ai

import (
	"encoding/json"
	"testing"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	in := Message{Role: RoleAssistant, Text: "hi", ToolCalls: []ToolCall{
		{ID: "c1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t1"}`)},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Role != RoleAssistant || out.Text != "hi" || len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "get_ticket" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if string(out.ToolCalls[0].Args) != `{"id":"t1"}` {
		t.Fatalf("Args round-trip = %s", out.ToolCalls[0].Args)
	}
}

func TestToolResultMessageJSONRoundTrip(t *testing.T) {
	in := Message{Role: RoleTool, ToolResults: []ToolResult{
		{CallID: "c1", Content: "ticket #42", IsError: true},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Role != RoleTool || len(out.ToolResults) != 1 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	tr := out.ToolResults[0]
	if tr.CallID != "c1" || tr.Content != "ticket #42" || !tr.IsError {
		t.Fatalf("ToolResult round-trip = %+v", tr)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 7}
	if u.Total() != 17 {
		t.Errorf("Total() = %d, want 17", u.Total())
	}
}
