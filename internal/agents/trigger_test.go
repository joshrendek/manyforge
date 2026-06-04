package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type fakeTriggerStore struct {
	refs    []AgentRef
	created []struct {
		principal, agent uuid.UUID
		dedup            string
	}
	dedupSeen map[string]bool // (agent|dedup) already created
}

func (f *fakeTriggerStore) EnabledAgentsForBusiness(_ context.Context, _, _ uuid.UUID) ([]AgentRef, error) {
	return f.refs, nil
}

func (f *fakeTriggerStore) CreateEventRun(_ context.Context, principalID, _, agentID uuid.UUID, dedupKey string, _ *string, _ *uuid.UUID) (bool, error) {
	if f.dedupSeen == nil {
		f.dedupSeen = map[string]bool{}
	}
	k := agentID.String() + "|" + dedupKey
	if f.dedupSeen[k] {
		return false, nil
	}
	f.dedupSeen[k] = true
	f.created = append(f.created, struct {
		principal, agent uuid.UUID
		dedup            string
	}{principalID, agentID, dedupKey})
	return true, nil
}

func ticketCreatedEvent(t *testing.T, tenant, business, ticket, message uuid.UUID) events.Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticket, "business_id": business, "message_id": message,
	})
	return events.Event{ID: uuid.New(), TenantRootID: tenant, Topic: events.TopicTicketCreated, Payload: payload}
}

func TestTriageTrigger_CreatesEventRunPerEnabledAgent(t *testing.T) {
	tenant, business, ticket, message := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	a1, p1 := uuid.New(), uuid.New()
	a2, p2 := uuid.New(), uuid.New()
	store := &fakeTriggerStore{refs: []AgentRef{{a1, p1}, {a2, p2}}}
	trig := &TriageTrigger{Runs: store}

	if err := trig.Handle(context.Background(), nil, ticketCreatedEvent(t, tenant, business, ticket, message)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(store.created) != 2 {
		t.Fatalf("created %d runs, want 2 (one per enabled agent)", len(store.created))
	}
	for _, c := range store.created {
		if c.dedup != message.String() {
			t.Errorf("dedup key = %s, want triggering message id %s", c.dedup, message)
		}
	}
}

func TestTriageTrigger_DedupsRedelivery(t *testing.T) {
	tenant, business, ticket, message := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	a1, p1 := uuid.New(), uuid.New()
	store := &fakeTriggerStore{refs: []AgentRef{{a1, p1}}}
	trig := &TriageTrigger{Runs: store}
	ev := ticketCreatedEvent(t, tenant, business, ticket, message)

	for i := 0; i < 3; i++ { // at-least-once redelivery
		if err := trig.Handle(context.Background(), nil, ev); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d runs across 3 deliveries, want 1 (idempotent)", len(store.created))
	}
}

func TestTriageTrigger_PoisonPayloadIsProcessed(t *testing.T) {
	store := &fakeTriggerStore{}
	trig := &TriageTrigger{Runs: store}
	ev := events.Event{ID: uuid.New(), TenantRootID: uuid.New(), Topic: events.TopicTicketCreated, Payload: []byte("{not json")}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), ev); err != nil {
		t.Fatalf("poison payload must be treated as processed (nil err), got %v", err)
	}
	if len(store.created) != 0 {
		t.Fatalf("poison payload created %d runs, want 0", len(store.created))
	}
}
