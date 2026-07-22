package ai

import "sync"

// Model is registry metadata for one model: pricing (cents per MILLION tokens,
// integer to avoid float drift), context window, and whether it supports tools.
type Model struct {
	ID                 string
	Provider           string
	ContextWindow      int
	InputCentsPerMTok  int64
	OutputCentsPerMTok int64
	SupportsTools      bool
}

// CostCents returns the integer-cent cost of a usage under this model's pricing.
// Math is in (tokens * centsPerMTok) / 1_000_000 with rounding so a sub-million
// call is never free.
func (m Model) CostCents(u Usage) int64 {
	in := ceilDiv(int64(u.InputTokens)*m.InputCentsPerMTok, 1_000_000)
	out := ceilDiv(int64(u.OutputTokens)*m.OutputCentsPerMTok, 1_000_000)
	return in + out
}

func ceilDiv(a, b int64) int64 {
	// Negative token counts clamp to 0 (defensive — tokens are never negative here).
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// modelKey identifies a model by (provider, id). Pricing is keyed provider-first so a
// $0 openai_codex 'gpt-5' cannot shadow a metered same-named model of another provider —
// mirrors the model_pricing composite PK (provider, model_id). manyforge-6fx.2.
type modelKey struct{ provider, id string }

// Registry is a concurrency-safe model catalog. Seeded with known models at
// startup; self-hosters register local models (e.g. an ollama tag) too.
type Registry struct {
	mu     sync.RWMutex
	models map[modelKey]Model
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{models: map[modelKey]Model{}} }

// Register adds or replaces a model, keyed by its (Provider, ID).
func (r *Registry) Register(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[modelKey{m.Provider, m.ID}] = m
}

// Lookup returns the model for a (provider, id) and whether it is known. The provider
// is required: pricing is provider-scoped, so the same id under a different provider is
// a different model (or a miss).
func (r *Registry) Lookup(provider, id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[modelKey{provider, id}]
	return m, ok
}

// Len returns the number of registered models.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.models)
}
