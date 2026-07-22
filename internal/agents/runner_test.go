package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// --- fakes ---

type fakeRunStore struct {
	created    AgentRun
	progress   []string
	mtd        int64
	lastTokIn  int
	lastTokOut int
	lastCost   int64
}

func (f *fakeRunStore) CreateRun(_ context.Context, _, bid, aid uuid.UUID, trig, corr string, _ *string, _ *uuid.UUID) (AgentRun, error) {
	f.created = AgentRun{ID: uuid.New(), AgentID: aid, BusinessID: bid, Trigger: trig, Status: RunQueued, CorrelationID: corr}
	return f.created, nil
}
func (f *fakeRunStore) Progress(_ context.Context, _, _, _ uuid.UUID, status string, tokensIn, tokensOut int, costCents int64, _ *string) (AgentRun, error) {
	f.progress = append(f.progress, status)
	f.lastTokIn, f.lastTokOut, f.lastCost = tokensIn, tokensOut, costCents
	return AgentRun{Status: status}, nil
}
func (f *fakeRunStore) MonthToDateCostCents(_ context.Context, _, _, _ uuid.UUID) (int64, error) {
	return f.mtd, nil
}

type fakeResolver struct{ perms map[string]bool }

func (f fakeResolver) Has(_ context.Context, _, _ uuid.UUID, key string) (bool, error) {
	return f.perms[key], nil
}

type fakeAuditor struct{ actions []string }

func (f *fakeAuditor) Run(_ context.Context, _ uuid.UUID, _ AgentRun, action string, _, _ any, decision string) error {
	f.actions = append(f.actions, action+":"+decision)
	return nil
}

type fakeApprovals struct {
	created []string // "tool:effect"
	ids     []uuid.UUID
}

func (f *fakeApprovals) CreatePending(_ context.Context, _, _, _ uuid.UUID, tool string, _ json.RawMessage, effect int) (uuid.UUID, error) {
	id := uuid.New()
	f.created = append(f.created, fmt.Sprintf("%s:%d", tool, effect))
	f.ids = append(f.ids, id)
	return id, nil
}

func toolUse(id, name, args string) ai.Response {
	return ai.Response{FinishReason: ai.FinishToolUse, ToolCalls: []ai.ToolCall{{ID: id, Name: name, Args: json.RawMessage(args)}}, Usage: ai.Usage{InputTokens: 10, OutputTokens: 5}}
}
func finalText(s string) ai.Response {
	return ai.Response{FinishReason: ai.FinishStop, Text: s, Usage: ai.Usage{InputTokens: 4, OutputTokens: 2}}
}

func newTestEngine(prov ai.Provider, store runStore, perms map[string]bool, reg *ToolRegistry) (*Engine, *fakeAuditor, *fakeApprovals) {
	aud := &fakeAuditor{}
	ap := &fakeApprovals{}
	eng := &Engine{
		Runs:      store,
		Tools:     reg,
		Auditor:   aud,
		Resolver:  fakeResolver{perms: perms},
		Approvals: ap,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return prov, "claude-sonnet-4-5", nil
		},
		Cost:   func(_, _ string, u ai.Usage) int64 { return int64(u.Total()) },
		Limits: RunLimits{MaxIterations: 4, MaxTokensPerRun: 1000, MaxOutputTokens: 256, WallClock: defaultWallClock},
	}
	return eng, aud, ap
}

func loadedAgent(tools ...string) Agent {
	return Agent{ID: uuid.New(), BusinessID: uuid.New(), PrincipalID: uuid.New(), Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "be helpful", AllowedTools: tools, AutonomyMode: ModeAssist, Enabled: true, MonthlyBudgetCents: 0}
}

func containsDecision(actions []string, suffix string) bool {
	for _, a := range actions {
		if len(a) >= len(suffix) && a[len(a)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func TestRun_SafeToolThenFinish(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(fts, nil))

	run, err := eng.run(context.Background(), uuid.New(), loadedAgent("set_status"), "manual", nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status = %s, want succeeded; transitions=%v", run.Status, store.progress)
	}
	if fts.triageIn.Status == nil || *fts.triageIn.Status != "open" {
		t.Fatalf("tool did not execute set_status; got %+v", fts.triageIn)
	}
	reqs := prov.Requests()
	if len(reqs) != 2 || len(reqs[1].Messages) == 0 {
		t.Fatalf("expected 2 provider calls with tool-result re-entry, got %d", len(reqs))
	}
}

// TestExecute_OnPreCreatedRun pins the l29 drain path: Execute runs the loop on an
// already-created run (no second CreateRun), applying the tool and finishing succeeded.
func TestExecute_OnPreCreatedRun(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(fts, nil))

	ttype := "ticket"
	pre := AgentRun{ID: uuid.New(), AgentID: uuid.New(), BusinessID: uuid.New(), Trigger: "event", TargetType: &ttype, TargetID: &tid, Status: RunRunning, CorrelationID: uuid.NewString()}
	run, err := eng.Execute(context.Background(), uuid.New(), loadedAgent("set_status"), pre)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status = %s, want succeeded; transitions=%v", run.Status, store.progress)
	}
	if store.created.ID != uuid.Nil {
		t.Fatalf("Execute called CreateRun (created.ID=%s), want none (run pre-exists)", store.created.ID)
	}
	if fts.triageIn.Status == nil || *fts.triageIn.Status != "open" {
		t.Fatalf("tool did not execute set_status; got %+v", fts.triageIn)
	}
}

func TestRun_BudgetRefusesStart(t *testing.T) {
	prov := ai.NewMockProvider(finalText("never"))
	store := &fakeRunStore{mtd: 500}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent()
	ag.MonthlyBudgetCents = 500
	run, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if err == nil {
		t.Fatal("over-budget run must return an error")
	}
	if run.Status != RunFailed {
		t.Fatalf("status=%s want failed", run.Status)
	}
	if len(prov.Requests()) != 0 {
		t.Fatal("provider must not be called when budget refuses start")
	}
}

func TestRun_AllowlistDenied(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, aud, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(fts, nil))
	_, _ = eng.run(context.Background(), uuid.New(), loadedAgent("read_ticket"), "manual", nil, nil)
	if fts.triageIn.Status != nil {
		t.Fatal("disallowed tool must NOT execute")
	}
	if !containsDecision(aud.actions, "denied") {
		t.Fatalf("denied tool must be audited; actions=%v", aud.actions)
	}
}

func TestRun_Mode1ExternalQueuesApproval(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "draft_reply", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, aud, ap := newTestEngine(prov, store, map[string]bool{"tickets.reply": true}, NewToolRegistry(fts, nil))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("draft_reply"), "manual", nil, nil)
	if fts.gotTicket != (uuid.UUID{}) {
		t.Fatal("Mode-1 external tool must be queued (no execution)")
	}
	if len(ap.created) != 1 || ap.created[0] != "draft_reply:2" { // 2 == EffectExternal
		t.Fatalf("expected one queued draft_reply approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if !containsDecision(aud.actions, "proposed") {
		t.Fatalf("queued action must be audited; actions=%v", aud.actions)
	}
}

func TestRun_Mode2QueuesReversibleWrite(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"tickets.write": true}, NewToolRegistry(fts, nil))
	ag := loadedAgent("set_status")
	ag.AutonomyMode = ModeQueueWrites
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if fts.triageIn.Status != nil {
		t.Fatal("Mode-2 must queue reversible writes, not execute them")
	}
	if len(ap.created) != 1 || ap.created[0] != "set_status:1" { // 1 == EffectReversible
		t.Fatalf("expected one queued set_status approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
}

func TestRun_Mode3AutoRunsExternal(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "draft_reply", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"tickets.reply": true}, NewToolRegistry(fts, nil))
	ag := loadedAgent("draft_reply")
	ag.AutonomyMode = ModeAutonomous
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if fts.gotTicket != tid {
		t.Fatal("Mode-3 must auto-run external tools inline")
	}
	if len(ap.created) != 0 {
		t.Fatalf("Mode-3 must not queue; got %v", ap.created)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded", run.Status)
	}
}

func TestRun_MaxIterationsBound(t *testing.T) {
	tid := uuid.New()
	resps := make([]ai.Response, 10)
	for i := range resps {
		resps[i] = toolUse("c", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`)
	}
	prov := ai.NewMockProvider(resps...)
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(&fakeTicketSvc{}, nil))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("set_status"), "manual", nil, nil)
	if run.Status != RunFailed || run.Error == nil {
		t.Fatalf("bound-hit run must fail with an error reason; got %+v", run)
	}
	if len(prov.Requests()) > eng.Limits.MaxIterations {
		t.Fatalf("loop exceeded MaxIterations: %d calls", len(prov.Requests()))
	}
}

func TestRun_DisabledAgentRefused(t *testing.T) {
	prov := ai.NewMockProvider(finalText("never"))
	eng, _, _ := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent()
	ag.Enabled = false
	if _, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil); err == nil {
		t.Fatal("disabled agent must be refused")
	}
	if len(prov.Requests()) != 0 {
		t.Fatal("provider must not be called for a disabled agent")
	}
}

func TestRun_MaxTokensBound(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`), // 10+5 tokens
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(fts, nil))
	eng.Limits.MaxTokensPerRun = 10
	run, err := eng.run(context.Background(), uuid.New(), loadedAgent("set_status"), "manual", nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || *run.Error != "max_tokens exceeded" {
		t.Fatalf("want failed/max_tokens exceeded; got %+v", run)
	}
	// The token check precedes tool execution, so the tool must NOT have run.
	if fts.triageIn.Status != nil {
		t.Fatal("tool must not execute once the token budget is blown")
	}
}

// blockingProvider blocks until the request context is cancelled, then returns its
// error — used to drive the wall-clock-timeout path deterministically.
type blockingProvider struct{}

func (blockingProvider) Complete(ctx context.Context, _ ai.Request) (ai.Response, error) {
	<-ctx.Done()
	return ai.Response{}, ctx.Err()
}
func (blockingProvider) Name() string { return "blocking" }

func TestRun_WallClockTimeout(t *testing.T) {
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(blockingProvider{}, store, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	eng.Limits.WallClock = 10 * time.Millisecond
	run, err := eng.run(context.Background(), uuid.New(), loadedAgent(), "manual", nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || *run.Error != "wall-clock timeout" {
		t.Fatalf("want failed/wall-clock timeout; got %+v", run)
	}
}

func TestRun_CostAndTokensAccumulated(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`), // 10/5
		finalText("done"), // 4/2
	)
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(&fakeTicketSvc{}, nil))
	run, err := eng.run(context.Background(), uuid.New(), loadedAgent("set_status"), "manual", nil, nil)
	if err != nil || run.Status != RunSucceeded {
		t.Fatalf("want clean success; got run=%+v err=%v", run, err)
	}
	// The terminal Progress call carries the accumulated totals (running stamp is 0).
	if store.lastTokIn != 14 || store.lastTokOut != 7 || store.lastCost != 21 {
		t.Fatalf("accumulation wrong: tokIn=%d tokOut=%d cost=%d (want 14/7/21)", store.lastTokIn, store.lastTokOut, store.lastCost)
	}
}

func TestRun_ProviderErrorFailsRun(t *testing.T) {
	prov := ai.NewMockProvider() // empty queue → exhausted on first Complete
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	run, err := eng.run(context.Background(), uuid.New(), loadedAgent(), "manual", nil, nil)
	// Per-turn provider failure is NOT a Go error: Status is authoritative.
	if err != nil {
		t.Fatalf("per-turn provider failure must not surface a Go error; got %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || *run.Error != "provider error" {
		t.Fatalf("want failed/provider error; got %+v", run)
	}
}

// errResolver fails the permission lookup, exercising the fail-closed branch.
type errResolver struct{}

func (errResolver) Has(_ context.Context, _, _ uuid.UUID, _ string) (bool, error) {
	return false, errors.New("down")
}

func TestRun_ResolverErrorDeniesTool(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	aud := &fakeAuditor{}
	eng := &Engine{
		Runs:      &fakeRunStore{},
		Tools:     NewToolRegistry(fts, nil),
		Auditor:   aud,
		Resolver:  errResolver{},
		Approvals: &fakeApprovals{},
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return prov, "claude-sonnet-4-5", nil
		},
		Cost:   func(_, _ string, u ai.Usage) int64 { return int64(u.Total()) },
		Limits: RunLimits{MaxIterations: 4, MaxTokensPerRun: 1000, MaxOutputTokens: 256, WallClock: defaultWallClock},
	}
	_, _ = eng.run(context.Background(), uuid.New(), loadedAgent("set_status"), "manual", nil, nil)
	if fts.triageIn.Status != nil {
		t.Fatal("a resolver error must fail closed — tool must NOT execute")
	}
	if !containsDecision(aud.actions, "denied") {
		t.Fatalf("resolver-error denial must be audited; actions=%v", aud.actions)
	}
}

// --- US6 T5: gate-branch pins — external connector WRITE tools ---

// TestRun_ExternalCommentQueuesInAssist — ModeAssist + add_external_comment:
// the gate must queue an approval and NOT call EnqueueComment.
func TestRun_ExternalCommentQueuesInAssist(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "add_external_comment", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	fgw := &fakeConnectorGateway{}
	store := &fakeRunStore{}
	eng, aud, ap := newTestEngine(prov, store, map[string]bool{"connectors.write": true}, NewToolRegistry(fts, fgw))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("add_external_comment"), "manual", nil, nil)
	if fgw.enqueueCommentCalled {
		t.Fatal("ModeAssist: EnqueueComment must NOT be called (tool must be queued)")
	}
	if len(ap.created) != 1 || ap.created[0] != "add_external_comment:2" { // 2 == EffectExternal
		t.Fatalf("expected one queued add_external_comment approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if !containsDecision(aud.actions, "proposed") {
		t.Fatalf("queued action must be audited proposed; actions=%v", aud.actions)
	}
}

// TestRun_ExternalCommentQueuesInQueueWrites — ModeQueueWrites + add_external_comment:
// same queued behaviour (no enqueue) as ModeAssist.
func TestRun_ExternalCommentQueuesInQueueWrites(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "add_external_comment", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	fgw := &fakeConnectorGateway{}
	eng, aud, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"connectors.write": true}, NewToolRegistry(fts, fgw))
	ag := loadedAgent("add_external_comment")
	ag.AutonomyMode = ModeQueueWrites
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if fgw.enqueueCommentCalled {
		t.Fatal("ModeQueueWrites: EnqueueComment must NOT be called (tool must be queued)")
	}
	if len(ap.created) != 1 || ap.created[0] != "add_external_comment:2" { // 2 == EffectExternal
		t.Fatalf("expected one queued add_external_comment approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if !containsDecision(aud.actions, "proposed") {
		t.Fatalf("queued action must be audited proposed; actions=%v", aud.actions)
	}
}

// TestRun_ExternalCommentExecutesInAutonomous — ModeAutonomous + add_external_comment:
// EnqueueComment must be called once; no approval queued; run succeeds.
func TestRun_ExternalCommentExecutesInAutonomous(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "add_external_comment", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	noteID := uuid.New()
	fts := &fakeTicketSvc{addNoteMsg: ticketing.Message{ID: noteID}}
	fgw := &fakeConnectorGateway{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"connectors.write": true}, NewToolRegistry(fts, fgw))
	ag := loadedAgent("add_external_comment")
	ag.AutonomyMode = ModeAutonomous
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if !fgw.enqueueCommentCalled {
		t.Fatal("ModeAutonomous: EnqueueComment must be called once inline")
	}
	if fgw.enqueueCommentMsgID != noteID {
		t.Fatalf("EnqueueComment msgID=%v want note id %v", fgw.enqueueCommentMsgID, noteID)
	}
	if len(ap.created) != 0 {
		t.Fatalf("ModeAutonomous must not queue approvals; got %v", ap.created)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded", run.Status)
	}
}

// TestRun_TransitionQueuesInAssist — ModeAssist + transition_external_status:
// the gate must queue an approval and NOT call EnqueueTransition.
func TestRun_TransitionQueuesInAssist(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "transition_external_status", `{"ticket_id":"`+tid.String()+`","status":"Done"}`),
		finalText("ok"),
	)
	fgw := &fakeConnectorGateway{}
	store := &fakeRunStore{}
	eng, aud, ap := newTestEngine(prov, store, map[string]bool{"connectors.write": true}, NewToolRegistry(&fakeTicketSvc{}, fgw))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("transition_external_status"), "manual", nil, nil)
	if fgw.enqueueTransitionCalled {
		t.Fatal("ModeAssist: EnqueueTransition must NOT be called (tool must be queued)")
	}
	if len(ap.created) != 1 || ap.created[0] != "transition_external_status:2" { // 2 == EffectExternal
		t.Fatalf("expected one queued transition_external_status approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if !containsDecision(aud.actions, "proposed") {
		t.Fatalf("queued action must be audited proposed; actions=%v", aud.actions)
	}
}

// TestRun_TransitionExecutesInAutonomous — ModeAutonomous + transition_external_status:
// EnqueueTransition must be called once; no approval queued; run succeeds.
func TestRun_TransitionExecutesInAutonomous(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "transition_external_status", `{"ticket_id":"`+tid.String()+`","status":"Done"}`),
		finalText("ok"),
	)
	fgw := &fakeConnectorGateway{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"connectors.write": true}, NewToolRegistry(&fakeTicketSvc{}, fgw))
	ag := loadedAgent("transition_external_status")
	ag.AutonomyMode = ModeAutonomous
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if !fgw.enqueueTransitionCalled {
		t.Fatal("ModeAutonomous: EnqueueTransition must be called once inline")
	}
	if len(ap.created) != 0 {
		t.Fatalf("ModeAutonomous must not queue approvals; got %v", ap.created)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded", run.Status)
	}
}

// TestRun_ExternalToolDeniedWithoutPerm — perms map lacks connectors.write:
// RBAC gate denies the tool; no enqueue, no approval; audited "denied".
func TestRun_ExternalToolDeniedWithoutPerm(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "add_external_comment", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	fgw := &fakeConnectorGateway{}
	eng, aud, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(fts, fgw))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("add_external_comment"), "manual", nil, nil)
	if fgw.enqueueCommentCalled {
		t.Fatal("RBAC-denied tool must NOT call EnqueueComment")
	}
	if len(ap.created) != 0 {
		t.Fatalf("RBAC-denied tool must NOT create an approval; got %v", ap.created)
	}
	if !containsDecision(aud.actions, "denied") {
		t.Fatalf("RBAC-denied tool must be audited denied; actions=%v", aud.actions)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded (deny is non-fatal)", run.Status)
	}
}

// TestRun_TransitionDeniedWithoutPerm — perms map lacks connectors.write: the RBAC gate
// denies transition_external_status; no EnqueueTransition, no approval; audited "denied".
// Companion to TestRun_ExternalToolDeniedWithoutPerm so BOTH write tools are pinned
// fail-closed against a missing connectors.write (manyforge-q9c).
func TestRun_TransitionDeniedWithoutPerm(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "transition_external_status", `{"ticket_id":"`+tid.String()+`","status":"Done"}`),
		finalText("ok"),
	)
	fgw := &fakeConnectorGateway{}
	eng, aud, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, fgw))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("transition_external_status"), "manual", nil, nil)
	if fgw.enqueueTransitionCalled {
		t.Fatal("RBAC-denied tool must NOT call EnqueueTransition")
	}
	if len(ap.created) != 0 {
		t.Fatalf("RBAC-denied tool must NOT create an approval; got %v", ap.created)
	}
	if !containsDecision(aud.actions, "denied") {
		t.Fatalf("RBAC-denied tool must be audited denied; actions=%v", aud.actions)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded (deny is non-fatal)", run.Status)
	}
}

// --- Task 3: web_fetch/web_search → OpenRouter server tools ---

// TestRun_WebFetchRoutedToServerTools pins the interception: for an OpenRouter agent,
// web_fetch (with web_allowed_domains) is routed to req.ServerTools (NOT the gated
// toolDefs/allow set), while a normal function tool (draft_reply) stays gated.
func TestRun_WebFetchRoutedToServerTools(t *testing.T) {
	prov := ai.NewMockProvider(finalText("done"))
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent("web_fetch", "draft_reply")
	ag.Provider = "openrouter"
	ag.WebAllowedDomains = []string{"docs.sysward.com"}

	if _, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider was never called")
	}
	req := reqs[0]

	// (a) ServerTools carries the scoped web_fetch.
	if len(req.ServerTools) != 1 {
		t.Fatalf("ServerTools len=%d want 1; got %+v", len(req.ServerTools), req.ServerTools)
	}
	st := req.ServerTools[0]
	if st.Type != "openrouter:web_fetch" {
		t.Fatalf("ServerTool type=%q want openrouter:web_fetch", st.Type)
	}
	if len(st.AllowedDomains) != 1 || st.AllowedDomains[0] != "docs.sysward.com" {
		t.Fatalf("ServerTool domains=%v want [docs.sysward.com]", st.AllowedDomains)
	}

	// (a cont.) draft_reply stays a gated function tool; web_fetch must NOT.
	var sawDraft, sawFetch bool
	for _, td := range req.Tools {
		if td.Name == "draft_reply" {
			sawDraft = true
		}
		if td.Name == "web_fetch" {
			sawFetch = true
		}
	}
	if !sawDraft {
		t.Fatalf("draft_reply must remain in gated Tools; got %+v", req.Tools)
	}
	if sawFetch {
		t.Fatal("web_fetch must NOT appear in the gated Tools (it is a server tool)")
	}
}

// TestRun_WebFetchSkippedWhenNoDomains is the SAFETY GUARD: web_fetch without
// web_allowed_domains must NOT enable an unscoped server-side fetch.
func TestRun_WebFetchSkippedWhenNoDomains(t *testing.T) {
	prov := ai.NewMockProvider(finalText("done"))
	eng, _, _ := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent("web_fetch")
	ag.Provider = "openrouter"
	ag.WebAllowedDomains = nil

	if _, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider was never called")
	}
	if len(reqs[0].ServerTools) != 0 {
		t.Fatalf("web_fetch without domains must NOT enable a server tool; got %+v", reqs[0].ServerTools)
	}
}

// TestRun_ServerToolsOnlyForOpenRouter — a non-openrouter agent never gets ServerTools
// built (even with domains set), keeping the path clean for anthropic/openai/etc.
func TestRun_ServerToolsOnlyForOpenRouter(t *testing.T) {
	prov := ai.NewMockProvider(finalText("done"))
	eng, _, _ := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent("web_fetch")
	ag.Provider = "anthropic"
	ag.WebAllowedDomains = []string{"docs.sysward.com"}

	if _, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider was never called")
	}
	if len(reqs[0].ServerTools) != 0 {
		t.Fatalf("non-openrouter agent must get NO ServerTools; got %+v", reqs[0].ServerTools)
	}
}

// TestRun_WebSearchRoutedToServerTools — web_search needs no domains; it routes to a
// ServerTool with type openrouter:web_search and empty AllowedDomains.
func TestRun_WebSearchRoutedToServerTools(t *testing.T) {
	prov := ai.NewMockProvider(finalText("done"))
	eng, _, _ := newTestEngine(prov, &fakeRunStore{}, map[string]bool{}, NewToolRegistry(&fakeTicketSvc{}, nil))
	ag := loadedAgent("web_search")
	ag.Provider = "openrouter"

	if _, err := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider was never called")
	}
	if len(reqs[0].ServerTools) != 1 || reqs[0].ServerTools[0].Type != "openrouter:web_search" {
		t.Fatalf("web_search must route to one openrouter:web_search ServerTool; got %+v", reqs[0].ServerTools)
	}
	if len(reqs[0].ServerTools[0].AllowedDomains) != 0 {
		t.Fatalf("web_search ServerTool must carry no domains; got %v", reqs[0].ServerTools[0].AllowedDomains)
	}
}

// TestRun_ReadExternalRunsInline — read_external_ticket (EffectRead) executes inline
// in ModeAssist (reads never queue); gateway ReadTicketExternal called once.
func TestRun_ReadExternalRunsInline(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "read_external_ticket", `{"ticket_id":"`+tid.String()+`"}`),
		finalText("ok"),
	)
	fgw := &fakeConnectorGateway{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"connectors.read": true}, NewToolRegistry(&fakeTicketSvc{}, fgw))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("read_external_ticket"), "manual", nil, nil)
	if !fgw.readCalled {
		t.Fatal("ModeAssist read tool must execute inline; ReadTicketExternal was not called")
	}
	if len(ap.created) != 0 {
		t.Fatalf("read tools must never queue approvals; got %v", ap.created)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded", run.Status)
	}
}
