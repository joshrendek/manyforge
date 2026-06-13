package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// Approval-item states (mirror the approval_item CHECK constraint).
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExecuted = "executed"
	ApprovalExpired  = "expired"
	// ApprovalFailed is terminal: the executor sets it (recording the reason in error) when an
	// approved action can never execute — an unknown tool / no MCP host, or a transient failure
	// that exhausted the outbox retry budget (manyforge-sa8).
	ApprovalFailed = "failed"
)

// defaultApprovalTTL is how long a pending item stays actionable before the sweep
// expires it (design §8, resolved in US4).
const defaultApprovalTTL = 7 * 24 * time.Hour

// ApprovalItem is the domain view of an approval_item row.
type ApprovalItem struct {
	ID                   uuid.UUID
	AgentRunID           uuid.UUID
	BusinessID           uuid.UUID
	TenantRootID         uuid.UUID
	Tool                 string
	Args                 json.RawMessage
	EffectClass          int
	State                string
	DecidedByPrincipalID *uuid.UUID
	ExecutedAt           *time.Time
	ExpiresAt            time.Time
	Error                *string
}

type approvalDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// ApprovalStore persists approval_item rows over the RLS DB.
type ApprovalStore struct {
	DB  approvalDB
	TTL time.Duration // 0 ⇒ defaultApprovalTTL
}

// var _ asserts the store implements the engine's approvalWriter contract (runner.go).
var _ approvalWriter = (*ApprovalStore)(nil)

func (s *ApprovalStore) ttl() time.Duration {
	if s.TTL <= 0 {
		return defaultApprovalTTL
	}
	return s.TTL
}

// CreatePending inserts a pending item for a gated call (called by the engine under the
// agent principal). Implements approvalWriter. The TTL is passed in seconds so the SQL can
// use make_interval(secs => …) and avoid a pgtype.Interval param.
func (s *ApprovalStore) CreatePending(ctx context.Context, principalID, businessID, agentRunID uuid.UUID, tool string, args json.RawMessage, effect int) (uuid.UUID, error) {
	id := uuid.New()
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, e := dbgen.New(tx).CreateApprovalItem(ctx, dbgen.CreateApprovalItemParams{
			ID: id, AgentRunID: agentRunID, BusinessID: businessID,
			Tool: tool, Args: []byte(args), EffectClass: int16(effect),
			TtlSeconds: int32(s.ttl().Seconds()),
		})
		return e
	})
	if err != nil {
		return uuid.Nil, mapAgentRunErr(err)
	}
	return id, nil
}

// Get reads one item (business-scoped, no oracle).
func (s *ApprovalStore) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (ApprovalItem, error) {
	var row dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).GetApprovalItem(ctx, dbgen.GetApprovalItemParams{ID: id, BusinessID: businessID})
		row = r
		return e
	})
	if err != nil {
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return toApprovalItem(row), nil
}

// ListPending returns the business's pending queue (most recent first).
func (s *ApprovalStore) ListPending(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]ApprovalItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListPendingApprovals(ctx, dbgen.ListPendingApprovalsParams{BusinessID: businessID, State: ApprovalPending, Limit: int32(limit)})
		rows = r
		return e
	})
	if err != nil {
		return nil, mapAgentRunErr(err)
	}
	out := make([]ApprovalItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, toApprovalItem(r))
	}
	return out, nil
}

// Decide transitions a pending item to approved/denied (caller = the deciding human).
// A non-pending / expired item yields pgx.ErrNoRows → ErrConflict (409).
func (s *ApprovalStore) Decide(ctx context.Context, principalID, businessID, id uuid.UUID, decidedBy uuid.UUID, approve bool) (ApprovalItem, error) {
	state := ApprovalDenied
	if approve {
		state = ApprovalApproved
	}
	var row dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).DecideApprovalItem(ctx, dbgen.DecideApprovalItemParams{
			ID: id, BusinessID: businessID, State: state, DecidedBy: decidedBy,
		})
		row = r
		return e
	})
	if err != nil {
		// ErrNoRows here means "not pending anymore" (already decided/expired) OR unknown.
		// We cannot distinguish from the UPDATE alone; disambiguate with a follow-up read:
		// if the row EXISTS (and is RLS-visible) it was already decided/expired → 409, else 404.
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := s.Get(ctx, principalID, businessID, id); gerr == nil {
				return ApprovalItem{}, fmt.Errorf("agents: approval already decided/expired: %w", errs.ErrConflict)
			}
			return ApprovalItem{}, fmt.Errorf("agents: approval not found: %w", errs.ErrNotFound)
		}
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return toApprovalItem(row), nil
}

// Approve transitions pending→approved AND enqueues the execution event in ONE tx, so a
// committed approval always has its outbox event (no lost action — the existing outbox
// Worker dispatches it to the ApprovalExecutor). Returns ErrConflict (409) if the item is
// not pending/expired and ErrNotFound (404) if it doesn't exist (same no-oracle
// disambiguation as Decide). The enqueued payload carries everything the executor needs to
// run the tool AS the agent and to join its audit rows to the originating run by
// correlation id.
func (s *ApprovalStore) Approve(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error) {
	var item ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, e := q.DecideApprovalItem(ctx, dbgen.DecideApprovalItemParams{
			ID: id, BusinessID: businessID, State: ApprovalApproved, DecidedBy: decidedBy,
		})
		if e != nil {
			return e // ErrNoRows handled below (mapped to 409 if the row exists, else 404)
		}
		item = toApprovalItem(row)
		// Derive the acting agent principal AND the run's correlation id: the approval must
		// execute as the agent, and executor-emitted audit rows join back to the run.
		actor, ae := q.GetRunActorForApproval(ctx, dbgen.GetRunActorForApprovalParams{ID: item.AgentRunID, BusinessID: businessID})
		if ae != nil {
			return ae
		}
		return events.Enqueue(ctx, tx, item.TenantRootID, TopicAgentApproved, approvalEventPayload{
			ApprovalID: item.ID, AgentRunID: item.AgentRunID, AgentPrincipalID: actor.PrincipalID,
			BusinessID: businessID, TenantRootID: item.TenantRootID, Tool: item.Tool, Args: item.Args,
			CorrelationID: actor.CorrelationID,
		})
	})
	if err != nil {
		// ErrNoRows from the UPDATE means "not pending anymore" (already decided/expired) OR
		// the row is unknown/foreign. Disambiguate with a follow-up read (same as Decide):
		// existing+RLS-visible → 409 conflict, otherwise → 404 not-found (no oracle).
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := s.Get(ctx, principalID, businessID, id); gerr == nil {
				return ApprovalItem{}, fmt.Errorf("agents: approval already decided/expired: %w", errs.ErrConflict)
			}
			return ApprovalItem{}, fmt.Errorf("agents: approval not found: %w", errs.ErrNotFound)
		}
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return item, nil
}

// MarkExecuted is the executor's idempotency claim: approved → executed iff still
// approved. ok=false means a prior delivery already executed it (skip).
func (s *ApprovalStore) MarkExecuted(ctx context.Context, principalID, businessID, id uuid.UUID) (ok bool, err error) {
	e := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, ie := dbgen.New(tx).MarkApprovalExecuted(ctx, dbgen.MarkApprovalExecutedParams{ID: id, BusinessID: businessID})
		return ie
	})
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapAgentRunErr(e)
	}
	return true, nil
}

// MarkFailed is the executor's terminal-failure claim: approved -> failed iff still approved,
// recording reason in the error column for operator visibility. ok=false means a prior delivery
// already executed/failed it (an idempotent replay) — same contract as MarkExecuted.
func (s *ApprovalStore) MarkFailed(ctx context.Context, principalID, businessID, id uuid.UUID, reason string) (ok bool, err error) {
	e := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, ie := dbgen.New(tx).MarkApprovalFailed(ctx, dbgen.MarkApprovalFailedParams{ID: id, BusinessID: businessID, Error: reason})
		return ie
	})
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapAgentRunErr(e)
	}
	return true, nil
}

// Note: the periodic expiry sweep is NOT a store method — a system-wide sweep has no
// per-tenant principal, so it runs the SECURITY DEFINER expire_stale_approvals()
// function (migration 0032) on the principal-less tx directly in main, mirroring the
// outbox worker's definer-function pattern.

func toApprovalItem(r dbgen.ApprovalItem) ApprovalItem {
	out := ApprovalItem{
		ID: r.ID, AgentRunID: r.AgentRunID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		Tool: r.Tool, Args: json.RawMessage(r.Args), EffectClass: int(r.EffectClass),
		State: r.State, ExpiresAt: r.ExpiresAt, Error: r.Error,
	}
	if r.DecidedByPrincipalID.Valid {
		v := uuid.UUID(r.DecidedByPrincipalID.Bytes)
		out.DecidedByPrincipalID = &v
	}
	if r.ExecutedAt.Valid {
		t := r.ExecutedAt.Time
		out.ExecutedAt = &t
	}
	return out
}
