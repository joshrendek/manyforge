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
	state      string
	getCalls   int
	markCalls  int
	markOK     bool
	failCalls  int
	failReason string
}

func (f *fakeApprovalState) Get(_ context.Context, _, _, _ uuid.UUID) (ApprovalItem, error) {
	f.getCalls++
	return ApprovalItem{State: f.state, Tool: "set_status", Args: json.RawMessage(`{"ticket_id":"` + uuid.New().String() + `","status":"open"}`)}, nil
}
func (f *fakeApprovalState) MarkExecuted(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	f.markCalls++
	return f.markOK, nil
}
func (f *fakeApprovalState) MarkFailed(_ context.Context, _, _, _ uuid.UUID, reason string) (bool, error) {
	f.failCalls++
	f.failReason = reason
	return true, nil
}

func payload(t *testing.T, p approvalEventPayload) []byte {
	t.Helper()
	b, _ := json.Marshal(p)
	return b
}

func TestExecutor_ExecutesApprovedOnce(t *testing.T) {
	fts := &fakeTicketSvc{}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts, nil), Auditor: &fakeAuditor{}}
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
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts, nil), Auditor: &fakeAuditor{}}
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
	exWin := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: true}, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: win}
	if err := exWin.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle (winner): %v", err)
	}
	if got := countSuffix(win.actions, "agent.approval.executed:executed"); got != 1 {
		t.Fatalf("winner executed-audit count = %d, want 1; actions=%v", got, win.actions)
	}

	// Replay: claim loses (ok==false) → zero executed audit rows (no second "executed").
	replay := &corrAuditor{}
	exReplay := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: false}, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: replay}
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
	ex := &ApprovalExecutor{Approvals: &fakeApprovalState{state: ApprovalApproved, markOK: true}, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: aud}
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
	toolErr  bool
	result   string
	lastHint string
	lastTool string
}

func (f *fakeMCPInvoker) InvokeMCPTool(_ context.Context, _, _ uuid.UUID, tool string, _ json.RawMessage, idemHint string) (string, bool, error) {
	f.calls++
	f.lastTool = tool
	f.lastHint = idemHint
	return f.result, f.toolErr, f.err
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
		Tools:     NewToolRegistry(&fakeTicketSvc{}, nil),
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
		Tools:     NewToolRegistry(&fakeTicketSvc{}, nil),
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
		Tools:     NewToolRegistry(&fakeTicketSvc{}, nil),
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

// TestExecutor_MCP_ToolErrorResultCompletes pins manyforge-9zi: when the MCP server returns an
// error RESULT (toolErr=true, no transport error), the call has COMPLETED — the foreign side
// effect, if any, already happened. The executor must MarkExecuted (so it leaves the queue) and
// NOT reschedule (which would re-dispatch and double-fire the side effect). The error content is
// fed back as the result and the audit lands in the executed lane with an "error" outcome.
func TestExecutor_MCP_ToolErrorResultCompletes(t *testing.T) {
	invoker := &fakeMCPInvoker{result: "the remote tool failed", toolErr: true}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}, nil),
		Auditor:   aud,
		MCP:       invoker,
	}
	pl := approvalEventPayload{
		ApprovalID:       uuid.New(),
		AgentPrincipalID: uuid.New(),
		BusinessID:       uuid.New(),
		Tool:             "mcp:crm:delete_contact",
		Args:             json.RawMessage(`{"id":"123"}`),
	}
	// A completed-with-tool-error must NOT return an error (no reschedule).
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("Handle with tool-error result: want nil (no reschedule), got %v", err)
	}
	// The tool was invoked exactly once — not re-dispatched.
	if invoker.calls != 1 {
		t.Fatalf("InvokeMCPTool calls=%d, want 1 (no re-dispatch)", invoker.calls)
	}
	// MarkExecuted must fire once — the item leaves the queue.
	if st.markCalls != 1 {
		t.Fatalf("MarkExecuted calls=%d, want 1 (completed)", st.markCalls)
	}
	// Audit lands in the executed lane with an error outcome — NOT a reschedule-error.
	if got := countSuffix(aud.actions, "agent.approval.executed:error"); got != 1 {
		t.Errorf("executed-with-error audit count=%d, want 1; actions=%v", got, aud.actions)
	}
	if got := countSuffix(aud.actions, "agent.approval.error:error"); got != 0 {
		t.Errorf("reschedule-error audit count=%d, want 0; actions=%v", got, aud.actions)
	}
}

// TestExecutor_MCP_NoMCPHostConfigured verifies that when MCP is nil, an mcp: tool is a
// deterministic permanent failure: it is marked terminally 'failed' (NOT 'executed') with a
// reason, and a failed audit row is recorded (manyforge-sa8).
func TestExecutor_MCP_NoMCPHostConfigured(t *testing.T) {
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{
		Approvals: st,
		Tools:     NewToolRegistry(&fakeTicketSvc{}, nil),
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
	if st.failCalls != 1 {
		t.Errorf("MarkFailed calls=%d, want 1 (terminal failure)", st.failCalls)
	}
	if st.markCalls != 0 {
		t.Errorf("MarkExecuted calls=%d, want 0 (must NOT be marked executed)", st.markCalls)
	}
	if st.failReason != "no MCP host configured" {
		t.Errorf("fail reason = %q, want 'no MCP host configured'", st.failReason)
	}
	if got := countSuffix(aud.actions, "agent.approval.failed:failed"); got != 1 {
		t.Errorf("failed-audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}

// TestExecutor_UnknownToolMarkedFailed: an internal tool removed since proposal is a
// deterministic permanent failure → terminal 'failed' with reason (manyforge-sa8).
func TestExecutor_UnknownToolMarkedFailed(t *testing.T) {
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: aud}
	pl := approvalEventPayload{
		ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(),
		Tool: "no_such_tool", Args: json.RawMessage(`{}`),
	}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("Handle (unknown tool): %v", err)
	}
	if st.failCalls != 1 || st.markCalls != 0 {
		t.Errorf("MarkFailed=%d MarkExecuted=%d, want 1/0 (terminal failure, not executed)", st.failCalls, st.markCalls)
	}
	if st.failReason != "unknown tool" {
		t.Errorf("fail reason = %q, want 'unknown tool'", st.failReason)
	}
}

// TestExecutor_FinalAttemptTransientErrorMarkedFailed pins the dead-letter half of manyforge-sa8:
// a transient invoke error on the FINAL delivery (Attempts+1 == MaxAttempts) must record a
// terminal 'failed' (so the approval doesn't linger 'approved' forever) and return nil so the
// outbox marks the row processed rather than dead-lettering it.
func TestExecutor_FinalAttemptTransientErrorMarkedFailed(t *testing.T) {
	invoker := &fakeMCPInvoker{err: fmt.Errorf("agents: mcp_host: transport: unreachable")}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: aud, MCP: invoker}
	pl := approvalEventPayload{
		ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(),
		Tool: "mcp:crm:get_contact", Args: json.RawMessage(`{}`),
	}
	// Attempts=9, MaxAttempts=10 → this is the final try.
	ev := events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl), Attempts: 9, MaxAttempts: 10}
	if err := ex.Handle(context.Background(), nil, ev); err != nil {
		t.Fatalf("Handle final attempt: want nil (terminal-failed, not re-dead-lettered), got %v", err)
	}
	if st.failCalls != 1 {
		t.Errorf("MarkFailed calls=%d, want 1 on exhaustion", st.failCalls)
	}
	if got := countSuffix(aud.actions, "agent.approval.failed:failed"); got != 1 {
		t.Errorf("failed-audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}

// TestExecutor_NonFinalAttemptTransientErrorReschedules: the same transient error on an EARLIER
// attempt must reschedule (return error), NOT mark failed — retries may still succeed.
func TestExecutor_NonFinalAttemptTransientErrorReschedules(t *testing.T) {
	invoker := &fakeMCPInvoker{err: fmt.Errorf("agents: mcp_host: transport: unreachable")}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	aud := &corrAuditor{}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(&fakeTicketSvc{}, nil), Auditor: aud, MCP: invoker}
	pl := approvalEventPayload{
		ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(),
		Tool: "mcp:crm:get_contact", Args: json.RawMessage(`{}`),
	}
	// Attempts=0, MaxAttempts=10 → many tries left.
	ev := events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl), Attempts: 0, MaxAttempts: 10}
	if err := ex.Handle(context.Background(), nil, ev); err == nil {
		t.Fatal("Handle non-final attempt: want error (reschedule), got nil")
	}
	if st.failCalls != 0 {
		t.Errorf("MarkFailed calls=%d, want 0 (retries may still succeed)", st.failCalls)
	}
	if got := countSuffix(aud.actions, "agent.approval.error:error"); got != 1 {
		t.Errorf("reschedule-error audit count=%d, want 1; actions=%v", got, aud.actions)
	}
}
