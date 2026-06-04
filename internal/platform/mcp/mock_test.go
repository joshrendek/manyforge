package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMockClient_ListTools(t *testing.T) {
	tools := []ToolDef{
		{Name: "search", Description: "search the web", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	m := NewMockClient(tools, nil)

	got, err := m.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "search" {
		t.Errorf("got %+v", got)
	}
}

func TestMockClient_CallTool_ScriptedAndRecords(t *testing.T) {
	m := NewMockClient(nil, map[string][]Result{
		"echo": {
			{Content: "first", IsError: false},
			{Content: "second", IsError: false},
		},
		"errtool": {
			{Content: "boom", IsError: true},
		},
	})

	// First echo call.
	r1, err := m.CallTool(context.Background(), "echo", json.RawMessage(`{}`), "k1")
	if err != nil || r1.Content != "first" || r1.IsError {
		t.Fatalf("call1 = (%+v, %v)", r1, err)
	}

	// Second echo call.
	r2, err := m.CallTool(context.Background(), "echo", json.RawMessage(`{}`), "k2")
	if err != nil || r2.Content != "second" {
		t.Fatalf("call2 = (%+v, %v)", r2, err)
	}

	// errtool: IsError true surfaced as Result (not a Go error).
	r3, err := m.CallTool(context.Background(), "errtool", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("errtool: unexpected error: %v", err)
	}
	if !r3.IsError || r3.Content != "boom" {
		t.Fatalf("errtool result = %+v", r3)
	}

	// Exhausted: echo queue is empty.
	_, err = m.CallTool(context.Background(), "echo", json.RawMessage(`{}`), "")
	if !errors.Is(err, ErrMockExhausted) {
		t.Errorf("want ErrMockExhausted, got %v", err)
	}

	// Unknown tool: also exhausted.
	_, err = m.CallTool(context.Background(), "unknown", json.RawMessage(`{}`), "")
	if !errors.Is(err, ErrMockExhausted) {
		t.Errorf("want ErrMockExhausted for unknown tool, got %v", err)
	}

	// Recording: 5 calls total.
	calls := m.Calls()
	if len(calls) != 5 {
		t.Fatalf("want 5 recorded calls, got %d", len(calls))
	}
	if calls[0].Name != "echo" || calls[0].IdemHint != "k1" {
		t.Errorf("call[0] = %+v", calls[0])
	}
	if calls[2].Name != "errtool" {
		t.Errorf("call[2] = %+v", calls[2])
	}
}

func TestMockClient_Initialize_Noop(t *testing.T) {
	m := NewMockClient(nil, nil)
	if err := m.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}
