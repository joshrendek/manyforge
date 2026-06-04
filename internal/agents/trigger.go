package agents

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// ticketCreatedPayload is the consumer-owned decode of the inbox-produced ticket.created
// outbox event (producer enqueues {ticket_id, business_id, message_id}).
type ticketCreatedPayload struct {
	TicketID   uuid.UUID `json:"ticket_id"`
	BusinessID uuid.UUID `json:"business_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

// triggerStore is the trigger's view of the run store (fakeable).
type triggerStore interface {
	EnabledAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error)
	CreateEventRun(ctx context.Context, agentPrincipalID, businessID, agentID uuid.UUID, dedupKey string, targetType *string, targetID *uuid.UUID) (bool, error)
}

// TriageTrigger subscribes to ticket.created and enqueues a queued agent_run for each
// enabled agent in the ticket's business. It runs inside the outbox worker, so it MUST be
// fast — it does NOT run the agent loop (the RunDrainer does, decoupled) — and idempotent
// (it dedups on the triggering message id; at-least-once redelivery enqueues at most one
// run per agent).
//
// LOOP-GUARD (Spec 003 §3.3): it subscribes ONLY to ticket.created (a brand-new ticket),
// never message.received. An agent's own outbound reply emits ticket.replied, never
// ticket.created, so an agent reply can NEVER re-trigger triage. Inbound auto-responders
// that open new tickets are bounded upstream by spec-002's is_auto_reply suppression cap
// (migration 0024), which suppresses ingest — and thus the ticket.created enqueue.
type TriageTrigger struct {
	Runs   triggerStore
	Logger *slog.Logger
}

// Handle implements events.Handler. It ignores the worker tx (its store opens its own
// principal-scoped txs) and is idempotent: at-least-once delivery is expected.
func (t *TriageTrigger) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p ticketCreatedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Poison payload: log + treat as processed (the producer is trusted, in-process)
		// so it doesn't retry forever.
		t.logger().ErrorContext(ctx, "triage trigger: bad ticket.created payload", "event_id", ev.ID, "err", err)
		return nil
	}
	refs, err := t.Runs.EnabledAgentsForBusiness(ctx, p.BusinessID, ev.TenantRootID)
	if err != nil {
		return err // transient → reschedule
	}
	targetType := "ticket"
	dedup := p.MessageID.String()
	for _, ref := range refs {
		if _, err := t.Runs.CreateEventRun(ctx, ref.PrincipalID, p.BusinessID, ref.AgentID, dedup, &targetType, &p.TicketID); err != nil {
			// Reschedule the whole event; runs already created dedup on retry (exactly-once
			// per agent), so partial progress is safe.
			return err
		}
	}
	return nil
}

func (t *TriageTrigger) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
