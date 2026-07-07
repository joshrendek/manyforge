package coding

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestParseAgentChain pins the pure parsing of a fallback chain: valid ids parse in order
// (trimming whitespace), a malformed id is a client validation error, and nil ⇒ empty.
func TestParseAgentChain(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	got, err := parseAgentChain([]string{id1.String(), "  " + id2.String() + "  "})
	if err != nil {
		t.Fatalf("valid ids rejected: %v", err)
	}
	if len(got) != 2 || got[0] != id1 || got[1] != id2 {
		t.Fatalf("parse/order wrong: %v", got)
	}
	if _, err := parseAgentChain([]string{"not-a-uuid"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("malformed id must be ErrValidation, got %v", err)
	}
	if empty, err := parseAgentChain(nil); err != nil || len(empty) != 0 {
		t.Fatalf("nil chain → empty, no error; got %v err=%v", empty, err)
	}
}

// TestDistinctUUIDs pins dedup with first-seen order preserved (so the existence count
// isn't tricked by a repeated id).
func TestDistinctUUIDs(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := distinctUUIDs([]uuid.UUID{a, b, a})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("distinct/order wrong: %v", got)
	}
}

// mapReviewCfgErr must translate DB/sentinel errors into stable service sentinels so handlers can
// branch on typed errors and never leak raw DB internals to clients (manyforge-vay).
func TestMapReviewCfgErr(t *testing.T) {
	if got := mapReviewCfgErr(nil); got != nil {
		t.Fatalf("nil → nil, got %v", got)
	}
	if got := mapReviewCfgErr(pgx.ErrNoRows); !errors.Is(got, errs.ErrNotFound) {
		t.Fatalf("ErrNoRows → ErrNotFound, got %v", got)
	}
	if got := mapReviewCfgErr(&pgconn.PgError{Code: "23505"}); !errors.Is(got, errs.ErrConflict) {
		t.Fatalf("unique violation (23505) → ErrConflict, got %v", got)
	}
	if got := mapReviewCfgErr(fmt.Errorf("bad: %w", errs.ErrValidation)); !errors.Is(got, errs.ErrValidation) {
		t.Fatalf("existing sentinel must pass through, got %v", got)
	}
	// An unknown error must NOT be coerced into a typed sentinel, but must still wrap the cause
	// (logged server-side, generic to clients).
	base := errors.New("boom")
	got := mapReviewCfgErr(base)
	if errors.Is(got, errs.ErrNotFound) || errors.Is(got, errs.ErrConflict) || errors.Is(got, errs.ErrValidation) {
		t.Fatalf("unknown error must not map to a typed sentinel, got %v", got)
	}
	if !errors.Is(got, base) {
		t.Fatalf("unknown error must wrap its cause, got %v", got)
	}
}

// TestValidateDimensionInput pins the service-boundary validation (spec 008 Slice 2): dimension
// + severity must be in their sets; provider (when set) must be known AND carry a model; prompt
// length is bounded. Bad input → errs.ErrValidation.
func TestValidateDimensionInput(t *testing.T) {
	ok := ReviewDimensionInput{Dimension: "security", MinSeverity: "warning"}
	if err := validateDimensionInput(ok); err != nil {
		t.Fatalf("valid minimal input rejected: %v", err)
	}
	okProv := ReviewDimensionInput{Dimension: "ui", MinSeverity: "info", Provider: "openrouter", Model: "x-ai/grok"}
	if err := validateDimensionInput(okProv); err != nil {
		t.Fatalf("valid provider+model rejected: %v", err)
	}
	okFb := ReviewDimensionInput{Dimension: "docs", MinSeverity: "info", Provider: "vllm", Model: "ornith", FallbackChain: []FallbackEntry{{Provider: "openrouter", Model: "deepseek"}}}
	if err := validateDimensionInput(okFb); err != nil {
		t.Fatalf("valid primary+fallback rejected: %v", err)
	}
	okChain := ReviewDimensionInput{Dimension: "docs", MinSeverity: "info", FallbackChain: []FallbackEntry{
		{Provider: "openrouter", Model: "a"}, {Provider: "vllm", Model: "b"}, {Provider: "anthropic", Model: "c"},
	}}
	if err := validateDimensionInput(okChain); err != nil {
		t.Fatalf("valid 3-entry chain rejected: %v", err)
	}

	bad := map[string]ReviewDimensionInput{
		"unknown dimension":             {Dimension: "kitchen-sink", MinSeverity: "info"},
		"bad severity":                  {Dimension: "security", MinSeverity: "critical"},
		"unknown provider":              {Dimension: "security", MinSeverity: "info", Provider: "acme", Model: "m"},
		"provider no model":             {Dimension: "security", MinSeverity: "info", Provider: "openai", Model: "  "},
		"unknown fallback provider":     {Dimension: "security", MinSeverity: "info", FallbackChain: []FallbackEntry{{Provider: "acme", Model: "m"}}},
		"fallback provider no model":    {Dimension: "security", MinSeverity: "info", FallbackChain: []FallbackEntry{{Provider: "openai", Model: "  "}}},
		"fallback entry blank provider": {Dimension: "security", MinSeverity: "info", FallbackChain: []FallbackEntry{{Provider: "", Model: "m"}}},
		"prompt too long":               {Dimension: "security", MinSeverity: "info", Prompt: strings.Repeat("x", maxDimensionPromptBytes+1)},
	}
	for name, in := range bad {
		if err := validateDimensionInput(in); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: want ErrValidation, got %v", name, err)
		}
	}
}
