package connectors

import (
	"context"
	"net/http"
	"time"
)

// Outbox topics for the sync engine. The connectors package owns them (mirroring how
// agents owns TopicAgentApproved). US3 subscribes a connector-sync handler to these.
const (
	TopicConnectorInboundSync  = "connector.inbound.sync"
	TopicConnectorOutboundSync = "connector.outbound.sync"
)

// ExternalComment is one comment on an external issue.
type ExternalComment struct {
	ExternalID string
	Author     string // display name of the commenter (for UI attribution, not identity resolution)
	Body       string
	CreatedAt  time.Time
}

// ExternalIssue is the external system's view of a ticket (Jira issue / Zendesk ticket).
type ExternalIssue struct {
	ExternalID    string
	URL           string
	Title         string
	Status        string
	Priority      string
	ReporterEmail string // maps to requester (deduped by email); empty if the external system hides it
	ReporterName  string // optional display name; empty is fine
	Comments      []ExternalComment
	UpdatedAt     time.Time
}

// ExternalIssueDraft is the minimal payload to create a new external issue (US4 outbound).
type ExternalIssueDraft struct {
	ProjectKey    string // required: external project/space key the issue is created in
	IssueType     string // required: external issue type name (e.g. "Task")
	Summary       string // maps from native ticket.subject
	Description   string // maps from the native message body
	ReporterEmail string // best-effort; empty is fine
}

// WebhookEvent is the routing info decoded from an inbound webhook payload.
type WebhookEvent struct {
	DeliveryID string // unique per delivery, for replay dedupe
	ExternalID string // the external issue this event concerns
	Kind       string // e.g. "issue.updated", "comment.created"
}

// TicketingConnector is the capability contract every external ticketing system
// implements. A live instance is bound (by the Registry) to one business's resolved
// credential + an SSRF-safe HTTP client. US3 implements Jira; US5 implements Zendesk.
// US3 may refine these signatures against the real Jira API.
type TicketingConnector interface {
	// FetchIssue returns the external issue + its comments by external id.
	FetchIssue(ctx context.Context, externalID string) (ExternalIssue, error)
	// PostComment appends a comment, returning the created comment's metadata. When internal
	// is true the comment is posted as INTERNAL/private (JSM internal comment / Zendesk
	// private comment) — visible to agents only, never to the requester.
	PostComment(ctx context.Context, externalID, body string, internal bool) (ExternalComment, error)
	// TransitionStatus moves the external issue to the target status.
	TransitionStatus(ctx context.Context, externalID, status string) error
	// ListUpdatedSince returns external issue ids updated at/after the cursor (reconcile).
	ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error)
	// VerifyWebhook checks the inbound payload's signature (per-connector secret).
	VerifyWebhook(headers http.Header, body []byte) error
	// DecodeWebhook extracts routing info from a verified inbound payload.
	DecodeWebhook(body []byte) (WebhookEvent, error)
	// CreateIssue creates a new external issue from a native ticket, returning its
	// assigned external id + URL. Used by US4 outbound "new linked ticket -> issue".
	CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error)
}
