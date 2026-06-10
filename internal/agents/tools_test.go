package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// fakeTicketSvc records the typed calls the tools make.
type fakeTicketSvc struct {
	triageIn  ticketing.TriageInput
	triageErr error
	gotTicket uuid.UUID

	// AddNote recording
	addNoteIn  ticketing.NoteInput
	addNoteMsg ticketing.Message
	addNoteErr error
}

func (f *fakeTicketSvc) GetTicket(_ context.Context, _, _, id uuid.UUID) (ticketing.Ticket, error) {
	f.gotTicket = id
	return ticketing.Ticket{}, nil
}
func (f *fakeTicketSvc) ListMessages(_ context.Context, _, _, _ uuid.UUID, _ string, _ int) (ticketing.Page[ticketing.Message], error) {
	return ticketing.Page[ticketing.Message]{}, nil
}
func (f *fakeTicketSvc) Triage(_ context.Context, _, _, id uuid.UUID, in ticketing.TriageInput) (ticketing.Ticket, error) {
	f.gotTicket, f.triageIn = id, in
	return ticketing.Ticket{}, f.triageErr
}
func (f *fakeTicketSvc) Reply(_ context.Context, _, _, id uuid.UUID, _ ticketing.ReplyInput) (ticketing.Message, error) {
	f.gotTicket = id
	return ticketing.Message{}, nil
}
func (f *fakeTicketSvc) AddNote(_ context.Context, _, _, id uuid.UUID, in ticketing.NoteInput) (ticketing.Message, error) {
	f.gotTicket = id
	f.addNoteIn = in
	return f.addNoteMsg, f.addNoteErr
}

// fakeConnectorGateway records calls made by the connector tools.
type fakeConnectorGateway struct {
	readTicketResult connectors.ExternalIssue
	readTicketErr    error
	readCalled       bool

	enqueueCommentCalled   bool
	enqueueCommentTicketID uuid.UUID
	enqueueCommentMsgID    uuid.UUID
	enqueueCommentBody     string
	enqueueCommentErr      error

	enqueueTransitionCalled   bool
	enqueueTransitionTicketID uuid.UUID
	enqueueTransitionStatus   string
	enqueueTransitionErr      error
}

func (f *fakeConnectorGateway) ReadTicketExternal(_ context.Context, _, _, _ uuid.UUID) (connectors.ExternalIssue, error) {
	f.readCalled = true
	return f.readTicketResult, f.readTicketErr
}
func (f *fakeConnectorGateway) EnqueueComment(_ context.Context, _, _, ticketID, msgID uuid.UUID, body string) error {
	f.enqueueCommentCalled = true
	f.enqueueCommentTicketID = ticketID
	f.enqueueCommentMsgID = msgID
	f.enqueueCommentBody = body
	return f.enqueueCommentErr
}
func (f *fakeConnectorGateway) EnqueueTransition(_ context.Context, _, _, ticketID uuid.UUID, status string) error {
	f.enqueueTransitionCalled = true
	f.enqueueTransitionTicketID = ticketID
	f.enqueueTransitionStatus = status
	return f.enqueueTransitionErr
}

func TestSetStatusToolValidatesArgs(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	tool, ok := reg.Get("set_status")
	if !ok {
		t.Fatal("set_status missing")
	}
	if tool.Effect != EffectReversible {
		t.Fatalf("set_status must be Reversible, got %v", tool.Effect)
	}
	_, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","status":"banana"}`))
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("invalid status must error, got %v", err)
	}
}

func TestToolRejectsUnknownFieldAndBadUUID(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	tool, _ := reg.Get("read_ticket")
	// Both failures must carry errs.ErrValidation — that's the sentinel the
	// executor branches on to map a tool-arg fault to a 400-class outcome.
	_, badUUIDErr := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"not-a-uuid"}`))
	if badUUIDErr == nil {
		t.Fatal("bad uuid must error")
	}
	if !errors.Is(badUUIDErr, errs.ErrValidation) {
		t.Fatalf("bad uuid must be ErrValidation, got %v", badUUIDErr)
	}
	_, unknownErr := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","evil":1}`))
	if unknownErr == nil {
		t.Fatal("unknown field must error (DisallowUnknownFields)")
	}
	if !errors.Is(unknownErr, errs.ErrValidation) {
		t.Fatalf("unknown field must be ErrValidation, got %v", unknownErr)
	}
}

func TestDraftReplyIsExternalEffect(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	tool, ok := reg.Get("draft_reply")
	if !ok || tool.Effect != EffectExternal {
		t.Fatalf("draft_reply must be External, got ok=%v effect=%v", ok, tool.Effect)
	}
}

func TestUnknownToolNotFound(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	if _, ok := reg.Get("rm_minus_rf"); ok {
		t.Fatal("unknown tool must not resolve")
	}
}

func TestEffectClasses(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	want := map[string]EffectClass{
		"read_ticket":  EffectRead,
		"read_thread":  EffectRead,
		"set_status":   EffectReversible,
		"set_priority": EffectReversible,
		"set_tags":     EffectReversible,
		"set_assignee": EffectReversible,
		"draft_reply":  EffectExternal,
	}
	for name, eff := range want {
		tl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q missing from registry", name)
		}
		if tl.Effect != eff {
			t.Errorf("%s effect = %d, want %d", name, tl.Effect, eff)
		}
	}

	// Connector tools: registered when conn != nil; correct effect classes.
	fgw := &fakeConnectorGateway{}
	regConn := NewToolRegistry(&fakeTicketSvc{}, fgw)
	connWant := map[string]EffectClass{
		"read_external_ticket":       EffectRead,
		"add_external_comment":       EffectExternal,
		"transition_external_status": EffectExternal,
	}
	for name, eff := range connWant {
		tl, ok := regConn.Get(name)
		if !ok {
			t.Fatalf("connector tool %q missing from registry", name)
		}
		if tl.Effect != eff {
			t.Errorf("%s effect = %d, want %d", name, tl.Effect, eff)
		}
	}
}

func TestSetStatusInvokesTriage(t *testing.T) {
	f := &fakeTicketSvc{}
	reg := NewToolRegistry(f, nil)
	tool, _ := reg.Get("set_status")
	if _, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","status":"open"}`)); err != nil {
		t.Fatalf("valid set_status must succeed, got %v", err)
	}
	if f.triageIn.Status == nil || *f.triageIn.Status != "open" {
		t.Fatalf("Triage must receive Status=open, got %#v", f.triageIn.Status)
	}
	// Only the targeted field is set — we must not perturb priority/tags.
	if f.triageIn.Priority != nil || f.triageIn.Tags != nil {
		t.Fatalf("set_status must touch only Status, got %#v", f.triageIn)
	}
}

func TestSetTagsClearsWithEmptyList(t *testing.T) {
	f := &fakeTicketSvc{}
	reg := NewToolRegistry(f, nil)
	tool, _ := reg.Get("set_tags")
	if _, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","tags":[]}`)); err != nil {
		t.Fatalf("set_tags with empty list must succeed, got %v", err)
	}
	// Non-nil pointer to an empty slice = full replacement that clears all tags.
	if f.triageIn.Tags == nil || len(*f.triageIn.Tags) != 0 {
		t.Fatalf("empty tags must clear (non-nil, len 0), got %#v", f.triageIn.Tags)
	}
}

func TestSetAssigneeUnassignWithNull(t *testing.T) {
	f := &fakeTicketSvc{}
	reg := NewToolRegistry(f, nil)
	tool, _ := reg.Get("set_assignee")
	if _, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","assignee":null}`)); err != nil {
		t.Fatalf("set_assignee null must succeed, got %v", err)
	}
	if !f.triageIn.AssigneeSet || f.triageIn.Assignee != nil {
		t.Fatalf("null assignee must set AssigneeSet=true, Assignee=nil, got %#v", f.triageIn)
	}
}

// TestConnectorToolsValidation — bad UUID / unknown field / empty body → ErrValidation.
func TestConnectorToolsValidation(t *testing.T) {
	fgw := &fakeConnectorGateway{}
	reg := NewToolRegistry(&fakeTicketSvc{}, fgw)

	tid := uuid.New()

	tests := []struct {
		tool string
		args string
	}{
		// bad uuid for read_external_ticket
		{"read_external_ticket", `{"ticket_id":"not-a-uuid"}`},
		// unknown field for read_external_ticket
		{"read_external_ticket", `{"ticket_id":"` + tid.String() + `","evil":1}`},
		// bad uuid for add_external_comment
		{"add_external_comment", `{"ticket_id":"not-a-uuid","body_text":"hi"}`},
		// empty body for add_external_comment
		{"add_external_comment", `{"ticket_id":"` + tid.String() + `","body_text":""}`},
		// unknown field for add_external_comment
		{"add_external_comment", `{"ticket_id":"` + tid.String() + `","body_text":"hi","evil":1}`},
		// bad uuid for transition_external_status
		{"transition_external_status", `{"ticket_id":"not-a-uuid","status":"open"}`},
		// empty status for transition_external_status
		{"transition_external_status", `{"ticket_id":"` + tid.String() + `","status":""}`},
		// unknown field for transition_external_status
		{"transition_external_status", `{"ticket_id":"` + tid.String() + `","status":"open","evil":1}`},
	}

	for _, tc := range tests {
		tool, ok := reg.Get(tc.tool)
		if !ok {
			t.Fatalf("tool %q not registered", tc.tool)
		}
		_, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(tc.args))
		if err == nil {
			t.Errorf("%s args=%s: expected error, got nil", tc.tool, tc.args)
			continue
		}
		if !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s args=%s: expected ErrValidation, got %v", tc.tool, tc.args, err)
		}
	}
}

// TestConnectorToolsAbsentWhenGatewayNil — connector tools must NOT register when conn is nil.
func TestConnectorToolsAbsentWhenGatewayNil(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{}, nil)
	for _, name := range []string{"read_external_ticket", "add_external_comment", "transition_external_status"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("tool %q must not register when gateway is nil", name)
		}
	}
}

// TestReadExternalTicketReturnsView — fake gateway returns an ExternalIssue; tool returns
// JSON of the view (external id, url, title, status, priority, reporter, comments) and
// omits internal connector ids/secrets.
func TestReadExternalTicketReturnsView(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issue := connectors.ExternalIssue{
		ExternalID:    "PROJ-42",
		URL:           "https://example.atlassian.net/browse/PROJ-42",
		Title:         "Bug in login flow",
		Status:        "open",
		Priority:      "high",
		ReporterEmail: "alice@example.com",
		ReporterName:  "Alice",
		Comments: []connectors.ExternalComment{
			{ExternalID: "c1", Author: "Bob", Body: "reproducing now", CreatedAt: now},
		},
		UpdatedAt: now,
	}
	fgw := &fakeConnectorGateway{readTicketResult: issue}
	reg := NewToolRegistry(&fakeTicketSvc{}, fgw)
	tool, ok := reg.Get("read_external_ticket")
	if !ok {
		t.Fatal("read_external_ticket not registered")
	}

	tid := uuid.New()
	result, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+tid.String()+`"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fgw.readCalled {
		t.Fatal("gateway ReadTicketExternal was not called")
	}

	var view map[string]any
	if err := json.Unmarshal([]byte(result), &view); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Required fields present.
	for _, key := range []string{"external_id", "url", "title", "status", "priority", "reporter_email", "reporter_name", "comments", "updated_at"} {
		if _, ok := view[key]; !ok {
			t.Errorf("view missing key %q", key)
		}
	}

	// No internal ids/secrets leaked.
	for _, bad := range []string{"connector_id", "secret_ref", "id", "business_id"} {
		if _, ok := view[bad]; ok {
			t.Errorf("view must not contain internal field %q", bad)
		}
	}

	if view["external_id"] != "PROJ-42" {
		t.Errorf("external_id = %v, want PROJ-42", view["external_id"])
	}
}

// TestAddExternalCommentCreatesNoteThenEnqueues — approvalKeyFrom flows into
// NoteInput.IdempotencyKey, then the external comment is enqueued anchored to the note id.
// ordering: add_external_comment calls AddNote before EnqueueComment, returning early if AddNote errors.
func TestAddExternalCommentCreatesNoteThenEnqueues(t *testing.T) {
	noteID := uuid.New()
	fts := &fakeTicketSvc{
		addNoteMsg: ticketing.Message{ID: noteID},
	}
	fgw := &fakeConnectorGateway{}

	reg := NewToolRegistry(fts, fgw)
	tool, ok := reg.Get("add_external_comment")
	if !ok {
		t.Fatal("add_external_comment not registered")
	}

	approvalKey := uuid.New()
	ctx := withApprovalKey(context.Background(), approvalKey)
	tid := uuid.New()

	result, err := tool.Invoke(ctx, uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+tid.String()+`","body_text":"hello external"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// AddNote must have been called.
	if fts.addNoteIn.BodyText != "hello external" {
		t.Errorf("AddNote body = %q, want %q", fts.addNoteIn.BodyText, "hello external")
	}
	// IdempotencyKey must be the approval key.
	if fts.addNoteIn.IdempotencyKey == nil || *fts.addNoteIn.IdempotencyKey != approvalKey {
		t.Errorf("IdempotencyKey = %v, want %v", fts.addNoteIn.IdempotencyKey, approvalKey)
	}

	// EnqueueComment must have been called.
	if !fgw.enqueueCommentCalled {
		t.Fatal("EnqueueComment not called")
	}
	if fgw.enqueueCommentTicketID != tid {
		t.Errorf("EnqueueComment ticketID = %v, want %v", fgw.enqueueCommentTicketID, tid)
	}
	if fgw.enqueueCommentMsgID != noteID {
		t.Errorf("EnqueueComment messageID = %v, want %v", fgw.enqueueCommentMsgID, noteID)
	}
	if fgw.enqueueCommentBody != "hello external" {
		t.Errorf("EnqueueComment body = %q, want %q", fgw.enqueueCommentBody, "hello external")
	}
}

// TestTransitionExternalStatusEnqueues — calls EnqueueTransition with ticket_id and status.
func TestTransitionExternalStatusEnqueues(t *testing.T) {
	fgw := &fakeConnectorGateway{}
	reg := NewToolRegistry(&fakeTicketSvc{}, fgw)
	tool, ok := reg.Get("transition_external_status")
	if !ok {
		t.Fatal("transition_external_status not registered")
	}

	tid := uuid.New()
	result, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+tid.String()+`","status":"closed"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	if !fgw.enqueueTransitionCalled {
		t.Fatal("EnqueueTransition not called")
	}
	if fgw.enqueueTransitionTicketID != tid {
		t.Errorf("EnqueueTransition ticketID = %v, want %v", fgw.enqueueTransitionTicketID, tid)
	}
	if fgw.enqueueTransitionStatus != "closed" {
		t.Errorf("EnqueueTransition status = %q, want %q", fgw.enqueueTransitionStatus, "closed")
	}
}
