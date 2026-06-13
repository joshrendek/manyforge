package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type fakeReplyStore struct {
	refs       []AgentRef
	refsErr    error
	enqueued   []enqCall
	enqOutcome string
	enqErr     error
	lastTenant uuid.UUID
	lastBizID  uuid.UUID
}

type enqCall struct {
	messageID uuid.UUID
	agentID   uuid.UUID
	cap       int
}

func (f *fakeReplyStore) EnabledRetriageAgentsForBusiness(_ context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	f.lastBizID, f.lastTenant = businessID, tenantRootID
	return f.refs, f.refsErr
}

func (f *fakeReplyStore) EnqueueReplyRetriageRun(_ context.Context, messageID, agentID uuid.UUID, cap int) (string, error) {
	f.enqueued = append(f.enqueued, enqCall{messageID, agentID, cap})
	if f.enqOutcome == "" {
		return "enqueued", f.enqErr
	}
	return f.enqOutcome, f.enqErr
}

func replyEvent(t *testing.T, ticketID, businessID, messageID, tenantRootID uuid.UUID) events.Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticketID, "business_id": businessID, "message_id": messageID,
	})
	return events.Event{ID: uuid.New(), TenantRootID: tenantRootID, Payload: payload}
}

// One enqueue per opted-in agent, with the configured cap and the payload's message id.
func TestReplyRetriageTrigger_EnqueuesPerAgent(t *testing.T) {
	a1, a2 := AgentRef{AgentID: uuid.New(), PrincipalID: uuid.New()}, AgentRef{AgentID: uuid.New(), PrincipalID: uuid.New()}
	store := &fakeReplyStore{refs: []AgentRef{a1, a2}}
	trig := &ReplyRetriageTrigger{Runs: store, RetriageCap: 7}
	tid, bid, mid, troot := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, tid, bid, mid, troot)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(store.enqueued) != 2 {
		t.Fatalf("enqueued %d times, want 2", len(store.enqueued))
	}
	for i, c := range store.enqueued {
		if c.messageID != mid {
			t.Errorf("call %d messageID=%s, want %s", i, c.messageID, mid)
		}
		if c.cap != 7 {
			t.Errorf("call %d cap=%d, want 7", i, c.cap)
		}
	}
	if store.lastTenant != troot || store.lastBizID != bid {
		t.Errorf("lister scoped to (%s,%s), want (%s,%s)", store.lastBizID, store.lastTenant, bid, troot)
	}
}

// Zero/unset cap backstops to the default 5 (a misconfig must not disable re-triage).
func TestReplyRetriageTrigger_CapDefaults(t *testing.T) {
	store := &fakeReplyStore{refs: []AgentRef{{AgentID: uuid.New(), PrincipalID: uuid.New()}}}
	trig := &ReplyRetriageTrigger{Runs: store} // RetriageCap unset => 0
	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, uuid.New(), uuid.New(), uuid.New(), uuid.New())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(store.enqueued) != 1 || store.enqueued[0].cap != 5 {
		t.Fatalf("cap backstop: got %+v, want one call with cap=5", store.enqueued)
	}
}

// A poison payload is logged and treated as processed (return nil — do not retry forever).
func TestReplyRetriageTrigger_BadPayloadIsProcessed(t *testing.T) {
	store := &fakeReplyStore{}
	trig := &ReplyRetriageTrigger{Runs: store}
	ev := events.Event{ID: uuid.New(), TenantRootID: uuid.New(), Payload: []byte("{not json")}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), ev); err != nil {
		t.Fatalf("bad payload should return nil, got %v", err)
	}
	if len(store.enqueued) != 0 {
		t.Fatalf("bad payload must not enqueue, got %d", len(store.enqueued))
	}
}

// A transient lister error reschedules (returns the error).
func TestReplyRetriageTrigger_ListerErrorReschedules(t *testing.T) {
	store := &fakeReplyStore{refsErr: context.DeadlineExceeded}
	trig := &ReplyRetriageTrigger{Runs: store}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, uuid.New(), uuid.New(), uuid.New(), uuid.New())); err == nil {
		t.Fatal("transient lister error must be returned (reschedule), got nil")
	}
}
