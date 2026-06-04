package agents

import (
	"context"
	"encoding/json"
	"fmt"
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

// corrAuditor captures the AgentRun + action so we can pin (a) no double-execute audit on
// an idempotent replay and (b) the originating run's correlation id on executor audits.
type corrAuditor struct {
	actions []string
	corrs   []string
}

func (a *corrAuditor) Run(_ context.Context, _ uuid.UUID, run AgentRun, action string, _, _ any, decision string) error {
	a.actions = append(a.actions, action+":"+decision)
	a.corrs = append(a.corrs, run.CorrelationID)
	return nil
}

// EX-5 fix #1: a redelivery whose MarkExecuted claim loses (ok==false — a prior delivery
// already executed the item) must NOT write a second "executed" audit row.
func TestExecutor_NoDoubleAuditOnReplay(t *testing.T) {
	tid := uuid.New()
	args := json.RawMessage(`{"ticket_id":"` + tid.String() + `","status":"open"}`)
	pl := approvalEventPayload{ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(), Tool: "set_status", Args: args}

	// Winner: claim succeeds (ok==true) → exactly one executed audit.
	win := &corrAuditor{}
	exWin := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: true}, Tools: NewToolRegistry(&fakeTicketSvc{}), Auditor: win}
	if err := exWin.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle (winner): %v", err)
	}
	if got := countSuffix(win.actions, "agent.approval.executed:executed"); got != 1 {
		t.Fatalf("winner executed-audit count = %d, want 1; actions=%v", got, win.actions)
	}

	// Replay: claim loses (ok==false) → zero executed audit rows (no second "executed").
	replay := &corrAuditor{}
	exReplay := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: false}, Tools: NewToolRegistry(&fakeTicketSvc{}), Auditor: replay}
	if err := exReplay.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle (replay): %v", err)
	}
	if got := countSuffix(replay.actions, "agent.approval.executed:executed"); got != 0 {
		t.Fatalf("replay executed-audit count = %d, want 0 (no double-audit); actions=%v", got, replay.actions)
	}
}

// EX-5 fix #2: executor-emitted audit rows carry the originating run's correlation id.
func TestExecutor_AuditCarriesCorrelationID(t *testing.T) {
	tid := uuid.New()
	corr := uuid.NewString()
	pl := approvalEventPayload{
		ApprovalID: uuid.New(), AgentRunID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(),
		Tool: "set_status", Args: json.RawMessage(`{"ticket_id":"` + tid.String() + `","status":"open"}`), CorrelationID: corr,
	}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: true}, Tools: NewToolRegistry(&fakeTicketSvc{}), Auditor: aud}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(aud.corrs) == 0 {
		t.Fatal("expected at least one audit row")
	}
	for i, c := range aud.corrs {
		if c != corr {
			t.Fatalf("audit[%d] correlation = %q, want %q", i, c, corr)
		}
	}
}

func countSuffix(actions []string, want string) int {
	n := 0
	for _, a := range actions {
		if a == want {
			n++
		}
	}
	return n
}

// fakeMCPInvoker is a controllable mcpInvoker for executor unit tests.
type fakeMCPInvoker struct {
	calls    int
	err      error
	result   string
	lastHint string
	lastTool string
}

func (f *fakeMCPInvoker) InvokeMCPTool(_ context.Context, _, _ uuid.UUID, tool string, _ json.RawMessage, idemHint string) (string, error) {
	f.calls++
	f.lastTool = tool
	f.lastHint = idemHint
	return f.result, f.err
}

// TestExecutor_MCP_ExecutesApprovedOnce verifies that an approved mcp: tool is
// dispatched through the mcpInvoker exactly once: CallTool fires once, MarkExecuted
// fires once, and the audit records "executed".
func TestExecutor_MCP_ExecutesApprovedOnce(t *testing.T) {
	invoker := &fakeMCPInvoker{result: `{"ok":true}`}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}),
		Auditor:   aud,
		MCP:       invoker,
	}
	approvalID := uuid.New()
	pl := approvalEventPayload{
		ApprovalID:       approvalID,
		AgentPrincipalID: uuid.New(),
		BusinessID:       uuid.New(),
		Tool:             "mcp:crm:get_contact",
		Args:             json.RawMessage(`{"id":"123"}`),
	}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// CallTool must have been invoked exactly once.
	if invoker.calls != 1 {
		t.Fatalf("InvokeMCPTool calls=%d, want 1", invoker.calls)
	}
	// The approval id is passed as the idempotency hint.
	if invoker.lastHint != approvalID.String() {
		t.Errorf("IdemHint=%q, want %q", invoker.lastHint, approvalID.String())
	}
	// MarkExecuted must fire once.
	if st.markCalls != 1 {
		t.Fatalf("MarkExecuted calls=%d, want 1", st.markCalls)
	}
	// Exactly one "executed" audit row.
	if got := countSuffix(aud.actions, "agent.approval.executed:executed"); got != 1 {
		t.Errorf("executed-audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}

// TestExecutor_MCP_SecondDeliverySkips verifies the exactly-once dedup: a second
// delivery where state is already "executed" (pre-check returns non-approved) must
// NOT call InvokeMCPTool again.
func TestExecutor_MCP_SecondDeliverySkips(t *testing.T) {
	invoker := &fakeMCPInvoker{result: `{"ok":true}`}
	// Second delivery: state is already executed — pre-check returns non-approved.
	st := &fakeApprovalState{state: ApprovalExecuted}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}),
		Auditor:   aud,
		MCP:       invoker,
	}
	pl := approvalEventPayload{
		ApprovalID: uuid.New(),
		Tool:       "mcp:crm:get_contact",
		Args:       json.RawMessage(`{"id":"123"}`),
	}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("Handle (second delivery): %v", err)
	}

	// InvokeMCPTool must NOT be called on the second delivery.
	if invoker.calls != 0 {
		t.Fatalf("InvokeMCPTool calls=%d on second delivery, want 0 (pre-check must gate)", invoker.calls)
	}
}

// TestExecutor_MCP_UnknownServerMarkedFailed verifies that when InvokeMCPTool
// returns an error (e.g. unknown/foreign server), the executor returns that error
// (triggering reschedule), does NOT call MarkExecuted, and records an error audit.
func TestExecutor_MCP_UnknownServerMarkedFailed(t *testing.T) {
	invoker := &fakeMCPInvoker{err: fmt.Errorf("agents: not found: server unknown")}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}),
		Auditor:   aud,
		MCP:       invoker,
	}
	pl := approvalEventPayload{
		ApprovalID:       uuid.New(),
		AgentPrincipalID: uuid.New(),
		BusinessID:       uuid.New(),
		Tool:             "mcp:foreign:some_tool",
		Args:             json.RawMessage(`{}`),
	}
	err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)})
	if err == nil {
		t.Fatal("Handle with unknown server: want error (reschedule), got nil")
	}
	// MarkExecuted must NOT fire — the failure is returned for reschedule.
	if st.markCalls != 0 {
		t.Errorf("MarkExecuted calls=%d, want 0 on tool invocation error", st.markCalls)
	}
	// An error audit row must be recorded.
	if got := countSuffix(aud.actions, "agent.approval.error:error"); got != 1 {
		t.Errorf("error-audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}

// TestExecutor_MCP_NoMCPHostConfigured verifies that when MCP is nil, an mcp: tool
// is treated as unknown: MarkExecuted fires (to drain the queue) and an error audit
// row is recorded, with no panic.
func TestExecutor_MCP_NoMCPHostConfigured(t *testing.T) {
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}),
		Auditor:   aud,
		MCP:       nil, // intentionally nil
	}
	pl := approvalEventPayload{
		ApprovalID:       uuid.New(),
		AgentPrincipalID: uuid.New(),
		BusinessID:       uuid.New(),
		Tool:             "mcp:crm:some_tool",
		Args:             json.RawMessage(`{}`),
	}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("Handle (nil MCP): %v", err)
	}
	if st.markCalls != 1 {
		t.Errorf("MarkExecuted calls=%d, want 1 (drain the queue)", st.markCalls)
	}
	if got := countSuffix(aud.actions, "agent.approval.error:error"); got != 1 {
		t.Errorf("error-audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}
