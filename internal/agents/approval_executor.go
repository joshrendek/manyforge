package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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
	// CorrelationID is the originating run's correlation id, so executor-emitted audit
	// rows join back to the run that proposed the action.
	CorrelationID string `json:"correlation_id"`
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
//
// MCP tools (tool name prefixed "mcp:") are dispatched through MCP (if set). When MCP
// is nil, mcp: tools are treated as unknown and marked failed. Internal tools use the
// Tools registry as before.
type ApprovalExecutor struct {
	Approvals approvalReader
	Tools     *ToolRegistry
	Auditor   runAuditor
	// MCP is optional. When set, approved tool calls whose name starts with "mcp:"
	// are dispatched through it instead of the internal ToolRegistry.
	MCP mcpInvoker
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
	run := AgentRun{ID: p.AgentRunID, BusinessID: p.BusinessID, CorrelationID: p.CorrelationID}

	// Pre-check: only an 'approved' item executes (skip denied/expired/already-executed).
	item, err := e.Approvals.Get(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
	if err != nil {
		return err // transient → reschedule
	}
	if item.State != ApprovalApproved {
		return nil // idempotent skip
	}

	// Execute as the agent, keyed by the approval id (the reply tool dedups on it). The
	// args come from the (trusted) event payload, mirroring what was gated at proposal.
	execCtx := withApprovalKey(ctx, p.ApprovalID)
	var out string
	var ierr error
	// toolErr distinguishes a COMPLETED call whose tool returned an error result (MCP isError)
	// from a TRANSPORT failure (ierr). A completed call must NOT reschedule — its side effect, if
	// any, already happened on the remote (manyforge-9zi). Internal tools leave this false.
	var toolErr bool

	if strings.HasPrefix(p.Tool, "mcp:") {
		// MCP tool path: resolve and invoke via the MCPHost. The approval id is passed
		// as idemHint so the remote server can (optionally) deduplicate on it.
		// At-least-once caveat (design §3.6): see InvokeMCPTool for the full note.
		if e.MCP == nil {
			// No MCP host wired — treat as unknown tool (fail-closed).
			_, _ = e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
			_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": "no MCP host configured"}, "error")
			return nil
		}
		out, toolErr, ierr = e.MCP.InvokeMCPTool(execCtx, p.AgentPrincipalID, p.BusinessID, p.Tool, p.Args, p.ApprovalID.String())
	} else {
		// Internal tool path: look up in the registry and invoke.
		tool, ok := e.Tools.Get(p.Tool)
		if !ok {
			// A tool removed since proposal: do not execute; mark executed-with-error so it
			// leaves the queue. (Fail-closed: never guess an action.)
			_, _ = e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
			_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": "unknown tool"}, "error")
			return nil
		}
		out, ierr = tool.Invoke(execCtx, p.AgentPrincipalID, p.BusinessID, p.Args)
	}

	if ierr != nil {
		_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": safeMsg(ierr)}, "error")
		return fmt.Errorf("approval execute %s: %w", p.Tool, ierr) // reschedule
	}

	// Claim: approved → executed. ok=false means a prior delivery already executed this
	// item (an idempotent replay); the dedup key ensured the side effect happened at most
	// once, so we must NOT write a second "executed" audit row. Only the delivery that
	// wins the claim (ok=true) records the executed audit.
	okMarked, merr := e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
	if merr != nil {
		return merr // reschedule; re-execution is dedup-safe
	}
	if okMarked {
		if toolErr {
			// The call COMPLETED but the tool returned an error result (MCP isError). It is not
			// rescheduled — the foreign side effect, if any, already happened. Record it in the
			// executed lane with an "error" outcome and feed the error content back as the result
			// so the model can react to it (manyforge-9zi).
			_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.executed", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "result": out, "tool_error": true}, "error")
		} else {
			_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.executed", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "result": out}, "executed")
		}
	}
	return nil
}
