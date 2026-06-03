package ai

import (
	"context"
	"errors"
	"testing"
)

func TestMockProviderScriptsAndRecords(t *testing.T) {
	m := NewMockProvider(
		Response{ToolCalls: []ToolCall{{ID: "c1", Name: "get_ticket"}}, FinishReason: FinishToolUse},
		Response{Text: "done", FinishReason: FinishStop, Usage: Usage{InputTokens: 5, OutputTokens: 3}},
	)

	r1, err := m.Complete(context.Background(), Request{Model: "x", Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil || r1.FinishReason != FinishToolUse {
		t.Fatalf("call 1 = (%+v, %v)", r1, err)
	}
	r2, _ := m.Complete(context.Background(), Request{Model: "x"})
	if r2.Text != "done" {
		t.Fatalf("call 2 text = %q", r2.Text)
	}
	// Exhausted: a third call errors so a runaway loop in a test fails loudly.
	if _, err := m.Complete(context.Background(), Request{}); !errors.Is(err, ErrMockExhausted) {
		t.Fatalf("want ErrMockExhausted, got %v", err)
	}
	// It recorded every request it received.
	reqs := m.Requests()
	if len(reqs) != 3 || reqs[0].Messages[0].Text != "hi" {
		t.Fatalf("recorded = %+v", reqs)
	}
}
