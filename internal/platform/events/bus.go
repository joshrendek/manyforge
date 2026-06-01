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
)

// Event is a drained outbox row handed to subscribers.
type Event struct {
	ID           uuid.UUID
	TenantRootID uuid.UUID
	Topic        string
	Payload      []byte // JSON
	Attempts     int32
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
