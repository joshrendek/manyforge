package ai

import (
	"context"
	"errors"
)

// Provider is one LLM backend. Implementations: anthropic + openai-compat
// transports (US1b) and MockProvider (this plan). Complete is a SINGLE,
// non-streaming round-trip; the agentic loop lives above this in internal/agents.
type Provider interface {
	// Complete sends one Request and returns the model's Response. It must map
	// transport/HTTP failures to the sentinels below so callers branch uniformly.
	Complete(ctx context.Context, req Request) (Response, error)
	// Name identifies the provider for logs/metrics (e.g. "anthropic").
	Name() string
}

// Gateway error sentinels — wrap with fmt.Errorf("...: %w", Err...). Callers use
// errors.Is. Never surface a raw upstream body to an end user (Principle II).
var (
	ErrProviderUnavailable = errors.New("ai: provider unavailable") // network/5xx/timeout — retryable
	ErrBadRequest          = errors.New("ai: bad request")          // 4xx from provider — not retryable
	ErrContextLength       = errors.New("ai: context length exceeded")
)
