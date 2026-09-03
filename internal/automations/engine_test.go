package automations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type fakeSteps struct {
	records    []StepRecord
	failures   []StepFailure
	waiting    bool
	deliveries map[string]uuid.UUID
	event      bool
	recordOK   bool
	failOK     bool
}

func (f *fakeSteps) Record(_ context.Context, _ pgx.Tx, r StepRecord) (bool, error) {
	f.records = append(f.records, r)
	if !f.recordOK {
		return false, nil
	}
	return true, nil
}
func (f *fakeSteps) Fail(_ context.Context, _ pgx.Tx, failure StepFailure) (bool, error) {
	f.failures = append(f.failures, failure)
	if !f.failOK {
		return false, nil
	}
	return true, nil
}
func (f *fakeSteps) Waiting(context.Context, pgx.Tx, uuid.UUID, string) (bool, error) {
	return f.waiting, nil
}
func (f *fakeSteps) Delivery(_ context.Context, _ pgx.Tx, _ uuid.UUID, nodeID string) (*uuid.UUID, error) {
	id, ok := f.deliveries[nodeID]
	if !ok {
		return nil, nil
	}
	return &id, nil
}
func (f *fakeSteps) EventExists(context.Context, pgx.Tx, uuid.UUID, string, string, time.Time, *time.Duration) (bool, error) {
	return f.event, nil
}

type fakeSubscribers struct {
	snapshot SubscriberSnapshot
	err      error
	onList   bool
}

func (f *fakeSubscribers) Snapshot(context.Context, pgx.Tx, uuid.UUID) (SubscriberSnapshot, error) {
	return f.snapshot, f.err
}
func (f *fakeSubscribers) ActiveOnList(context.Context, pgx.Tx, uuid.UUID, string, uuid.UUID) (bool, error) {
	return f.onList, nil
}
func (f *fakeSubscribers) ResolveForList(context.Context, pgx.Tx, uuid.UUID, string, uuid.UUID) (uuid.UUID, error) {
	return f.snapshot.ID, nil
}

type fakeSender struct {
	specs []MessageSpec
	id    uuid.UUID
	err   error
}

func (f *fakeSender) Enqueue(_ context.Context, _ pgx.Tx, spec MessageSpec) (uuid.UUID, error) {
	f.specs = append(f.specs, spec)
	return f.id, f.err
}

type fakeEngagement struct{ value Engagement }

func (f fakeEngagement) Engagement(context.Context, pgx.Tx, uuid.UUID) (Engagement, error) {
	return f.value, nil
}

type fakeTagger struct{ added, removed []string }

func (f *fakeTagger) AddTag(_ context.Context, _ pgx.Tx, _, _, _ uuid.UUID, tag string) error {
	f.added = append(f.added, tag)
	return nil
}
func (f *fakeTagger) RemoveTag(_ context.Context, _ pgx.Tx, _, _, _ uuid.UUID, tag string) error {
	f.removed = append(f.removed, tag)
	return nil
}

func engineFixture() (Enrollment, time.Time, *fakeSteps, *fakeSubscribers) {
	now := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	enrollment := Enrollment{
		ID: uuid.New(), BusinessID: uuid.New(), TenantRootID: uuid.New(),
		AutomationID: uuid.New(), VersionID: uuid.New(), SubscriberID: uuid.New(),
		CurrentNodeID: "trigger", WakeAt: now, EnrolledAt: now.Add(-time.Hour), ClaimGeneration: 3,
	}
	steps := &fakeSteps{recordOK: true, failOK: true, deliveries: map[string]uuid.UUID{}}
	subscribers := &fakeSubscribers{snapshot: SubscriberSnapshot{
		ID: enrollment.SubscriberID, BusinessID: enrollment.BusinessID,
		TenantRootID: enrollment.TenantRootID, ListID: uuid.New(),
		Email: "reader@example.test", Status: "active", Tags: []string{"VIP"},
	}}
	return enrollment, now, steps, subscribers
}

func TestAdvanceRunsEveryActionNodeTransactionally(t *testing.T) {
	enrollment, now, steps, subscribers := engineFixture()
	templateID, deliveryID := uuid.New(), uuid.New()
	sender, tagger := &fakeSender{id: deliveryID}, &fakeTagger{}
	graph := Graph{Nodes: []Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"event","list_id":"` + subscribers.snapshot.ListID.String() + `","name":"joined"}`)},
		{ID: "send", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + templateID.String() + `","track_opens":false,"track_clicks":true}`)},
		{ID: "add", Kind: "add_tag", Config: json.RawMessage(`{"tag":"customer"}`)},
		{ID: "remove", Kind: "remove_tag", Config: json.RawMessage(`{"tag":"lead"}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: chain("trigger", "send", "add", "remove", "exit")}
	engine := Engine{Deps: Deps{Steps: steps, Subscribers: subscribers, Sender: sender, Tagger: tagger}}

	out, err := engine.Advance(context.Background(), nil, enrollment, graph, now)
	if err != nil || out.Status != "completed" || out.NodesProcessed != 5 {
		t.Fatalf("Advance = %+v, err=%v", out, err)
	}
	if len(sender.specs) != 1 || sender.specs[0].TrackOpens || !sender.specs[0].TrackClicks || sender.specs[0].SourceKind != "automation" {
		t.Fatalf("message specs = %+v", sender.specs)
	}
	if len(tagger.added) != 1 || tagger.added[0] != "customer" || len(tagger.removed) != 1 || tagger.removed[0] != "lead" {
		t.Fatalf("tags added=%v removed=%v", tagger.added, tagger.removed)
	}
	want := []string{"advanced", "sent", "advanced", "advanced", "exited"}
	for i, record := range steps.records {
		if record.Outcome != want[i] {
			t.Fatalf("record %d outcome=%s want=%s", i, record.Outcome, want[i])
		}
	}
	if steps.records[1].DeliveryID == nil || *steps.records[1].DeliveryID != deliveryID {
		t.Fatalf("send record = %+v", steps.records[1])
	}
}

func TestAdvanceWaitParksThenResumes(t *testing.T) {
	enrollment, now, steps, subscribers := engineFixture()
	enrollment.CurrentNodeID = "wait"
	graph := Graph{Nodes: []Node{
		{ID: "wait", Kind: "wait", Config: json.RawMessage(`{"mode":"duration","seconds":120}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: chain("wait", "exit")}
	engine := Engine{Deps: Deps{Steps: steps, Subscribers: subscribers}}

	out, err := engine.Advance(context.Background(), nil, enrollment, graph, now)
	if err != nil || !out.Parked || len(steps.records) != 1 || steps.records[0].Outcome != "waiting" {
		t.Fatalf("first Advance = %+v records=%+v err=%v", out, steps.records, err)
	}
	if steps.records[0].WakeAt == nil || !steps.records[0].WakeAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("wake at = %v", steps.records[0].WakeAt)
	}

	steps.records = nil
	steps.waiting = true
	out, err = engine.Advance(context.Background(), nil, enrollment, graph, now.Add(2*time.Minute))
	if err != nil || out.Parked || out.Status != "completed" || len(steps.records) != 2 || steps.records[0].Outcome != "advanced" {
		t.Fatalf("resumed Advance = %+v records=%+v err=%v", out, steps.records, err)
	}
}

func TestWaitUntilUsesLocalWeekday(t *testing.T) {
	now := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC) // Friday, 11:00 New York.
	wake, err := waitUntil(json.RawMessage(`{"mode":"until","weekday":1,"time":"09:30","timezone":"America/New_York"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 7, 9, 30, 0, 0, wake.Location())
	if !wake.Equal(want) {
		t.Fatalf("wake=%v want=%v", wake, want)
	}
}

func TestAdvanceConditions(t *testing.T) {
	tests := []struct {
		name       string
		predicate  string
		configure  func(*fakeSteps, *fakeSubscribers) fakeEngagement
		wantResult string
	}{
		{"opened", `{"type":"opened_email","node_id":"send"}`, func(s *fakeSteps, _ *fakeSubscribers) fakeEngagement {
			s.deliveries["send"] = uuid.New()
			return fakeEngagement{Engagement{Opened: true}}
		}, "branch_yes"},
		{"exact click", `{"type":"clicked_link","node_id":"send","url":"https://example.test/a"}`, func(s *fakeSteps, _ *fakeSubscribers) fakeEngagement {
			s.deliveries["send"] = uuid.New()
			return fakeEngagement{Engagement{ClickedURLs: []string{"https://example.test/a"}}}
		}, "branch_yes"},
		{"missing delivery", `{"type":"opened_email","node_id":"send"}`, func(*fakeSteps, *fakeSubscribers) fakeEngagement { return fakeEngagement{} }, "branch_no"},
		{"tag case insensitive", `{"type":"has_tag","tag":"vip"}`, func(*fakeSteps, *fakeSubscribers) fakeEngagement { return fakeEngagement{} }, "branch_yes"},
		{"on list", `{"type":"on_list","list_id":"11111111-1111-1111-1111-111111111111"}`, func(_ *fakeSteps, s *fakeSubscribers) fakeEngagement { s.onList = true; return fakeEngagement{} }, "branch_yes"},
		{"event", `{"type":"event_received","name":"checkout","within_seconds":300}`, func(s *fakeSteps, _ *fakeSubscribers) fakeEngagement { s.event = true; return fakeEngagement{} }, "branch_yes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enrollment, now, steps, subscribers := engineFixture()
			enrollment.CurrentNodeID = "condition"
			engagement := test.configure(steps, subscribers)
			yes, no := "yes", "no"
			graph := Graph{Nodes: []Node{
				{ID: "condition", Kind: "condition", Config: json.RawMessage(`{"predicate":` + test.predicate + `}`)},
				{ID: "yes", Kind: "exit", Config: json.RawMessage(`{}`)},
				{ID: "no", Kind: "exit", Config: json.RawMessage(`{}`)},
			}, Edges: []Edge{{ID: "e1", From: "condition", To: "yes", Branch: &yes}, {ID: "e2", From: "condition", To: "no", Branch: &no}}}
			engine := Engine{Deps: Deps{Steps: steps, Subscribers: subscribers, Engagement: engagement}}
			out, err := engine.Advance(context.Background(), nil, enrollment, graph, now)
			if err != nil || out.Status != "completed" || len(steps.records) != 2 || steps.records[0].Outcome != test.wantResult {
				t.Fatalf("Advance=%+v records=%+v err=%v", out, steps.records, err)
			}
		})
	}
}

func TestAdvanceInactiveSubscriberExits(t *testing.T) {
	enrollment, now, steps, subscribers := engineFixture()
	enrollment.CurrentNodeID = "send"
	subscribers.snapshot.Status = "unsubscribed"
	graph := Graph{Nodes: []Node{{ID: "send", Kind: "send_email", Config: json.RawMessage(`{}`)}}}
	out, err := (Engine{Deps: Deps{Steps: steps, Subscribers: subscribers}}).Advance(context.Background(), nil, enrollment, graph, now)
	if err != nil || out.Status != "exited" || steps.records[0].Detail["reason"] != "unsubscribed" {
		t.Fatalf("Advance=%+v records=%+v err=%v", out, steps.records, err)
	}
}

func TestAdvanceRetriesAndTerminates(t *testing.T) {
	for _, test := range []struct {
		name       string
		attempts   int
		err        error
		terminal   bool
		retryAfter time.Duration
	}{
		{"first retry", 0, errors.New("temporary"), false, 30 * time.Second},
		{"fifth failure", 4, errors.New("temporary"), true, 8 * time.Minute},
		{"invalid reference", 0, errors.Join(errors.New("missing template"), ErrInvalidReference), true, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			enrollment, now, steps, subscribers := engineFixture()
			enrollment.CurrentNodeID, enrollment.NodeAttempts = "send", test.attempts
			sender := &fakeSender{err: test.err}
			graph := Graph{Nodes: []Node{{ID: "send", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + uuid.NewString() + `","track_opens":true,"track_clicks":true}`)}}}
			out, err := (Engine{Deps: Deps{Steps: steps, Subscribers: subscribers, Sender: sender}}).Advance(context.Background(), nil, enrollment, graph, now)
			if err != nil || len(steps.failures) != 1 || steps.failures[0].Terminal != test.terminal {
				t.Fatalf("Advance=%+v failures=%+v err=%v", out, steps.failures, err)
			}
			if got := steps.failures[0].RetryAt.Sub(now); got != test.retryAfter {
				t.Fatalf("retry delay=%v want=%v", got, test.retryAfter)
			}
		})
	}
}

func TestAdvanceDoesNotCarryRecoveredNodeAttemptsForward(t *testing.T) {
	enrollment, now, steps, subscribers := engineFixture()
	enrollment.NodeAttempts = 4
	sender := &fakeSender{err: errors.New("first failure on send")}
	graph := Graph{Nodes: []Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{}`)},
		{ID: "send", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + uuid.NewString() + `","track_opens":true,"track_clicks":true}`)},
	}, Edges: chain("trigger", "send")}

	out, err := (Engine{Deps: Deps{Steps: steps, Subscribers: subscribers, Sender: sender}}).Advance(context.Background(), nil, enrollment, graph, now)
	if err != nil || out.Status != "active" || len(steps.failures) != 1 || steps.failures[0].Terminal {
		t.Fatalf("Advance=%+v failures=%+v err=%v", out, steps.failures, err)
	}
	if got := steps.failures[0].RetryAt.Sub(now); got != 30*time.Second {
		t.Fatalf("next-node retry delay=%v want=30s", got)
	}
}

func TestAdvanceHonorsNodeBound(t *testing.T) {
	enrollment, now, steps, subscribers := engineFixture()
	nodes := make([]Node, 30)
	names := make([]string, 30)
	for i := range nodes {
		names[i] = "n" + string(rune('a'+i))
		nodes[i] = Node{ID: names[i], Kind: "trigger", Config: json.RawMessage(`{}`)}
	}
	enrollment.CurrentNodeID = names[0]
	out, err := (Engine{Deps: Deps{Steps: steps, Subscribers: subscribers}}).Advance(context.Background(), nil, enrollment, Graph{Nodes: nodes, Edges: chain(names...)}, now)
	if err != nil || out.NodesProcessed != DefaultMaxNodesPerTick || len(steps.records) != DefaultMaxNodesPerTick || out.Status != "active" {
		t.Fatalf("Advance=%+v records=%d err=%v", out, len(steps.records), err)
	}
}

func chain(ids ...string) []Edge {
	edges := make([]Edge, 0, len(ids)-1)
	for i := 0; i+1 < len(ids); i++ {
		edges = append(edges, Edge{ID: "edge", From: ids[i], To: ids[i+1]})
	}
	return edges
}
