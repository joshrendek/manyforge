package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// ErrMockExhausted is returned by MockClient when its scripted CallTool
// responses run out — surfacing unexpected extra tool calls in tests loudly.
var ErrMockExhausted = errors.New("mcp: mock client exhausted")

// Compile-time assertion: MockClient must satisfy ClientLike.
var _ ClientLike = (*MockClient)(nil)

// CallRecord holds the arguments passed to a single CallTool invocation.
type CallRecord struct {
	Name     string
	Args     json.RawMessage
	IdemHint string
}

// MockClient is a deterministic MCP client for tests. It returns scripted
// ListTools results and a map (or queue) of CallTool responses, recording
// every invocation behind a mutex. It mirrors ai/mock.go's conventions.
type MockClient struct {
	mu sync.Mutex

	// listResult is returned verbatim by every ListTools call.
	listResult []ToolDef

	// callResults maps tool name → queue of scripted Results. If a name is not
	// present, callDefault is used (if set), otherwise ErrMockExhausted.
	callResults map[string][]Result

	// calls records every CallTool invocation in order.
	calls []CallRecord
}

// NewMockClient builds a MockClient. listResult is returned by ListTools.
// callResults maps tool name → ordered Results; a name absent from the map
// returns ErrMockExhausted.
func NewMockClient(listResult []ToolDef, callResults map[string][]Result) *MockClient {
	// Deep-copy the queue map so callers can't mutate it after construction.
	m := make(map[string][]Result, len(callResults))
	for k, v := range callResults {
		m[k] = append([]Result(nil), v...)
	}
	return &MockClient{
		listResult:  append([]ToolDef(nil), listResult...),
		callResults: m,
	}
}

// Initialize is a no-op on the mock (always succeeds).
func (m *MockClient) Initialize(_ context.Context) error { return nil }

// ListTools returns the scripted tool list.
func (m *MockClient) ListTools(_ context.Context) ([]ToolDef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ToolDef(nil), m.listResult...), nil
}

// CallTool returns the next scripted Result for the named tool, recording the
// call. Returns ErrMockExhausted when the queue for that tool is empty.
func (m *MockClient) CallTool(_ context.Context, name string, args json.RawMessage, idemHint string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, CallRecord{Name: name, Args: args, IdemHint: idemHint})
	queue, ok := m.callResults[name]
	if !ok || len(queue) == 0 {
		return Result{}, ErrMockExhausted
	}
	r := queue[0]
	m.callResults[name] = queue[1:]
	return r, nil
}

// Calls returns a shallow copy of every CallTool invocation in order.
func (m *MockClient) Calls() []CallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CallRecord(nil), m.calls...)
}
