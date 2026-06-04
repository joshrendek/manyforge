package agents

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Run status values (mirror the agent_run CHECK constraint).
const (
	RunQueued           = "queued"
	RunRunning          = "running"
	RunAwaitingApproval = "awaiting_approval"
	RunSucceeded        = "succeeded"
	RunFailed           = "failed"
)

// AgentRun is the domain view of an agent_run row.
type AgentRun struct {
	ID            uuid.UUID
	AgentID       uuid.UUID
	BusinessID    uuid.UUID
	Trigger       string
	TargetType    *string
	TargetID      *uuid.UUID
	Status        string
	TokensIn      int
	TokensOut     int
	CostCents     int64
	CorrelationID string
	Error         *string
}

type agentRunDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// AgentRunStore persists agent_run rows over the RLS DB.
type AgentRunStore struct{ DB agentRunDB }

func validTrigger(t string) bool { return t == "event" || t == "manual" }

func validStatus(s string) bool {
	switch s {
	case RunQueued, RunRunning, RunAwaitingApproval, RunSucceeded, RunFailed:
		return true
	}
	return false
}

// CreateRun inserts a queued run for an RLS-visible agent. Foreign/unknown agent → ErrNotFound.
func (s *AgentRunStore) CreateRun(ctx context.Context, principalID, businessID, agentID uuid.UUID, trigger, correlationID string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	if !validTrigger(trigger) {
		return AgentRun{}, fmt.Errorf("agents: invalid trigger %q: %w", trigger, errs.ErrValidation)
	}
	var row dbgen.AgentRun
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).CreateAgentRun(ctx, dbgen.CreateAgentRunParams{
			ID: uuid.New(), AgentID: agentID, BusinessID: businessID,
			Trigger: trigger, CorrelationID: correlationID,
			TargetType: targetType, TargetID: db.PGUUIDPtr(targetID),
		})
		row = r
		return e
	})
	if err != nil {
		return AgentRun{}, mapAgentRunErr(err)
	}
	return toAgentRun(row), nil
}

// CreateEventRun idempotently inserts a queued, event-triggered run for the agent under
// the AGENT's own principal (so the insert passes RLS as the acting identity), deduped on
// dedupKey (the triggering ticket_message id). created=false means a prior at-least-once
// delivery already enqueued this (agent, dedupKey) — a benign replay; the caller skips it.
func (s *AgentRunStore) CreateEventRun(ctx context.Context, agentPrincipalID, businessID, agentID uuid.UUID, dedupKey string, targetType *string, targetID *uuid.UUID) (created bool, err error) {
	e := s.DB.WithPrincipal(ctx, agentPrincipalID, func(tx pgx.Tx) error {
		_, ie := dbgen.New(tx).CreateEventAgentRun(ctx, dbgen.CreateEventAgentRunParams{
			ID: uuid.New(), AgentID: agentID, BusinessID: businessID,
			CorrelationID: uuid.NewString(), TriggerDedupKey: dedupKey,
			TargetType: targetType, TargetID: db.PGUUIDPtr(targetID),
		})
		return ie
	})
	if e != nil {
		// ErrNoRows ⇒ ON CONFLICT DO NOTHING (deduped) — under the agent's own principal the
		// agent row is always visible, so the only zero-row cause is the dedup conflict.
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapAgentRunErr(e)
	}
	return true, nil
}

// Progress writes status + token/cost totals + optional error.
func (s *AgentRunStore) Progress(ctx context.Context, principalID, businessID, runID uuid.UUID, status string, tokensIn, tokensOut int, costCents int64, runErr *string) (AgentRun, error) {
	if !validStatus(status) {
		return AgentRun{}, fmt.Errorf("agents: invalid run status %q: %w", status, errs.ErrValidation)
	}
	if tokensIn < 0 || tokensIn > math.MaxInt32 || tokensOut < 0 || tokensOut > math.MaxInt32 {
		return AgentRun{}, fmt.Errorf("agents: token counts out of range [0, 2147483647]: %w", errs.ErrValidation)
	}
	if costCents < 0 {
		return AgentRun{}, fmt.Errorf("agents: cost_cents must be >= 0: %w", errs.ErrValidation)
	}
	var row dbgen.AgentRun
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).UpdateAgentRunProgress(ctx, dbgen.UpdateAgentRunProgressParams{
			ID: runID, BusinessID: businessID, Status: status,
			TokensIn: int32(tokensIn), TokensOut: int32(tokensOut), CostCents: costCents,
			Error: runErr,
		})
		row = r
		return e
	})
	if err != nil {
		return AgentRun{}, mapAgentRunErr(err)
	}
	return toAgentRun(row), nil
}

// Get returns a run by id within the business AND under the given agent. Scoping by
// agent_id (in SQL) closes a same-business IDOR: run R for agent A is NOT fetchable via
// a different agent B's path. A foreign/unknown agentID yields pgx.ErrNoRows ->
// ErrNotFound -> 404 (no existence oracle).
func (s *AgentRunStore) Get(ctx context.Context, principalID, businessID, agentID, runID uuid.UUID) (AgentRun, error) {
	var row dbgen.AgentRun
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).GetAgentRun(ctx, dbgen.GetAgentRunParams{ID: runID, BusinessID: businessID, AgentID: agentID})
		row = r
		return e
	})
	if err != nil {
		return AgentRun{}, mapAgentRunErr(err)
	}
	return toAgentRun(row), nil
}

// MonthToDateCostCents sums this agent's run cost in the current month. The month
// boundary is UTC (mirrors the SQL's date_trunc('month', now())).
func (s *AgentRunStore) MonthToDateCostCents(ctx context.Context, principalID, businessID, agentID uuid.UUID) (int64, error) {
	var cents int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		c, e := dbgen.New(tx).AgentMonthToDateCostCents(ctx, dbgen.AgentMonthToDateCostCentsParams{AgentID: agentID, BusinessID: businessID})
		cents = c
		return e
	})
	if err != nil {
		return 0, mapAgentRunErr(err)
	}
	return cents, nil
}

func mapAgentRunErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: run not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("agents: duplicate run: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: run query: %w", err)
	}
}

func toAgentRun(r dbgen.AgentRun) AgentRun {
	out := AgentRun{
		ID: r.ID, AgentID: r.AgentID, BusinessID: r.BusinessID, Trigger: r.Trigger,
		Status: r.Status, TokensIn: int(r.TokensIn), TokensOut: int(r.TokensOut),
		CostCents: r.CostCents, CorrelationID: r.CorrelationID,
		TargetType: r.TargetType, Error: r.Error,
	}
	if r.TargetID.Valid {
		v := uuid.UUID(r.TargetID.Bytes)
		out.TargetID = &v
	}
	return out
}
