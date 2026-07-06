package agents

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// agentDB is the minimal DB surface AgentService needs — satisfied by the real
// *db.DB. An interface so unit tests can omit it (validation runs with DB nil).
type agentDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// mcpValidator is the subset of MCPServerService AgentService needs to validate
// allowed_mcp_servers ids. Declared as an interface so unit tests can inject a
// fake without a real DB. MCPServerService satisfies this interface.
type mcpValidator interface {
	ValidateServerIDs(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) error
}

// AgentService manages business-bound agent definitions over the RLS DB. Each
// Create also mints the agent's kind='agent' principal (its acting identity).
// MCPServers may be nil (safe for unit tests that don't supply mcp_server ids).
type AgentService struct {
	DB         agentDB
	MCPServers mcpValidator
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
	AllowedMCPServers  []uuid.UUID
	RetriageOnReply    bool
	WebAllowedDomains  []string
	// MaxConcurrentLanes bounds how many code-review dimension lanes this agent runs at
	// once when it is the review's resolved reviewbot (1–16; DB default 4).
	MaxConcurrentLanes int
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
	AllowedMCPServers  []uuid.UUID
	RetriageOnReply    bool
	WebAllowedDomains  []string
	MaxConcurrentLanes int // 0 ⇒ default (4)
}

// UpdateAgentInput is a partial (PATCH) update — nil fields are left unchanged.
// Provider is intentionally absent: it is immutable after creation.
// AllowedMCPServers nil = absent (preserve current value); non-nil = replace
// (empty non-nil slice clears to {}). WebAllowedDomains follows the same
// tri-state convention as AllowedTools/AllowedMCPServers.
type UpdateAgentInput struct {
	Name               *string
	Model              *string
	SystemPrompt       *string
	AllowedTools       *[]string
	AutonomyMode       *int
	Enabled            *bool
	MonthlyBudgetCents *int
	AllowedMCPServers  *[]uuid.UUID
	RetriageOnReply    *bool
	WebAllowedDomains  *[]string
	MaxConcurrentLanes *int // nil = unchanged (PATCH); 1–16 when set
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
	if in.MonthlyBudgetCents < 0 || in.MonthlyBudgetCents > math.MaxInt32 {
		return fmt.Errorf("agents: monthly_budget_cents out of range [0, 2147483647]: %w", errs.ErrValidation)
	}
	// 0 ⇒ "use the DB default (4)"; any explicit value must be in [1,16].
	if in.MaxConcurrentLanes != 0 && (in.MaxConcurrentLanes < 1 || in.MaxConcurrentLanes > 16) {
		return fmt.Errorf("agents: max_concurrent_lanes must be in [1, 16]: %w", errs.ErrValidation)
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
	if in.MonthlyBudgetCents != nil && (*in.MonthlyBudgetCents < 0 || *in.MonthlyBudgetCents > math.MaxInt32) {
		return fmt.Errorf("agents: monthly_budget_cents out of range [0, 2147483647]: %w", errs.ErrValidation)
	}
	if in.MaxConcurrentLanes != nil && (*in.MaxConcurrentLanes < 1 || *in.MaxConcurrentLanes > 16) {
		return fmt.Errorf("agents: max_concurrent_lanes must be in [1, 16]: %w", errs.ErrValidation)
	}
	return nil
}

// toAgent maps a dbgen row into the domain Agent (narrowing int16/int32 → int).
func toAgent(r dbgen.Agent) Agent {
	tools := r.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	mcpServers := r.AllowedMcpServers
	if mcpServers == nil {
		mcpServers = []uuid.UUID{}
	}
	webDomains := r.WebAllowedDomains
	if webDomains == nil {
		webDomains = []string{}
	}
	return Agent{
		ID: r.ID, BusinessID: r.BusinessID, PrincipalID: r.PrincipalID,
		Name: r.Name, Provider: string(r.Provider), Model: r.Model,
		SystemPrompt: r.SystemPrompt, AllowedTools: tools,
		AutonomyMode: int(r.AutonomyMode), Enabled: r.Enabled,
		MonthlyBudgetCents: int(r.MonthlyBudgetCents),
		AllowedMCPServers:  mcpServers,
		RetriageOnReply:    r.RetriageOnReply,
		WebAllowedDomains:  webDomains,
		MaxConcurrentLanes: int(r.MaxConcurrentLanes),
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
	// 0 means "unset" — default to the same 4 the DB column defaults to (a NOT NULL
	// column with a CHECK BETWEEN 1 AND 16 would reject a zero insert).
	if in.MaxConcurrentLanes == 0 {
		in.MaxConcurrentLanes = 4
	}
	// Validate allowed_mcp_servers before touching the DB. Nil-safe: only call
	// when the validator is wired AND the slice is non-empty.
	if s.MCPServers != nil && len(in.AllowedMCPServers) > 0 {
		if err := s.MCPServers.ValidateServerIDs(ctx, principalID, businessID, in.AllowedMCPServers); err != nil {
			return Agent{}, err
		}
	}
	tools := in.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	mcpServers := in.AllowedMCPServers
	if mcpServers == nil {
		mcpServers = []uuid.UUID{}
	}
	webDomains := in.WebAllowedDomains
	if webDomains == nil {
		webDomains = []string{}
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
			AllowedMcpServers:  mcpServers,
			RetriageOnReply:    in.RetriageOnReply,
			WebAllowedDomains:  webDomains,
			MaxConcurrentLanes: int32(in.MaxConcurrentLanes),
			BusinessID:         businessID,
		})
		if aerr != nil {
			return aerr
		}
		row = r
		// US3: bind the agent's acting membership (home business + agent_runtime preset
		// role) so authorized_businesses() is non-empty and the agent can pass RLS/RBAC to
		// act. membership_rls is WITH CHECK(true) so the creator's context may insert;
		// membership_agent_guard validates home-business/single-membership/no-admin-perms.
		roleID, rErr := q.PresetRoleID(ctx, "agent_runtime")
		if rErr != nil {
			return rErr
		}
		if mErr := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
			ID:           uuid.New(),
			PrincipalID:  agentPrincipalID,
			BusinessID:   businessID,
			TenantRootID: row.TenantRootID,
			RoleID:       roleID,
			GrantedBy:    db.PGUUID(principalID),
		}); mErr != nil {
			return mErr
		}
		return nil
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// Get loads one agent by (id, business_id). RLS + the explicit business_id predicate
// make a foreign/unknown id indistinguishable (no oracle). pgx.ErrNoRows → 404.
func (s *AgentService) Get(ctx context.Context, principalID, businessID, agentID uuid.UUID) (Agent, error) {
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetAgent(ctx, dbgen.GetAgentParams{ID: agentID, BusinessID: businessID})
		row = r
		return qerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// List returns all agents for a business, ordered by name.
func (s *AgentService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]Agent, error) {
	var rows []dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListAgents(ctx, businessID)
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapAgentErr(err)
	}
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAgent(r))
	}
	return out, nil
}

// Update applies a partial change. Omitted (nil) fields are preserved via COALESCE
// in SQL. No matching (id, business_id) → ErrNoRows → 404 (no oracle).
func (s *AgentService) Update(ctx context.Context, principalID, businessID, agentID uuid.UUID, in UpdateAgentInput) (Agent, error) {
	if err := validateUpdateAgent(in); err != nil {
		return Agent{}, err
	}
	// Validate allowed_mcp_servers before touching the DB. Only validate when
	// the slice is non-nil (nil = absent = PATCH preserve) AND non-empty.
	// Nil-safe: only call when the validator is wired.
	if s.MCPServers != nil && in.AllowedMCPServers != nil && len(*in.AllowedMCPServers) > 0 {
		if err := s.MCPServers.ValidateServerIDs(ctx, principalID, businessID, *in.AllowedMCPServers); err != nil {
			return Agent{}, err
		}
	}
	params := dbgen.UpdateAgentParams{ID: agentID, BusinessID: businessID}
	params.Name = in.Name
	params.Model = in.Model
	params.SystemPrompt = in.SystemPrompt
	if in.AllowedTools != nil {
		params.AllowedTools = *in.AllowedTools
	}
	if in.AutonomyMode != nil {
		m := int16(*in.AutonomyMode)
		params.AutonomyMode = &m
	}
	params.Enabled = in.Enabled
	if in.MonthlyBudgetCents != nil {
		c := int32(*in.MonthlyBudgetCents)
		params.MonthlyBudgetCents = &c
	}
	// AllowedMCPServers: nil pointer = absent (COALESCE in SQL preserves current
	// value); non-nil pointer = replace (empty non-nil slice clears to {}).
	if in.AllowedMCPServers != nil {
		params.AllowedMcpServers = *in.AllowedMCPServers
	}
	// WebAllowedDomains: nil pointer = absent (COALESCE preserves current value);
	// non-nil pointer = replace (empty non-nil slice clears to {}).
	if in.WebAllowedDomains != nil {
		params.WebAllowedDomains = *in.WebAllowedDomains
	}
	params.RetriageOnReply = in.RetriageOnReply
	if in.MaxConcurrentLanes != nil {
		n := int32(*in.MaxConcurrentLanes)
		params.MaxConcurrentLanes = &n
	}
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).UpdateAgent(ctx, params)
		row = r
		return qerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// Delete removes an agent and its agent principal atomically. rows-affected 0 (the
// agent didn't exist / wasn't visible) → ErrNotFound (no oracle).
func (s *AgentService) Delete(ctx context.Context, principalID, businessID, agentID uuid.UUID) error {
	var affected int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, qerr := dbgen.New(tx).DeleteAgent(ctx, dbgen.DeleteAgentParams{ID: agentID, BusinessID: businessID})
		affected = n
		return qerr
	})
	if err != nil {
		return mapAgentErr(err)
	}
	if affected == 0 {
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	}
	return nil
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
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return fmt.Errorf("agents: agent has acted and cannot be deleted; disable it instead: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
