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
}

func TestUsageTotal(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 7}
	if u.Total() != 17 {
		t.Errorf("Total() = %d, want 17", u.Total())
	}
}
