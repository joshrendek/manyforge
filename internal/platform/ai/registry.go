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

// Registry is a concurrency-safe model catalog. Seeded with known models at
// startup; self-hosters register local models (e.g. an ollama tag) too.
type Registry struct {
	mu     sync.RWMutex
	models map[string]Model
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{models: map[string]Model{}} }

// Register adds or replaces a model by ID.
func (r *Registry) Register(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[m.ID] = m
}

// Lookup returns the model by ID and whether it is known.
func (r *Registry) Lookup(id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[id]
	return m, ok
}
