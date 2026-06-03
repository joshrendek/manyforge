package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// TopicAgentApproved is the outbox topic an approved action is enqueued under (US4).
const TopicAgentApproved = "agent.action.approved"

// approvalEventPayload is the JSON carried on TopicAgentApproved.
type approvalEventPayload struct {
	ApprovalID       uuid.UUID       `json:"approval_id"`
	AgentRunID       uuid.UUID       `json:"agent_run_id"`
	AgentPrincipalID uuid.UUID       `json:"agent_principal_id"`
	BusinessID       uuid.UUID       `json:"business_id"`
	TenantRootID     uuid.UUID       `json:"tenant_root_id"`
	Tool             string          `json:"tool"`
	Args             json.RawMessage `json:"args"`
}

// approvalReader is the executor's view of the store (pre-check + idempotency claim).
type approvalReader interface {
	Get(ctx context.Context, principalID, businessID, id uuid.UUID) (ApprovalItem, error)
	MarkExecuted(ctx context.Context, principalID, businessID, id uuid.UUID) (bool, error)
}

// The production store provably satisfies the executor's view of it.
var _ approvalReader = (*ApprovalStore)(nil)

// ApprovalExecutor runs an approved tool-call as the agent principal, exactly once.
// It subscribes to TopicAgentApproved. It does its DB work in its OWN principal-scoped
// transactions (NOT the worker tx — the worker tx has no principal context), mirroring
// the notify SendSubscriber. Exactly-once comes from (a) the pre-check + MarkExecuted
// state claim and (b) the reply dedup key (the draft_reply tool keys on ApprovalID).
//
// It does NOT re-check RBAC at execution time: the human approval (itself gated by the
// agents.approve permission) IS the authorization, and the tool runs under the agent
// principal's RLS.
type ApprovalExecutor struct {
	Approvals approvalReader
	Tools     *ToolRegistry
	Auditor   runAuditor
}

// Handle implements events.Handler. It ignores the worker tx (uses its own principal
// txs via the store) and MUST be idempotent: outbox delivery is at-least-once.
func (e *ApprovalExecutor) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p approvalEventPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Malformed payload is a poison event; log + treat as processed (return nil) so
		// it doesn't retry forever. (The producer is trusted, in-process.)
		slog.ErrorContext(ctx, "approval executor: bad payload", "err", err)
		return nil
	}
	run := AgentRun{ID: p.AgentRunID, BusinessID: p.BusinessID}

	// Pre-check: only an 'approved' item executes (skip denied/expired/already-executed).
	item, err := e.Approvals.Get(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
	if err != nil {
		return err // transient → reschedule
	}
	if item.State != ApprovalApproved {
		return nil // idempotent skip
	}

	tool, ok := e.Tools.Get(p.Tool)
	if !ok {
		// A tool removed since proposal: do not execute; mark executed-with-error so it
		// leaves the queue. (Fail-closed: never guess an action.)
		_, _ = e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
		_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": "unknown tool"}, "error")
		return nil
	}

	// Execute as the agent, keyed by the approval id (the reply tool dedups on it). The
	// args come from the (trusted) event payload, mirroring what was gated at proposal.
	execCtx := withApprovalKey(ctx, p.ApprovalID)
	out, ierr := tool.Invoke(execCtx, p.AgentPrincipalID, p.BusinessID, p.Args)
	if ierr != nil {
		_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": safeMsg(ierr)}, "error")
		return fmt.Errorf("approval execute %s: %w", p.Tool, ierr) // reschedule
	}

	// Claim: approved → executed. Already-executed (ok=false) is fine (a prior delivery
	// ran the tool; the dedup key ensured the side effect happened at most once).
	if _, merr := e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID); merr != nil {
		return merr // reschedule; re-execution is dedup-safe
	}
	_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.executed", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "result": out}, "executed")
	return nil
}
