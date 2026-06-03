package ai

import (
	"context"
	"errors"
	"sync"
)

// ErrMockExhausted is returned when a MockProvider runs out of scripted
// responses — surfacing an unexpected extra model call in a test loudly.
var ErrMockExhausted = errors.New("ai: mock provider exhausted")

// MockProvider returns pre-scripted Responses in order and records every Request
// it was handed. It is the deterministic backbone for run-loop / gate / triage
// tests — no network, no fixtures (recorded golden fixtures arrive in US1b).
type MockProvider struct {
	mu    sync.Mutex
	queue []Response
	calls []Request
}

// NewMockProvider scripts the responses returned by successive Complete calls.
func NewMockProvider(responses ...Response) *MockProvider {
	return &MockProvider{queue: append([]Response(nil), responses...)}
}

// Complete returns the next scripted response, recording the request.
func (m *MockProvider) Complete(_ context.Context, req Request) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if len(m.queue) == 0 {
		return Response{}, ErrMockExhausted
	}
	resp := m.queue[0]
	m.queue = m.queue[1:]
	return resp, nil
}

// Name identifies the provider.
func (m *MockProvider) Name() string { return "mock" }

// Requests returns a copy of every Request handed to Complete, in order.
func (m *MockProvider) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.calls...)
}
