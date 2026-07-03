package coding

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

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

	bad := map[string]ReviewDimensionInput{
		"unknown dimension":   {Dimension: "kitchen-sink", MinSeverity: "info"},
		"bad severity":        {Dimension: "security", MinSeverity: "critical"},
		"unknown provider":    {Dimension: "security", MinSeverity: "info", Provider: "acme", Model: "m"},
		"provider no model":   {Dimension: "security", MinSeverity: "info", Provider: "openai", Model: "  "},
		"prompt too long":     {Dimension: "security", MinSeverity: "info", Prompt: strings.Repeat("x", maxDimensionPromptBytes+1)},
	}
	for name, in := range bad {
		if err := validateDimensionInput(in); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: want ErrValidation, got %v", name, err)
		}
	}
}
