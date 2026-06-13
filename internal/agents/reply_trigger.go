package agents

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// defaultRetriageCap backstops a zero/unset RetriageCap so a config miss never silently
// disables (cap=0) OR uncaps re-triage.
const defaultRetriageCap = 5

// messageReceivedPayload is the consumer-owned decode of the inbox-produced message.received
// event. message_id is the ticket_message ROW id (uuid), same shape as ticket.created.
type messageReceivedPayload struct {
	TicketID   uuid.UUID `json:"ticket_id"`
	BusinessID uuid.UUID `json:"business_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

// replyTriggerStore is the trigger's view of the run store (fakeable).
type replyTriggerStore interface {
	EnabledRetriageAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error)
	EnqueueReplyRetriageRun(ctx context.Context, messageID, agentID uuid.UUID, cap int) (string, error)
}

// ReplyRetriageTrigger subscribes to message.received and re-invokes each opted-in, enabled
// agent in the business when a customer replies to an existing ticket. It runs in the outbox
// worker — fast, principal-less, idempotent.
//
// SEPARATE from TriageTrigger (which is ticket.created-only): the loop-guard lives in the
// enqueue_reply_retriage_run DEFINER (inbound AND NOT is_auto_reply + per-ticket/agent hourly
// cap + dedup on the reply message id). The same dedup key as the ticket.created 'event' run
// collapses a new ticket's first message to skipped_dedup, so a fresh ticket runs once.
type ReplyRetriageTrigger struct {
	Runs        replyTriggerStore
	RetriageCap int
	Logger      *slog.Logger
}

func (t *ReplyRetriageTrigger) cap() int {
	if t.RetriageCap <= 0 {
		return defaultRetriageCap
	}
	return t.RetriageCap
}

// Handle implements events.Handler. Idempotent: the DEFINER dedups on the reply message id.
func (t *ReplyRetriageTrigger) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p messageReceivedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Poison payload: log + treat as processed (the producer is trusted, in-process).
		t.logger().ErrorContext(ctx, "reply retriage: bad message.received payload", "event_id", ev.ID, "err", err)
		return nil
	}
	refs, err := t.Runs.EnabledRetriageAgentsForBusiness(ctx, p.BusinessID, ev.TenantRootID)
	if err != nil {
		return err // transient → reschedule
	}
	for _, ref := range refs {
		outcome, eErr := t.Runs.EnqueueReplyRetriageRun(ctx, p.MessageID, ref.AgentID, t.cap())
		if eErr != nil {
			return eErr // reschedule; the DEFINER is idempotent (dedup), so a retry is safe
		}
		t.logger().DebugContext(ctx, "reply retriage",
			"agent_id", ref.AgentID, "message_id", p.MessageID, "outcome", outcome)
	}
	return nil
}

func (t *ReplyRetriageTrigger) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
