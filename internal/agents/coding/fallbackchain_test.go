package coding

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// stubProbe reports liveness by base_url — a pure test double for reviewbotProber.
type stubProbe map[string]bool

func (s stubProbe) Live(_ context.Context, c AICredential) bool { return s[c.BaseURL] }

func resolverFor(m map[uuid.UUID]AICredential) resolveFn {
	return func(_ context.Context, id uuid.UUID) (AICredential, error) {
		c, ok := m[id]
		if !ok {
			return AICredential{}, errs.ErrNotFound
		}
		return c, nil
	}
}

func TestChooseReviewbot(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	creds := map[uuid.UUID]AICredential{
		a1: {Provider: "vllm", BaseURL: "http://lan/v1"},
		a2: {Provider: "openrouter", BaseURL: "http://cloud/v1"},
	}
	chain := []uuid.UUID{a1, a2}

	// Primary live ⇒ primary chosen.
	got, err := chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{"http://lan/v1": true, "http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://lan/v1" {
		t.Fatalf("primary-live: got %q err=%v, want lan", got.BaseURL, err)
	}

	// Primary dead ⇒ secondary chosen.
	got, err = chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{"http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("primary-dead: got %q err=%v, want cloud", got.BaseURL, err)
	}

	// All dead but resolvable ⇒ last resolvable (let the real call fail → retry).
	got, err = chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("all-dead: got %q err=%v, want last resolvable (cloud)", got.BaseURL, err)
	}

	// A stale entry is skipped; the next resolvable+live one wins.
	got, err = chooseReviewbot(context.Background(), []uuid.UUID{uuid.New(), a2}, resolverFor(creds), stubProbe{"http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("stale-then-live: got %q err=%v, want cloud", got.BaseURL, err)
	}

	// Nothing resolves ⇒ terminal validation error.
	if _, err := chooseReviewbot(context.Background(), []uuid.UUID{uuid.New()}, resolverFor(creds), stubProbe{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("all-stale: want ErrValidation, got %v", err)
	}
}

// TestResolveReviewChain_DBErrorDegradesToNil pins the intentional degradation: a DB
// failure loading review_config must NOT brick reviews — resolveReviewChain logs and
// returns nil so runJob falls back to the single enqueued agent (no chain, no error).
func TestResolveReviewChain_DBErrorDegradesToNil(t *testing.T) {
	s := &CodeReviewService{DB: fakeServiceDB{}} // WithPrincipal returns errFakeDB without running fn
	if got := s.resolveReviewChain(context.Background(), uuid.New(), uuid.New()); got != nil {
		t.Fatalf("a DB error must degrade to a nil chain, got %v", got)
	}
}
