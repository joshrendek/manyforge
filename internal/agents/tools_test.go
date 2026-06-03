package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// fakeTicketSvc records the typed calls the tools make.
type fakeTicketSvc struct {
	triageIn  ticketing.TriageInput
	triageErr error
	gotTicket uuid.UUID
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

func TestSetStatusToolValidatesArgs(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{})
	tool, ok := reg.Get("set_status")
	if !ok {
		t.Fatal("set_status missing")
	}
	if tool.Effect != EffectSafe {
		t.Fatalf("set_status must be Safe, got %v", tool.Effect)
	}
	_, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","status":"banana"}`))
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("invalid status must error, got %v", err)
	}
}

func TestToolRejectsUnknownFieldAndBadUUID(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{})
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
	reg := NewToolRegistry(&fakeTicketSvc{})
	tool, ok := reg.Get("draft_reply")
	if !ok || tool.Effect != EffectExternal {
		t.Fatalf("draft_reply must be External, got ok=%v effect=%v", ok, tool.Effect)
	}
}

func TestUnknownToolNotFound(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{})
	if _, ok := reg.Get("rm_minus_rf"); ok {
		t.Fatal("unknown tool must not resolve")
	}
}

func TestSafeToolsCoverExpectedSet(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{})
	for _, n := range []string{"read_ticket", "read_thread", "set_status", "set_priority", "set_tags", "set_assignee"} {
		tool, ok := reg.Get(n)
		if !ok {
			t.Fatalf("%s missing", n)
		}
		if tool.Effect != EffectSafe {
			t.Fatalf("%s must be Safe", n)
		}
	}
}

func TestSetStatusInvokesTriage(t *testing.T) {
	f := &fakeTicketSvc{}
	reg := NewToolRegistry(f)
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
	reg := NewToolRegistry(f)
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
	reg := NewToolRegistry(f)
	tool, _ := reg.Get("set_assignee")
	if _, err := tool.Invoke(context.Background(), uuid.New(), uuid.New(), []byte(`{"ticket_id":"`+uuid.New().String()+`","assignee":null}`)); err != nil {
		t.Fatalf("set_assignee null must succeed, got %v", err)
	}
	if !f.triageIn.AssigneeSet || f.triageIn.Assignee != nil {
		t.Fatalf("null assignee must set AssigneeSet=true, Assignee=nil, got %#v", f.triageIn)
	}
}
