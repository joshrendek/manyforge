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
	"github.com/jackc/pgx/v5/pgtype"
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
	CreatedAt     time.Time
}

type agentRunDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// AgentRunStore persists agent_run rows over the RLS DB.
type AgentRunStore struct{ DB agentRunDB }

func validTrigger(t string) bool { return t == "event" || t == "manual" }

// targetTypeTicket is the only run target the system understands today. agent_run.target_type
// is free-text at the DB layer (unlike trigger, which has a CHECK); validTargetType is the
// service-boundary allowlist that keeps it consistent without a migration. Blast radius is
// nil today (parameterized SQL + string compare), so this is correctness/clarity, not a fix.
const targetTypeTicket = "ticket"

func validTargetType(t string) bool { return t == targetTypeTicket }

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
	if targetType != nil && !validTargetType(*targetType) {
		return AgentRun{}, fmt.Errorf("agents: invalid target_type %q: %w", *targetType, errs.ErrValidation)
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
	if targetType != nil && !validTargetType(*targetType) {
		return false, fmt.Errorf("agents: invalid target_type %q: %w", *targetType, errs.ErrValidation)
	}
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

// AgentRef identifies an agent and its acting principal (the SECURITY DEFINER lister
// returns these for a business; the trigger creates one queued run per ref).
type AgentRef struct {
	AgentID     uuid.UUID
	PrincipalID uuid.UUID
}

// ClaimedRun is one run atomically claimed (queued→running) for execution, carrying the
// full agent config so the drainer needs no second (RLS) lookup.
type ClaimedRun struct {
	RunID         uuid.UUID
	CorrelationID string
	TargetType    *string
	TargetID      *uuid.UUID
	Agent         Agent
}

// EnabledAgentsForBusiness lists the enabled agents for a business via the system-wide
// SECURITY DEFINER fn. The caller (a ticket.created subscriber) runs principal-less, so
// it cannot use an RLS-scoped query; the fn is scoped by business_id AND tenant_root_id.
func (s *AgentRunStore) EnabledAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	var refs []AgentRef
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			"SELECT agent_id, principal_id FROM enabled_agents_for_business($1, $2)",
			businessID, tenantRootID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r AgentRef
			if se := rows.Scan(&r.AgentID, &r.PrincipalID); se != nil {
				return se
			}
			refs = append(refs, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list enabled agents: %w", err)
	}
	return refs, nil
}

// EnabledRetriageAgentsForBusiness lists the enabled agents for a business that have opted
// in to reply re-triage (retriage_on_reply = true), via the system-wide SECURITY DEFINER fn.
// Principal-less (the message.received subscriber has no principal GUC); the fn is scoped by
// business_id AND tenant_root_id so a cross-tenant event can never surface another tenant's
// agents. Mirrors EnabledAgentsForBusiness.
func (s *AgentRunStore) EnabledRetriageAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	var refs []AgentRef
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			"SELECT agent_id, principal_id FROM enabled_retriage_agents_for_business($1, $2)",
			businessID, tenantRootID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r AgentRef
			if se := rows.Scan(&r.AgentID, &r.PrincipalID); se != nil {
				return se
			}
			refs = append(refs, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list retriage agents: %w", err)
	}
	return refs, nil
}

// EnqueueReplyRetriageRun runs the atomic guard+cap+dedup-insert DEFINER for one
// (message, agent) pair and returns its text outcome (one of: enqueued, skipped_not_inbound,
// skipped_auto_reply, skipped_capped, skipped_dedup). Principal-less (mirrors
// ClaimNextQueuedRun): the DEFINER derives business/tenant from the message row.
func (s *AgentRunStore) EnqueueReplyRetriageRun(ctx context.Context, messageID, agentID uuid.UUID, cap int) (string, error) {
	var outcome string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT enqueue_reply_retriage_run($1, $2, $3)",
			messageID, agentID, cap).Scan(&outcome)
	})
	if err != nil {
		return "", fmt.Errorf("agents: enqueue reply retriage run: %w", err)
	}
	return outcome, nil
}

// ClaimNextQueuedRun atomically claims the oldest queued run (queued→running) across all
// tenants via the SECURITY DEFINER fn (SKIP LOCKED ⇒ concurrent drainers never
// double-claim). Returns (nil, nil) when nothing is queued.
func (s *AgentRunStore) ClaimNextQueuedRun(ctx context.Context) (*ClaimedRun, error) {
	var (
		c        ClaimedRun
		ag       Agent
		tt       *string
		tid      pgtype.UUID
		provider string
		mode     int16
		budget   int32
		found    bool
	)
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT run_id, business_id, tenant_root_id, correlation_id,
			target_type, target_id, agent_id, agent_principal_id, provider, model,
			system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents
			FROM claim_next_queued_agent_run()`)
		var tenantRootID uuid.UUID
		e := row.Scan(&c.RunID, &ag.BusinessID, &tenantRootID, &c.CorrelationID,
			&tt, &tid, &ag.ID, &ag.PrincipalID, &provider, &ag.Model,
			&ag.SystemPrompt, &ag.AllowedTools, &mode, &ag.Enabled, &budget)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil // nothing queued
		}
		if e != nil {
			return e
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: claim queued run: %w", err)
	}
	if !found {
		return nil, nil
	}
	ag.Provider = provider
	ag.AutonomyMode = int(mode)
	ag.MonthlyBudgetCents = int(budget)
	if ag.AllowedTools == nil {
		ag.AllowedTools = []string{}
	}
	c.Agent = ag
	c.TargetType = tt
	if tid.Valid {
		v := uuid.UUID(tid.Bytes)
		c.TargetID = &v
	}
	return &c, nil
}

// RunListFilter narrows a run list. Status "" = all statuses.
type RunListFilter struct {
	Status string
	Window Window
}

const (
	runListDefaultLimit = 50
	runListMaxLimit     = 100
)

// clampRunLimit applies the service-boundary page cap on the runs list. A non-positive
// request gets the default; an oversized request is silently capped at the max (never an
// unbounded scan) — the DoS guard, enforced here so every caller inherits it.
func clampRunLimit(n int) int {
	if n <= 0 {
		return runListDefaultLimit
	}
	if n > runListMaxLimit {
		return runListMaxLimit
	}
	return n
}

// ListRuns returns a keyset page of an agent's runs (newest first) plus the next
// cursor (nil when exhausted). cursor "" starts at the newest run.
func (s *AgentRunStore) ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error) {
	lim := clampRunLimit(limit)

	// Page 1 starts at the sentinel tuple (greater than any real row under
	// (created_at, id) DESC); a non-empty cursor resumes from its keyset.
	curTs, curID := runCursorPage1.ts, runCursorPage1.id
	if cursor != "" {
		k, err := decodeRunCursor(cursor)
		if err != nil {
			return nil, nil, err
		}
		curTs, curID = k.ts, k.id
	}

	var status *string
	if f.Status != "" {
		status = &f.Status
	}

	var rows []dbgen.AgentRun
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListAgentRuns(ctx, dbgen.ListAgentRunsParams{
			BusinessID:   businessID,
			AgentID:      agentID,
			FromTs:       f.Window.From,
			ToTs:         f.Window.To,
			Status:       status,
			CurCreatedAt: curTs,
			CurID:        curID,
			Lim:          int32(lim + 1),
		})
		rows = r
		return e
	})
	if err != nil {
		return nil, nil, mapAgentRunErr(err)
	}

	var next *string
	if len(rows) > lim {
		last := rows[lim-1]
		tok := encodeRunCursor(runKeyset{ts: last.CreatedAt, id: last.ID})
		next = &tok
		rows = rows[:lim]
	}
	out := make([]AgentRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAgentRun(r))
	}
	return out, next, nil
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
	out.CreatedAt = r.CreatedAt
	return out
}
