package agents

import (
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestValidateCreateAgent(t *testing.T) {
	base := CreateAgentInput{Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1, MonthlyBudgetCents: 0}
	if err := validateCreateAgent(base); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*CreateAgentInput)
	}{
		{"empty name", func(in *CreateAgentInput) { in.Name = "" }},
		{"unknown provider", func(in *CreateAgentInput) { in.Provider = "bedrock" }},
		{"empty model", func(in *CreateAgentInput) { in.Model = "" }},
		{"mode 0", func(in *CreateAgentInput) { in.AutonomyMode = 0 }},
		{"mode 4", func(in *CreateAgentInput) { in.AutonomyMode = 4 }},
		{"negative budget", func(in *CreateAgentInput) { in.MonthlyBudgetCents = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			if err := validateCreateAgent(in); !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}

func TestValidateUpdateAgent(t *testing.T) {
	ptr := func(s string) *string { return &s }
	mode := func(i int) *int { return &i }
	if err := validateUpdateAgent(UpdateAgentInput{}); err != nil {
		t.Fatalf("empty patch should be valid (no-op): %v", err)
	}
	bad := []UpdateAgentInput{
		{Name: ptr("")},
		{Model: ptr("")},
		{AutonomyMode: mode(0)},
		{AutonomyMode: mode(9)},
		{MonthlyBudgetCents: func(i int) *int { return &i }(-5)},
	}
	for i, in := range bad {
		if err := validateUpdateAgent(in); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("case %d: want ErrValidation, got %v", i, err)
		}
	}
}
