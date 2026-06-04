package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Loop bounds (design §8 — defaults; config-key promotion tracked as a follow-up).
const (
	defaultMaxIterations   = 8
	defaultMaxTokensPerRun = 100_000
	defaultMaxOutputTokens = 4096
	defaultWallClock       = 120 * time.Second
	defaultTemperature     = 0.0
)

// RunLimits bounds a single run. Zero-value fields fall back to defaults.
type RunLimits struct {
	MaxIterations   int
	MaxTokensPerRun int
	MaxOutputTokens int
	WallClock       time.Duration
}

func (l RunLimits) withDefaults() RunLimits {
	if l.MaxIterations <= 0 {
		l.MaxIterations = defaultMaxIterations
	}
	if l.MaxTokensPerRun <= 0 {
		l.MaxTokensPerRun = defaultMaxTokensPerRun
	}
	if l.MaxOutputTokens <= 0 {
		l.MaxOutputTokens = defaultMaxOutputTokens
	}
	if l.WallClock <= 0 {
		l.WallClock = defaultWallClock
	}
	return l
}

// ErrBudgetExceeded is returned when a run is refused/stopped for the monthly cap.
var ErrBudgetExceeded = fmt.Errorf("agents: monthly budget exceeded: %w", errs.ErrConflict)

// runStore is the subset of AgentRunStore the engine needs (fakeable in tests).
type runStore interface {
	CreateRun(ctx context.Context, principalID, businessID, agentID uuid.UUID, trigger, correlationID string, targetType *string, targetID *uuid.UUID) (AgentRun, error)
	Progress(ctx context.Context, principalID, businessID, runID uuid.UUID, status string, tokensIn, tokensOut int, costCents int64, runErr *string) (AgentRun, error)
	MonthToDateCostCents(ctx context.Context, principalID, businessID, agentID uuid.UUID) (int64, error)
}

// permChecker answers "does principal P hold key K at business B?" (wraps authz.Resolve).
type permChecker interface {
	Has(ctx context.Context, principalID, businessID uuid.UUID, key string) (bool, error)
}

// runAuditor records a per-run audit row under the agent principal's context.
type runAuditor interface {
	Run(ctx context.Context, principalID uuid.UUID, run AgentRun, action string, inputs, outputs any, decision string) error
}

// providerFactory resolves the agent's BYO credential into a Provider + resolved model id.
type providerFactory func(ctx context.Context, principalID, businessID uuid.UUID, provider string) (ai.Provider, string, error)

// approvalWriter persists a pending approval_item for a gated tool-call (US4). The
// engine calls it under the agent principal's context; the row is RLS-scoped to the
// run's business and ties back to the run via agentRunID.
type approvalWriter interface {
	CreatePending(ctx context.Context, principalID, businessID, agentRunID uuid.UUID, tool string, args json.RawMessage, effect int) (uuid.UUID, error)
}

// Engine executes the bounded agentic loop for one agent.
type Engine struct {
	Runs        runStore
	Tools       *ToolRegistry
	Auditor     runAuditor
	Resolver    permChecker
	Approvals   approvalWriter
	NewProvider providerFactory
	Cost        func(model string, u ai.Usage) int64
	Limits      RunLimits
}

// Run executes the loop. agentPrincipalID is the agent's kind='agent' principal (its
// acting identity); the business is the agent's home business (ag.BusinessID).
//
// Run always returns a terminal AgentRun. A non-nil error means EITHER the run could
// not start (disabled / over-budget / provider unavailable) OR the terminal state
// could not be persisted; callers should treat the returned AgentRun.Status as
// authoritative for the run outcome and the error as a start/persist signal. In
// particular, a per-turn provider failure mid-loop is NOT a Go error: it ends the run
// as RunFailed (Status authoritative) with err == nil.
func (e *Engine) Run(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	return e.run(ctx, agentPrincipalID, ag, trigger, targetType, targetID)
}

func (e *Engine) run(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	// Disabled agents never create a run on the synchronous (manual) path: the caller
	// gets a clean conflict with no row. (The async drain path guards again in execute,
	// marking an already-created run failed if the agent was disabled after enqueue.)
	if !ag.Enabled {
		return AgentRun{}, fmt.Errorf("agents: agent is disabled: %w", errs.ErrConflict)
	}
	run, err := e.Runs.CreateRun(ctx, agentPrincipalID, ag.BusinessID, ag.ID, trigger, uuid.NewString(), targetType, targetID)
	if err != nil {
		return AgentRun{}, err
	}
	return e.execute(ctx, agentPrincipalID, ag, run)
}

// Execute runs the bounded loop on an ALREADY-CREATED run. The Stage-2 RunDrainer calls
// it after claiming a queued run (queued→running); Run calls it right after CreateRun.
// The run carries Trigger/TargetType/TargetID/CorrelationID. It is safe to call on a run
// already marked 'running' (the in-flight Progress write is an idempotent UPDATE).
func (e *Engine) Execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	return e.execute(ctx, agentPrincipalID, ag, run)
}

func (e *Engine) execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	limits := e.Limits.withDefaults()
	businessID := ag.BusinessID
	targetType, targetID := run.TargetType, run.TargetID

	_ = e.Auditor.Run(ctx, agentPrincipalID, run, "agent.run.started", map[string]any{"agent_id": ag.ID, "trigger": run.Trigger}, nil, "started")

	// finish writes the terminal state + a completion/failure audit, and returns a
	// best-effort AgentRun (falling back to the created run if the fake store omits
	// ids). The terminal Progress write is source-of-truth: a persist failure is
	// logged AND propagated (never silently swallowed onto an otherwise-success path).
	finish := func(status, reason string, tokIn, tokOut int, cost int64) (AgentRun, error) {
		var rp *string
		if reason != "" {
			rp = &reason
		}
		r, perr := e.Runs.Progress(ctx, agentPrincipalID, businessID, run.ID, status, tokIn, tokOut, cost, rp)
		if perr != nil {
			slog.ErrorContext(ctx, "agent run: terminal state not persisted", "run_id", run.ID, "status", status, "err", perr)
		}
		action := "agent.run.completed"
		if status == RunFailed {
			action = "agent.run.failed"
		}
		_ = e.Auditor.Run(ctx, agentPrincipalID, run, action, nil, map[string]any{"status": status, "error": reason}, status)
		if r.ID == uuid.Nil {
			r = run
		}
		r.Status = status
		r.Error = rp
		return r, perr
	}

	// Async-path defense: a run created while the agent was enabled, then drained after a
	// disable, must terminate cleanly rather than execute. (Never hit on the manual path —
	// run() already returned for a disabled agent before CreateRun.)
	if !ag.Enabled {
		return finish(RunFailed, "agent disabled", 0, 0, 0)
	}

	// Budget guard (run-start). 0 ⇒ no cap. mtdStart is read ONCE here and reused for
	// the mid-run check below; under concurrency it is best-effort (the mid-run guard
	// is soft — a parallel run can race the cap; the hard ledger reconciles after).
	var mtdStart int64
	if ag.MonthlyBudgetCents > 0 {
		mtd, mErr := e.Runs.MonthToDateCostCents(ctx, agentPrincipalID, businessID, ag.ID)
		if mErr != nil {
			r, _ := finish(RunFailed, "budget lookup failed", 0, 0, 0)
			return r, mErr
		}
		mtdStart = mtd
		if mtdStart >= int64(ag.MonthlyBudgetCents) {
			r, _ := finish(RunFailed, "monthly budget exceeded", 0, 0, 0)
			return r, ErrBudgetExceeded
		}
	}

	prov, model, pErr := e.NewProvider(ctx, agentPrincipalID, businessID, ag.Provider)
	if pErr != nil {
		r, _ := finish(RunFailed, "provider unavailable", 0, 0, 0)
		return r, pErr
	}

	allow := map[string]bool{}
	var toolDefs []ai.ToolDef
	for _, name := range ag.AllowedTools {
		if t, ok := e.Tools.Get(name); ok {
			allow[name] = true
			toolDefs = append(toolDefs, ai.ToolDef{Name: t.Name, Description: t.Description, Schema: json.RawMessage(t.SchemaJSON)})
		}
	}

	loopCtx, cancel := context.WithTimeout(ctx, limits.WallClock)
	defer cancel()

	// Mark the run in-flight so a concurrent status read distinguishes it from a queued one.
	if _, perr := e.Runs.Progress(ctx, agentPrincipalID, businessID, run.ID, RunRunning, 0, 0, 0, nil); perr != nil {
		slog.WarnContext(ctx, "agent run: could not mark running", "run_id", run.ID, "err", perr)
	}

	msgs := []ai.Message{{Role: ai.RoleUser, Text: initialTask(targetType, targetID)}}
	var tokIn, tokOut int
	var costCents int64
	proposed := false

	for iter := 0; ; iter++ {
		if iter >= limits.MaxIterations {
			return finish(RunFailed, "max_iterations exceeded", tokIn, tokOut, costCents)
		}
		req := ai.Request{Model: model, System: ag.SystemPrompt, Messages: msgs, Tools: toolDefs, MaxTokens: limits.MaxOutputTokens, Temperature: defaultTemperature}
		resp, cErr := prov.Complete(loopCtx, req)
		if cErr != nil {
			if errors.Is(cErr, context.DeadlineExceeded) {
				return finish(RunFailed, "wall-clock timeout", tokIn, tokOut, costCents)
			}
			return finish(RunFailed, "provider error", tokIn, tokOut, costCents)
		}
		tokIn += resp.Usage.InputTokens
		tokOut += resp.Usage.OutputTokens
		costCents += e.Cost(model, resp.Usage)

		// Token check is strictly-greater (a run that lands exactly on the cap is
		// allowed to finish); the budget check below is >= (any run at/over the
		// monetary cap is refused) — the asymmetry is deliberate.
		if tokIn+tokOut > limits.MaxTokensPerRun {
			return finish(RunFailed, "max_tokens exceeded", tokIn, tokOut, costCents)
		}
		if ag.MonthlyBudgetCents > 0 && mtdStart+costCents >= int64(ag.MonthlyBudgetCents) {
			return finish(RunFailed, "monthly budget exceeded mid-run", tokIn, tokOut, costCents)
		}

		if resp.FinishReason != ai.FinishToolUse && len(resp.ToolCalls) == 0 {
			status := RunSucceeded
			if proposed {
				status = RunAwaitingApproval
			}
			return finish(status, "", tokIn, tokOut, costCents)
		}

		msgs = append(msgs, ai.Message{Role: ai.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		var results []ai.ToolResult
		for _, call := range resp.ToolCalls {
			content, isErr, prop := e.execTool(loopCtx, agentPrincipalID, businessID, ag.AutonomyMode, allow, run, call)
			proposed = proposed || prop
			results = append(results, ai.ToolResult{CallID: call.ID, Content: content, IsError: isErr})
		}
		msgs = append(msgs, ai.Message{Role: ai.RoleTool, ToolResults: results})
	}
}

// execTool runs the US4 fail-closed gate for one tool call. Order: allowlist → RBAC
// (Resolver.Has) → gate(effect, mode) → execute-or-queue. Returns the tool-result
// content, whether it is an error result, and whether it was queued for approval
// (which drives the run's terminal awaiting_approval status).
func (e *Engine) execTool(ctx context.Context, principalID, businessID uuid.UUID, mode int, allow map[string]bool, run AgentRun, call ai.ToolCall) (string, bool, bool) {
	audit := func(decision, detail string, inputs any) {
		_ = e.Auditor.Run(ctx, principalID, run, "agent.tool."+decision, inputs, map[string]any{"tool": call.Name, "detail": detail}, decision)
	}
	tool, ok := e.Tools.Get(call.Name)
	if !ok || !allow[call.Name] {
		audit("denied", "tool not allowed", map[string]any{"tool": call.Name})
		return "tool not permitted", true, false
	}
	// RBAC FIRST: the agent must hold the tool's permission (same authz as a human).
	if tool.RequiredPerm != "" {
		has, err := e.Resolver.Has(ctx, principalID, businessID, tool.RequiredPerm)
		if err != nil || !has {
			audit("denied", "missing permission "+tool.RequiredPerm, map[string]any{"tool": call.Name})
			return "permission denied", true, false
		}
	}
	// GATE SECOND (after RBAC, before exec): decide auto-exec vs. queue for approval.
	if gate(tool.Effect, mode) == decideApproval {
		apID, err := e.Approvals.CreatePending(ctx, principalID, businessID, run.ID, call.Name, json.RawMessage(call.Args), int(tool.Effect))
		if err != nil {
			audit("error", safeMsg(err), map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
			return "tool error: " + safeMsg(err), true, false
		}
		audit("proposed", "queued for human approval", map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args), "approval_item_id": apID})
		return "action queued for approval (id " + apID.String() + ")", false, true
	}
	out, err := tool.Invoke(ctx, principalID, businessID, call.Args)
	if err != nil {
		audit("error", safeMsg(err), map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
		return "tool error: " + safeMsg(err), true, false
	}
	audit("executed", out, map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
	return out, false, false
}

func initialTask(targetType *string, targetID *uuid.UUID) string {
	if targetType != nil && *targetType == "ticket" && targetID != nil {
		return fmt.Sprintf("Handle support ticket %s. Use the available tools to read it and take appropriate action, then summarize what you did.", targetID.String())
	}
	return "Perform your configured task using the available tools, then summarize what you did."
}

// safeMsg surfaces validation messages but never opaque internals to the model.
func safeMsg(err error) string {
	switch {
	case errors.Is(err, errs.ErrValidation):
		return err.Error()
	case errors.Is(err, errs.ErrNotFound):
		return "not found"
	default:
		return "internal error"
	}
}
