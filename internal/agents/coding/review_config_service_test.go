package coding

import (
	"errors"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

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
