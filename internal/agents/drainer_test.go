package agents

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type fakeClaimer struct {
	queue []*ClaimedRun // popped front-to-back; an empty queue returns nil
	calls int
}

func (f *fakeClaimer) ClaimNextQueuedRun(_ context.Context) (*ClaimedRun, error) {
	f.calls++
	if len(f.queue) == 0 {
		return nil, nil
	}
	c := f.queue[0]
	f.queue = f.queue[1:]
	return c, nil
}

type fakeExecutor struct {
	ran []struct {
		principal uuid.UUID
		runID     uuid.UUID
		agentID   uuid.UUID
	}
}

func (f *fakeExecutor) Execute(_ context.Context, principalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	f.ran = append(f.ran, struct {
		principal uuid.UUID
		runID     uuid.UUID
		agentID   uuid.UUID
	}{principalID, run.ID, ag.ID})
	run.Status = RunSucceeded
	return run, nil
}

func claimedFixture() *ClaimedRun {
	tt := "ticket"
	tid := uuid.New()
	return &ClaimedRun{
		RunID: uuid.New(), CorrelationID: uuid.NewString(), TargetType: &tt, TargetID: &tid,
		Agent: Agent{ID: uuid.New(), BusinessID: uuid.New(), PrincipalID: uuid.New(), Enabled: true, AllowedTools: []string{"read_ticket"}},
	}
}

func TestRunDrainer_ClaimsAndExecutesAsAgent(t *testing.T) {
	c := claimedFixture()
	exec := &fakeExecutor{}
	d := &RunDrainer{Runs: &fakeClaimer{queue: []*ClaimedRun{c}}, Engine: exec}

	ran, err := d.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !ran {
		t.Fatal("DrainOnce returned false, want true (a run was claimed)")
	}
	if len(exec.ran) != 1 {
		t.Fatalf("executed %d runs, want 1", len(exec.ran))
	}
	got := exec.ran[0]
	if got.principal != c.Agent.PrincipalID {
		t.Errorf("executed as principal %s, want the AGENT principal %s", got.principal, c.Agent.PrincipalID)
	}
	if got.runID != c.RunID || got.agentID != c.Agent.ID {
		t.Errorf("executed run/agent = %s/%s, want %s/%s", got.runID, got.agentID, c.RunID, c.Agent.ID)
	}
}

func TestRunDrainer_EmptyQueue(t *testing.T) {
	exec := &fakeExecutor{}
	d := &RunDrainer{Runs: &fakeClaimer{}, Engine: exec}
	ran, err := d.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if ran {
		t.Fatal("DrainOnce returned true on an empty queue, want false")
	}
	if len(exec.ran) != 0 {
		t.Fatalf("executed %d runs on empty queue, want 0", len(exec.ran))
	}
}
