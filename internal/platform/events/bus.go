package events

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Outbox topics. They are plain strings (the bus keys on the raw string), but the
// ones that cross module boundaries are declared here in the platform layer so a
// producer and its subscriber share one definition without importing each other —
// e.g. tenancy emits TopicBusinessCreated and inbox subscribes to it, with neither
// importing the other (no import cycle).
const (
	// TopicBusinessCreated is emitted (in the create tx) for every new business so
	// inbox can auto-provision its zero-config system inbound address (FR-001). The
	// payload carries {business_id, tenant_root_id}.
	TopicBusinessCreated = "business.created"

	// TopicTicketReplied is emitted in the reply tx; the notify worker drains it to
	// dispatch the threaded outbound email. Payload carries the message row id,
	// recipient, subject, and threading headers.
	TopicTicketReplied = "ticket.replied"

	// TopicTicketCreated fires once per brand-new ticket (inbox ingest tx). The
	// agent-runtime TriageTrigger subscribes to it — and ONLY it, never message.received —
	// so an agent's own outbound reply (which emits ticket.replied) can never re-trigger
	// triage. Payload carries {ticket_id, business_id, message_id}.
	TopicTicketCreated = "ticket.created"

	// TopicMessageReceived fans out on every non-duplicate inbound message (including the
	// first message of a brand-new ticket, which ALSO emits TopicTicketCreated). Consumed by
	// the reply re-triage trigger (manyforge-deo.1).
	TopicMessageReceived = "message.received"

	// TopicAttachmentPurge is emitted (one per blob) in the redact tx; the purge
	// worker drains it to delete the attachment object from blob storage out-of-band
	// (T066/FR-014). Payload carries {blob_key}. The handler is idempotent.
	TopicAttachmentPurge = "attachment.purge"
)

// Event is a drained outbox row handed to subscribers.
type Event struct {
	ID           uuid.UUID
	TenantRootID uuid.UUID
	Topic        string
	Payload      []byte // JSON
	Attempts     int32
	// MaxAttempts is the worker's dead-letter threshold, stamped onto the event before dispatch
	// so a handler can detect its FINAL attempt (Attempts+1 >= MaxAttempts) and record a terminal
	// outcome instead of letting the row dead-letter with the work half-done. Zero on the
	// in-process Bus path (no retry budget there).
	MaxAttempts int32
}

// Handler processes one event. It runs inside the worker's transaction (use tx
// for DB writes that must commit atomically with marking the event processed)
// and MUST be idempotent: delivery is at-least-once, so dedupe on Event.ID for
// any non-transactional side effect (e.g. outbound mail).
type Handler func(ctx context.Context, tx pgx.Tx, e Event) error

// Bus routes events to in-process topic subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
}

// NewBus creates an empty event bus.
func NewBus() *Bus { return &Bus{subs: make(map[string][]Handler)} }

// Subscribe registers h for a topic (e.g. "ticket.created", "message.received").
func (b *Bus) Subscribe(topic string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], h)
}

func (b *Bus) handlers(topic string) []Handler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.subs[topic]
}
