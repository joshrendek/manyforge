package coding

import (
	"encoding/json"
	"sync"
	"unicode/utf8"
)

// previewMaxBytes caps the streamed-output preview persisted in code_review.progress
// (the tail of the model's output). Keeps the jsonb small so the ~5s heartbeat write
// stays cheap, while being enough to "watch it write".
const previewMaxBytes = 4 << 10

// Progress is a goroutine-safe holder for a code review's live progress. runJob and
// the streaming localReview mutate it in-memory; the worker heartbeat reads
// Snapshot() every ~5s and persists it via renew_code_review_lease. All methods are
// nil-safe so direct (non-worker) callers can pass a nil *Progress.
type Progress struct {
	mu         sync.Mutex
	phase      string
	tokens     int
	rawPartial string
	secrets    []string
}

// SetPhase records the current pipeline phase ("preparing"/"reviewing"/"posting").
func (p *Progress) SetPhase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.phase = phase
	p.mu.Unlock()
}

// SetSecrets records secret values to scrub from the preview before persistence.
func (p *Progress) SetSecrets(secrets ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.secrets = append(p.secrets, secrets...)
	p.mu.Unlock()
}

// UpdateStream records the latest completion-token count and the accumulated raw
// model output so far (the full buffer; Snapshot tail-caps + redacts it).
func (p *Progress) UpdateStream(tokens int, partial string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.tokens = tokens
	p.rawPartial = partial
	p.mu.Unlock()
}

// progressSnapshot is the JSON shape persisted to code_review.progress and returned
// to the API (mirrored by the TS CodeReview.progress type).
type progressSnapshot struct {
	Phase   string `json:"phase"`
	Tokens  int    `json:"tokens"`
	Preview string `json:"preview"`
}

// Snapshot returns the JSON to persist, or nil if no phase has been set yet (so a
// pre-heartbeat renew leaves progress NULL). Redaction runs once per snapshot
// (every ~5s), not per token.
func (p *Progress) Snapshot() []byte {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == "" {
		return nil
	}
	preview := redactSecrets(tailBytes(p.rawPartial, previewMaxBytes), p.secrets...)
	b, err := json.Marshal(progressSnapshot{Phase: p.phase, Tokens: p.tokens, Preview: preview})
	if err != nil {
		return nil
	}
	return b
}

// tailBytes returns the last max bytes of s, trimmed forward to a valid UTF-8 rune
// boundary so the resulting JSON string stays valid.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	t := s[len(s)-max:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return t
}
