package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// EffectClass is each tool's static side-effect classification (design §2.4). The
// run loop's gate (gate.go) combines this with the agent's autonomy mode to decide
// auto-exec vs. approval. Ordered low→high risk; an UNKNOWN class is fail-closed to
// approval by the gate. Splitting Read from Reversible lets Mode 2 ("queue every
// write, reads inline") be expressed.
type EffectClass int

const (
	EffectRead         EffectClass = iota // pure reads — never mutate (read_ticket/read_thread)
	EffectReversible                      // reversible internal writes (status/priority/tags/assignee)
	EffectExternal                        // leaves the tenant boundary (send email)
	EffectIrreversible                    // destructive (delete/merge/billing)
)

// idemKeyCtx is the context key under which an approval id rides into a tool's Invoke
// so a write tool (draft_reply) can dedup its external side effect on execution.
type idemKeyCtx struct{}

// withApprovalKey tags ctx with the approval id so the reply tool dedups on execution.
func withApprovalKey(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, idemKeyCtx{}, id)
}

// approvalKeyFrom returns the approval idempotency key set by withApprovalKey, if any.
func approvalKeyFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(idemKeyCtx{}).(uuid.UUID)
	return id, ok
}

// ticketSvc is the subset of *ticketing.Service the tools call (lets unit tests fake it).
type ticketSvc interface {
	GetTicket(ctx context.Context, pid, bid, ticketID uuid.UUID) (ticketing.Ticket, error)
	ListMessages(ctx context.Context, pid, bid, ticketID uuid.UUID, cursor string, limit int) (ticketing.Page[ticketing.Message], error)
	Triage(ctx context.Context, pid, bid, ticketID uuid.UUID, in ticketing.TriageInput) (ticketing.Ticket, error)
	Reply(ctx context.Context, pid, bid, ticketID uuid.UUID, in ticketing.ReplyInput) (ticketing.Message, error)
	AddNote(ctx context.Context, pid, bid, ticketID uuid.UUID, in ticketing.NoteInput) (ticketing.Message, error)
}

// ConnectorGateway is the narrow surface the connector agent tools use (lets unit tests fake it).
// connectors.AgentGateway implements this interface. Exported so main.go can declare a
// typed interface variable (avoiding the typed-nil trap from *connectors.AgentGateway).
type ConnectorGateway interface {
	ReadTicketExternal(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (connectors.ExternalIssue, error)
	EnqueueComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error
	EnqueueTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error
}

// Tool is one invokable capability exposed to the model.
type Tool struct {
	Name         string
	Description  string
	SchemaJSON   string
	Effect       EffectClass
	RequiredPerm string
	Invoke       func(ctx context.Context, principalID, businessID uuid.UUID, args json.RawMessage) (string, error)
}

// ToolRegistry is the immutable set of internal tools.
type ToolRegistry struct{ tools map[string]Tool }

func (r *ToolRegistry) Get(name string) (Tool, bool) { t, ok := r.tools[name]; return t, ok }

// Names returns the registered tool names (unordered).
func (r *ToolRegistry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	return out
}

// strictUnmarshal decodes JSON into v, rejecting unknown fields (LLM output is untrusted).
func strictUnmarshal(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("agents: tool args: %v: %w", err, errs.ErrValidation)
	}
	return nil
}

type ticketRefArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
}
type setStatusArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Status   string    `json:"status"`
}
type setPriorityArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Priority string    `json:"priority"`
}
type setTagsArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Tags     []string  `json:"tags"`
}
type setAssigneeArgs struct {
	TicketID uuid.UUID  `json:"ticket_id"`
	Assignee *uuid.UUID `json:"assignee"`
}
type draftReplyArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	BodyText string    `json:"body_text"`
}
type addExternalCommentArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	BodyText string    `json:"body_text"`
}
type transitionExternalStatusArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Status   string    `json:"status"`
}

var validStatusValue = map[string]bool{"new": true, "open": true, "pending": true, "solved": true, "closed": true}
var validPriorityValue = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}

// ticketView is the redacted projection of a ticket handed to the model. It
// deliberately omits every internal identifier (tenant_root_id, business_id, the
// requester's contact_id, and all raw principal UUIDs incl. assignee_principal_id):
// the LLM never needs them and they must not leak into its context. Assignment is
// surfaced as a bool, not the assignee's principal id.
type ticketView struct {
	ID            uuid.UUID `json:"id"`
	Subject       string    `json:"subject"`
	Status        string    `json:"status"`
	Priority      string    `json:"priority"`
	Tags          []string  `json:"tags"`
	RequesterName *string   `json:"requester_name"`
	RequesterMail string    `json:"requester_email"`
	Assigned      bool      `json:"assigned"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// messageView is the redacted projection of one thread message. It exposes only
// the direction-derived author label, the body text, and the timestamp — never the
// author principal id, mail Message-ID/In-Reply-To/References routing headers, or
// the SPF/DKIM/DMARC auth results.
type messageView struct {
	Author    string    `json:"author"`
	Direction string    `json:"direction"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func toTicketView(t ticketing.Ticket) ticketView {
	return ticketView{
		ID:            t.ID,
		Subject:       t.Subject,
		Status:        t.Status,
		Priority:      t.Priority,
		Tags:          t.Tags,
		RequesterName: t.Requester.DisplayName,
		RequesterMail: t.Requester.Email,
		Assigned:      t.AssigneePrincipalID != nil,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
}

// authorLabel maps a message direction to a coarse role the model can reason about
// without ever seeing a principal id. Inbound = the customer; everything else
// (outbound replies, internal notes) = our side.
func authorLabel(direction string) string {
	if direction == "inbound" {
		return "customer"
	}
	return "agent/staff"
}

func toMessageViews(msgs []ticketing.Message) []messageView {
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		body := ""
		if m.BodyText != nil {
			body = *m.BodyText
		}
		out = append(out, messageView{
			Author:    authorLabel(m.Direction),
			Direction: m.Direction,
			Body:      body,
			CreatedAt: m.CreatedAt,
		})
	}
	return out
}

// externalTicketView is the redacted projection of an external issue handed to the model.
// It deliberately omits connector_id, secret_ref, and any other internal identifiers.
type externalTicketView struct {
	ExternalID    string                `json:"external_id"`
	URL           string                `json:"url"`
	Title         string                `json:"title"`
	Status        string                `json:"status"`
	Priority      string                `json:"priority"`
	ReporterEmail string                `json:"reporter_email"`
	ReporterName  string                `json:"reporter_name"`
	Comments      []externalCommentView `json:"comments"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

type externalCommentView struct {
	ExternalID string    `json:"external_id"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

func toExternalTicketView(iss connectors.ExternalIssue) externalTicketView {
	comments := make([]externalCommentView, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		comments = append(comments, externalCommentView{
			ExternalID: c.ExternalID,
			Author:     c.Author,
			Body:       c.Body,
			CreatedAt:  c.CreatedAt,
		})
	}
	return externalTicketView{
		ExternalID:    iss.ExternalID,
		URL:           iss.URL,
		Title:         iss.Title,
		Status:        iss.Status,
		Priority:      iss.Priority,
		ReporterEmail: iss.ReporterEmail,
		ReporterName:  iss.ReporterName,
		Comments:      comments,
		UpdatedAt:     iss.UpdatedAt,
	}
}

// keyPtrFrom wraps approvalKeyFrom, returning a *uuid.UUID (nil when no key is set).
func keyPtrFrom(ctx context.Context) *uuid.UUID {
	if k, ok := approvalKeyFrom(ctx); ok {
		return &k
	}
	return nil
}

// NewToolRegistry builds the US3 ticketing tool set, optionally extended with
// US6 connector tools when conn is non-nil.
func NewToolRegistry(svc ticketSvc, conn ConnectorGateway) *ToolRegistry {
	reg := &ToolRegistry{tools: map[string]Tool{}}
	add := func(t Tool) { reg.tools[t.Name] = t }

	add(Tool{
		Name: "read_ticket", Effect: EffectRead, RequiredPerm: authz.PermTicketsRead,
		Description: "Read a support ticket's fields (subject, status, priority, requester).",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"}},"required":["ticket_id"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a ticketRefArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			// Redaction filtering is enforced by the service: a redacted (or
			// other-business / unknown) ticket surfaces ErrNotFound, never a row.
			tkt, err := svc.GetTicket(ctx, pid, bid, a.TicketID)
			if err != nil {
				return "", err
			}
			return jsonResult(toTicketView(tkt))
		},
	})

	add(Tool{
		Name: "read_thread", Effect: EffectRead, RequiredPerm: authz.PermTicketsRead,
		Description: "Read the message thread (conversation) of a ticket.",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"}},"required":["ticket_id"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a ticketRefArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			page, err := svc.ListMessages(ctx, pid, bid, a.TicketID, "", 100)
			if err != nil {
				return "", err
			}
			views := toMessageViews(page.Items)
			// A non-nil cursor means the thread has more messages than this page;
			// tell the model so it doesn't assume it saw the whole conversation.
			if page.NextCursor != nil {
				views = append(views, messageView{
					Author:    "system",
					Direction: "system",
					Body:      "(older messages omitted)",
				})
			}
			return jsonResult(views)
		},
	})

	add(Tool{
		Name: "set_status", Effect: EffectReversible, RequiredPerm: authz.PermTicketsWrite,
		Description: "Set a ticket's status. One of: new, open, pending, solved, closed.",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"status":{"type":"string","enum":["new","open","pending","solved","closed"]}},"required":["ticket_id","status"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a setStatusArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			if !validStatusValue[a.Status] {
				return "", fmt.Errorf("agents: invalid status %q: %w", a.Status, errs.ErrValidation)
			}
			s := a.Status
			if _, err := svc.Triage(ctx, pid, bid, a.TicketID, ticketing.TriageInput{Status: &s}); err != nil {
				return "", err
			}
			return "status set to " + s, nil
		},
	})

	add(Tool{
		Name: "set_priority", Effect: EffectReversible, RequiredPerm: authz.PermTicketsWrite,
		Description: "Set a ticket's priority. One of: low, normal, high, urgent.",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"priority":{"type":"string","enum":["low","normal","high","urgent"]}},"required":["ticket_id","priority"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a setPriorityArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			if !validPriorityValue[a.Priority] {
				return "", fmt.Errorf("agents: invalid priority %q: %w", a.Priority, errs.ErrValidation)
			}
			p := a.Priority
			if _, err := svc.Triage(ctx, pid, bid, a.TicketID, ticketing.TriageInput{Priority: &p}); err != nil {
				return "", err
			}
			return "priority set to " + p, nil
		},
	})

	add(Tool{
		Name: "set_tags", Effect: EffectReversible, RequiredPerm: authz.PermTicketsWrite,
		Description: "Replace a ticket's tags with the given list (empty list clears all tags).",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"tags":{"type":"array","items":{"type":"string"}}},"required":["ticket_id","tags"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a setTagsArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			tags := a.Tags
			if _, err := svc.Triage(ctx, pid, bid, a.TicketID, ticketing.TriageInput{Tags: &tags}); err != nil {
				return "", err
			}
			return fmt.Sprintf("tags set (%d)", len(tags)), nil
		},
	})

	add(Tool{
		Name: "set_assignee", Effect: EffectReversible, RequiredPerm: authz.PermTicketsAssign,
		Description: "Assign the ticket to a member (by principal id), or unassign with null.",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"assignee":{"type":["string","null"],"format":"uuid"}},"required":["ticket_id"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a setAssigneeArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			if _, err := svc.Triage(ctx, pid, bid, a.TicketID, ticketing.TriageInput{AssigneeSet: true, Assignee: a.Assignee}); err != nil {
				return "", err
			}
			if a.Assignee == nil {
				return "unassigned", nil
			}
			return "assigned", nil
		},
	})

	add(Tool{
		Name: "draft_reply", Effect: EffectExternal, RequiredPerm: authz.PermTicketsReply,
		Description: "Compose an outbound reply to the requester. Sends email on execution.",
		SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"body_text":{"type":"string","minLength":1}},"required":["ticket_id","body_text"],"additionalProperties":false}`,
		Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
			var a draftReplyArgs
			if err := strictUnmarshal(raw, &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.BodyText) == "" {
				return "", fmt.Errorf("agents: empty reply body: %w", errs.ErrValidation)
			}
			in := ticketing.ReplyInput{BodyText: a.BodyText}
			// When this reply runs as an approved action, carry the approval id as the
			// idempotency key so an at-least-once outbox redelivery sends at most once.
			if k, ok := approvalKeyFrom(ctx); ok {
				in.IdempotencyKey = &k
			}
			if _, err := svc.Reply(ctx, pid, bid, a.TicketID, in); err != nil {
				return "", err
			}
			return "reply sent", nil
		},
	})

	// Connector tools are only registered when a gateway is available. When connectors
	// are disabled (conn == nil, e.g. MANYFORGE_CONNECTOR_MASTER_KEY unset) the tools
	// are absent from the registry and the binary boots without them.
	if conn != nil {
		add(Tool{
			Name: "read_external_ticket", Effect: EffectRead, RequiredPerm: authz.PermConnectorsRead,
			Description: "Read the external issue (Jira/Zendesk) linked to a support ticket.",
			SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"}},"required":["ticket_id"],"additionalProperties":false}`,
			Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
				var a ticketRefArgs
				if err := strictUnmarshal(raw, &a); err != nil {
					return "", err
				}
				iss, err := conn.ReadTicketExternal(ctx, pid, bid, a.TicketID)
				if err != nil {
					return "", err
				}
				return jsonResult(toExternalTicketView(iss))
			},
		})

		add(Tool{
			Name: "add_external_comment", Effect: EffectExternal, RequiredPerm: authz.PermConnectorsWrite,
			Description: "Add a comment to the external issue (Jira/Zendesk) linked to a support ticket. Records an internal note first, then enqueues the external write.",
			SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"body_text":{"type":"string","minLength":1}},"required":["ticket_id","body_text"],"additionalProperties":false}`,
			Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
				var a addExternalCommentArgs
				if err := strictUnmarshal(raw, &a); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.BodyText) == "" {
					return "", fmt.Errorf("agents: empty comment body: %w", errs.ErrValidation)
				}
				// Record an internal note first; this anchors the external write to a
				// durable message id for dedup on at-least-once outbox redelivery.
				note, err := svc.AddNote(ctx, pid, bid, a.TicketID, ticketing.NoteInput{
					BodyText:       a.BodyText,
					IdempotencyKey: keyPtrFrom(ctx),
				})
				if err != nil {
					return "", err
				}
				if err := conn.EnqueueComment(ctx, pid, bid, a.TicketID, note.ID, a.BodyText); err != nil {
					return "", err
				}
				return "external comment queued", nil
			},
		})

		add(Tool{
			Name: "transition_external_status", Effect: EffectExternal, RequiredPerm: authz.PermConnectorsWrite,
			Description: "Transition the external issue (Jira/Zendesk) linked to a support ticket to a new status.",
			SchemaJSON:  `{"type":"object","properties":{"ticket_id":{"type":"string","format":"uuid"},"status":{"type":"string","minLength":1}},"required":["ticket_id","status"],"additionalProperties":false}`,
			Invoke: func(ctx context.Context, pid, bid uuid.UUID, raw json.RawMessage) (string, error) {
				var a transitionExternalStatusArgs
				if err := strictUnmarshal(raw, &a); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Status) == "" {
					return "", fmt.Errorf("agents: empty status: %w", errs.ErrValidation)
				}
				if err := conn.EnqueueTransition(ctx, pid, bid, a.TicketID, a.Status); err != nil {
					return "", err
				}
				return "status transition queued", nil
			},
		})
	}

	return reg
}

func jsonResult(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("agents: marshal tool result: %w", err)
	}
	return string(b), nil
}
