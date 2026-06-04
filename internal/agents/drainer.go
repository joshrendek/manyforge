package agents

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// runClaimer + runExecutor are the drainer's narrow views (fakeable; *AgentRunStore and
// *Engine satisfy them in production).
type runClaimer interface {
	ClaimNextQueuedRun(ctx context.Context) (*ClaimedRun, error)
}
type runExecutor interface {
	Execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error)
}

// The production types provably satisfy the drainer's and trigger's views of them.
var (
	_ runClaimer   = (*AgentRunStore)(nil)
	_ runExecutor  = (*Engine)(nil)
	_ triggerStore = (*AgentRunStore)(nil)
)

// RunDrainer claims queued agent_runs and executes them as the agent, decoupled from the
// outbox worker so a long run (up to the wall-clock bound) never stalls event delivery.
// One DrainOnce claims+runs a single queued run; the background loop in main.go calls it
// until the queue drains, then ticks. The queued→running claim (SKIP LOCKED, in the
// definer fn) is the exactly-once gate: no two drainers ever execute the same run.
type RunDrainer struct {
	Runs   runClaimer
	Engine runExecutor
	Logger *slog.Logger
}

// DrainOnce claims and executes at most one queued run. Returns (true, nil) when a run was
// claimed+executed (caller should loop to drain more), (false, nil) when the queue is empty.
func (d *RunDrainer) DrainOnce(ctx context.Context) (bool, error) {
	claimed, err := d.Runs.ClaimNextQueuedRun(ctx)
	if err != nil {
		return false, err
	}
	if claimed == nil {
		return false, nil
	}
	run := AgentRun{
		ID: claimed.RunID, AgentID: claimed.Agent.ID, BusinessID: claimed.Agent.BusinessID,
		Trigger: "event", TargetType: claimed.TargetType, TargetID: claimed.TargetID,
		Status: RunRunning, CorrelationID: claimed.CorrelationID,
	}
	if _, eerr := d.Engine.Execute(ctx, claimed.Agent.PrincipalID, claimed.Agent, run); eerr != nil {
		// Execute already persisted a terminal (failed) state + audit; log and continue so
		// one bad run never wedges the drain loop.
		d.logger().ErrorContext(ctx, "run drainer: execute", "run_id", claimed.RunID, "err", eerr)
	}
	return true, nil
}

func (d *RunDrainer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
