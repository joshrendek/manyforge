package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// fakeApprovalState drives the executor's pre-check + claim deterministically.
type fakeApprovalState struct {
	state     string
	getCalls  int
	markCalls int
	markOK    bool
}

func (f *fakeApprovalState) Get(_ context.Context, _, _, _ uuid.UUID) (ApprovalItem, error) {
	f.getCalls++
	return ApprovalItem{State: f.state, Tool: "set_status", Args: json.RawMessage(`{"ticket_id":"` + uuid.New().String() + `","status":"open"}`)}, nil
}
func (f *fakeApprovalState) MarkExecuted(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	f.markCalls++
	return f.markOK, nil
}

func payload(t *testing.T, p approvalEventPayload) []byte {
	t.Helper()
	b, _ := json.Marshal(p)
	return b
}

func TestExecutor_ExecutesApprovedOnce(t *testing.T) {
	fts := &fakeTicketSvc{}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts), Auditor: &fakeAuditor{}}
	// The executor invokes the tool with p.Args from the PAYLOAD (not the Get result),
	// so the payload must carry valid args matching the named tool.
	tid := uuid.New()
	pl := approvalEventPayload{
		ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(), TenantRootID: uuid.New(),
		Tool: "set_status", Args: json.RawMessage(`{"ticket_id":"` + tid.String() + `","status":"open"}`),
	}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if fts.triageIn.Status == nil {
		t.Fatal("approved tool must execute")
	}
	if st.markCalls != 1 {
		t.Fatalf("mark-executed calls=%d want 1", st.markCalls)
	}
}

func TestExecutor_SkipsNonApproved(t *testing.T) {
	fts := &fakeTicketSvc{}
	st := &fakeApprovalState{state: ApprovalExecuted} // already done
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts), Auditor: &fakeAuditor{}}
	pl := approvalEventPayload{ApprovalID: uuid.New(), Tool: "set_status"}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if fts.triageIn.Status != nil {
		t.Fatal("a non-approved item must NOT execute (idempotent skip)")
	}
}
