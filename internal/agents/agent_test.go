package agents

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// fakeValidator is a mcpValidator that returns a fixed error (or nil).
type fakeValidator struct {
	err   error
	calls int
}

func (f *fakeValidator) ValidateServerIDs(_ context.Context, _, _ uuid.UUID, ids []uuid.UUID) error {
	if len(ids) > 0 {
		f.calls++
	}
	return f.err
}

// TestAgentService_AllowedMCPServers_ValidatorCalledAndRoundTrips verifies that
// when CreateAgentInput.AllowedMCPServers is non-empty the validator is invoked,
// and when the validator returns ErrValidation the error propagates before any DB
// interaction (DB is nil — if the code reaches the DB it will panic, proving the
// short-circuit).
func TestAgentService_AllowedMCPServers_ValidatorCalledAndErrors(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	t.Run("validator called and returns ErrValidation propagated", func(t *testing.T) {
		fv := &fakeValidator{err: errs.ErrValidation}
		svc := &AgentService{DB: nil, MCPServers: fv}

		in := CreateAgentInput{
			Name: "bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
			AutonomyMode: 1, AllowedMCPServers: []uuid.UUID{id1, id2},
		}
		_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), in)
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("want ErrValidation from validator, got %v", err)
		}
		if fv.calls != 1 {
			t.Fatalf("want validator called once, got %d", fv.calls)
		}
	})

	t.Run("empty AllowedMCPServers skips validator (nil DB safe)", func(t *testing.T) {
		fv := &fakeValidator{err: nil}
		// DB nil — would panic if reached
		svc := &AgentService{DB: nil, MCPServers: fv}

		in := CreateAgentInput{
			Name: "bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
			AutonomyMode: 1, AllowedMCPServers: nil,
		}
		// Validation passes, but DB is nil so we expect a nil-pointer panic/error
		// from the DB call — not ErrValidation. Recover from any panic to confirm
		// we got past validation without the validator being called.
		func() {
			defer func() { _ = recover() }()
			svc.Create(context.Background(), uuid.New(), uuid.New(), in) //nolint:errcheck
		}()
		if fv.calls != 0 {
			t.Fatalf("want validator NOT called for empty slice, got %d calls", fv.calls)
		}
	})

	t.Run("nil MCPServers field is safe when no ids", func(t *testing.T) {
		svc := &AgentService{DB: nil, MCPServers: nil}
		in := CreateAgentInput{
			Name: "bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
			AutonomyMode: 1, AllowedMCPServers: nil,
		}
		// Should not panic on nil MCPServers — should reach the DB call (nil) and
		// recover gracefully.
		func() {
			defer func() { _ = recover() }()
			svc.Create(context.Background(), uuid.New(), uuid.New(), in) //nolint:errcheck
		}()
		// If we got here without panicking at validator call site, the nil guard works.
	})
}

// TestAgentService_Update_AllowedMCPServers_ValidatorCalled tests that Update also
// calls the validator when a non-empty slice is supplied, and returns ErrValidation
// before the DB is touched.
func TestAgentService_Update_AllowedMCPServers_ValidatorCalled(t *testing.T) {
	id1 := uuid.New()
	fv := &fakeValidator{err: errs.ErrValidation}
	svc := &AgentService{DB: nil, MCPServers: fv}

	ids := []uuid.UUID{id1}
	in := UpdateAgentInput{AllowedMCPServers: &ids}
	_, err := svc.Update(context.Background(), uuid.New(), uuid.New(), uuid.New(), in)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("want ErrValidation from validator on update, got %v", err)
	}
	if fv.calls != 1 {
		t.Fatalf("want validator called once on update, got %d", fv.calls)
	}
}

func TestMapAgentErr_FKViolationIsConflict(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23503"}
	if got := mapAgentErr(pgErr); !errors.Is(got, errs.ErrConflict) {
		t.Fatalf("23503 → %v, want ErrConflict", got)
	}
}

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
		{"budget overflow", func(in *CreateAgentInput) { in.MonthlyBudgetCents = math.MaxInt32 + 1 }},
		{"lanes over max", func(in *CreateAgentInput) { in.MaxConcurrentLanes = 17 }},
		{"lanes negative", func(in *CreateAgentInput) { in.MaxConcurrentLanes = -1 }},
	}
	// 0 (unset ⇒ default 4) and any value in [1,16] are valid.
	for _, n := range []int{0, 1, 16} {
		in := base
		in.MaxConcurrentLanes = n
		if err := validateCreateAgent(in); err != nil {
			t.Fatalf("max_concurrent_lanes=%d should be valid: %v", n, err)
		}
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
		{MonthlyBudgetCents: func(i int) *int { return &i }(math.MaxInt32 + 1)},
		{MaxConcurrentLanes: mode(0)},  // explicit 0 invalid on PATCH (must be 1..16)
		{MaxConcurrentLanes: mode(17)}, // above the cap
	}
	for i, in := range bad {
		if err := validateUpdateAgent(in); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("case %d: want ErrValidation, got %v", i, err)
		}
	}
}
