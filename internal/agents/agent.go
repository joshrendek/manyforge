package agents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// agentDB is the minimal DB surface AgentService needs — satisfied by the real
// *db.DB. An interface so unit tests can omit it (validation runs with DB nil).
type agentDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// AgentService manages business-bound agent definitions over the RLS DB. Each
// Create also mints the agent's kind='agent' principal (its acting identity).
type AgentService struct {
	DB agentDB
}

// Agent is an agent definition as returned to callers.
type Agent struct {
	ID                 uuid.UUID
	BusinessID         uuid.UUID
	PrincipalID        uuid.UUID
	Name               string
	Provider           string
	Model              string
	SystemPrompt       string
	AllowedTools       []string
	AutonomyMode       int
	Enabled            bool
	MonthlyBudgetCents int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateAgentInput is the caller-supplied agent to create.
type CreateAgentInput struct {
	Name               string
	Provider           string
	Model              string
	SystemPrompt       string
	AllowedTools       []string
	AutonomyMode       int
	Enabled            bool
	MonthlyBudgetCents int
}

// UpdateAgentInput is a partial (PATCH) update — nil fields are left unchanged.
// Provider is intentionally absent: it is immutable after creation.
type UpdateAgentInput struct {
	Name               *string
	Model              *string
	SystemPrompt       *string
	AllowedTools       *[]string
	AutonomyMode       *int
	Enabled            *bool
	MonthlyBudgetCents *int
}

func validateCreateAgent(in CreateAgentInput) error {
	if in.Name == "" {
		return fmt.Errorf("agents: name required: %w", errs.ErrValidation)
	}
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.Model == "" {
		return fmt.Errorf("agents: model required: %w", errs.ErrValidation)
	}
	if in.AutonomyMode < 1 || in.AutonomyMode > 3 {
		return fmt.Errorf("agents: autonomy_mode must be 1, 2, or 3: %w", errs.ErrValidation)
	}
	if in.MonthlyBudgetCents < 0 {
		return fmt.Errorf("agents: monthly_budget_cents must be >= 0: %w", errs.ErrValidation)
	}
	return nil
}

func validateUpdateAgent(in UpdateAgentInput) error {
	if in.Name != nil && *in.Name == "" {
		return fmt.Errorf("agents: name cannot be empty: %w", errs.ErrValidation)
	}
	if in.Model != nil && *in.Model == "" {
		return fmt.Errorf("agents: model cannot be empty: %w", errs.ErrValidation)
	}
	if in.AutonomyMode != nil && (*in.AutonomyMode < 1 || *in.AutonomyMode > 3) {
		return fmt.Errorf("agents: autonomy_mode must be 1, 2, or 3: %w", errs.ErrValidation)
	}
	if in.MonthlyBudgetCents != nil && *in.MonthlyBudgetCents < 0 {
		return fmt.Errorf("agents: monthly_budget_cents must be >= 0: %w", errs.ErrValidation)
	}
	return nil
}

// toAgent maps a dbgen row into the domain Agent (narrowing int16/int32 → int).
func toAgent(r dbgen.Agent) Agent {
	tools := r.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	return Agent{
		ID: r.ID, BusinessID: r.BusinessID, PrincipalID: r.PrincipalID,
		Name: r.Name, Provider: string(r.Provider), Model: r.Model,
		SystemPrompt: r.SystemPrompt, AllowedTools: tools,
		AutonomyMode: int(r.AutonomyMode), Enabled: r.Enabled,
		MonthlyBudgetCents: int(r.MonthlyBudgetCents),
		CreatedAt:          r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Create mints the agent's kind='agent' principal and inserts the agent row in one
// RLS transaction. An invisible/foreign business → ErrNoRows (from the principal
// insert's business gate) → ErrNotFound (no oracle). Duplicate (business, name) → 409.
func (s *AgentService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateAgentInput) (Agent, error) {
	if err := validateCreateAgent(in); err != nil {
		return Agent{}, err
	}
	tools := in.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	agentID := uuid.New()
	agentPrincipalID := uuid.New()
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// (1) Create the agent's principal — gated on business visibility.
		if _, perr := q.CreateAgentPrincipal(ctx, dbgen.CreateAgentPrincipalParams{
			ID: agentPrincipalID, BusinessID: businessID,
		}); perr != nil {
			return perr // pgx.ErrNoRows when the business is invisible
		}
		// (2) Insert the agent row referencing that principal.
		r, aerr := q.CreateAgent(ctx, dbgen.CreateAgentParams{
			ID:                 agentID,
			PrincipalID:        agentPrincipalID,
			Name:               in.Name,
			Provider:           dbgen.AiProvider(in.Provider),
			Model:              in.Model,
			SystemPrompt:       in.SystemPrompt,
			AllowedTools:       tools,
			AutonomyMode:       int16(in.AutonomyMode),
			Enabled:            in.Enabled,
			MonthlyBudgetCents: int32(in.MonthlyBudgetCents),
			BusinessID:         businessID,
		})
		row = r
		return aerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// mapAgentErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows → ErrNotFound (no oracle); 23505 (duplicate (business, name)) →
// ErrConflict; typed sentinels pass through; everything else wraps for a 500.
func mapAgentErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("agents: duplicate agent: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
