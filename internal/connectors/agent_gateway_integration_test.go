//go:build integration

package connectors

// agent_gateway_integration_test.go — US6 T2 integration tests for AgentGateway and the
// Service enqueue/ref methods.
//
// Tests:
//   - TestAgentGatewayReadTicketExternal: seed a connector-linked ticket; gw.ReadTicketExternal
//     returns the issue; foreign business / unlinked ticket → errs.ErrNotFound.
//   - TestServiceEnqueueOutboundCommentOwnership: EnqueueOutboundComment inserts a 'comment' op
//     (message_id = noteMsgID); unlinked/foreign ticket → ErrNotFound.
//   - TestServiceEnqueueOutboundTransitionOwnership: EnqueueOutboundTransition inserts a
//     'transition' op; foreign → ErrNotFound; duplicate in-flight → no second op.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// fetchRecorder is a TicketingConnector whose FetchIssue returns a canned ExternalIssue.
// All other methods are no-ops satisfying the interface.
type fetchRecorder struct {
	externalID string
	issue      ExternalIssue
}

var _ TicketingConnector = (*fetchRecorder)(nil)

func (f *fetchRecorder) FetchIssue(_ context.Context, externalID string) (ExternalIssue, error) {
	iss := f.issue
	if iss.ExternalID == "" {
		iss.ExternalID = externalID
	}
	return iss, nil
}
func (f *fetchRecorder) PostComment(_ context.Context, _, _ string, _ bool) (ExternalComment, error) {
	return ExternalComment{}, nil
}
func (f *fetchRecorder) TransitionStatus(_ context.Context, _, _ string) error { return nil }
func (f *fetchRecorder) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (f *fetchRecorder) VerifyWebhook(_ http.Header, _ []byte) error  { return nil }
func (f *fetchRecorder) DecodeWebhook(_ []byte) (WebhookEvent, error) { return WebhookEvent{}, nil }
func (f *fetchRecorder) CreateIssue(_ context.Context, _ ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}

// newFetchRegistry builds a Registry whose "jira" factory returns a canned fetchRecorder.
func newFetchRegistry(svc *Service, canned ExternalIssue) *Registry {
	reg := NewRegistry(svc)
	reg.Register("jira", func(_ ResolvedConnector) (TicketingConnector, error) {
		return &fetchRecorder{issue: canned}, nil
	})
	return reg
}

func TestAgentGatewayReadTicketExternal(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	svc := newConnService(t, tdb, nil)

	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Seed a connector-linked native ticket with external_id "JIRA-GW".
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-GW", "https://acme.atlassian.net/browse/JIRA-GW", "Gateway test issue",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-GW"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	canned := ExternalIssue{
		ExternalID: "JIRA-GW",
		Title:      "Gateway test issue",
		Status:     "Open",
		Comments: []ExternalComment{
			{ExternalID: "c1", Author: "Alice", Body: "first comment"},
		},
	}
	reg := newFetchRegistry(svc, canned)
	gw := NewAgentGateway(svc, reg)

	// Happy path: returns the canned issue.
	got, err := gw.ReadTicketExternal(ctx, seed.principalID, seed.businessID, ticketID)
	if err != nil {
		t.Fatalf("ReadTicketExternal: %v", err)
	}
	if got.ExternalID != canned.ExternalID {
		t.Fatalf("got ExternalID=%q, want %q", got.ExternalID, canned.ExternalID)
	}
	if len(got.Comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(got.Comments))
	}

	// Foreign business → ErrNotFound (no 403/404 oracle).
	other := seedConnectorTenant(ctx, t, tdb)
	_, err = gw.ReadTicketExternal(ctx, other.principalID, other.businessID, ticketID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign business: want ErrNotFound, got %v", err)
	}

	// Completely unknown ticket id → ErrNotFound.
	_, err = gw.ReadTicketExternal(ctx, seed.principalID, seed.businessID, uuid.New())
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown ticket: want ErrNotFound, got %v", err)
	}

	// Unlinked ticket (connector_id NULL) → ErrNotFound.
	var requesterID, unlinkedTicketID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO requester (id, business_id, tenant_root_id, email, display_name,
		                       first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Unlinked Reporter', now(), now(), now(), now())
		RETURNING id`,
		seed.businessID, "unlinked-"+uuid.NewString()+"@x.test").Scan(&requesterID); err != nil {
		t.Fatalf("seed unlinked requester: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, status, priority,
		                    reply_token, last_message_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Unlinked ticket', 'open', 'normal',
		        $3, now(), now(), now())
		RETURNING id`,
		seed.businessID, requesterID, "unlinked-reply-"+uuid.NewString()).Scan(&unlinkedTicketID); err != nil {
		t.Fatalf("seed unlinked ticket: %v", err)
	}
	_, err = gw.ReadTicketExternal(ctx, seed.principalID, seed.businessID, unlinkedTicketID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unlinked ticket: want ErrNotFound, got %v", err)
	}
}

func TestServiceEnqueueOutboundCommentOwnership(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Seed a connector-linked native ticket.
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-COMM", "https://acme.atlassian.net/browse/JIRA-COMM", "Comment test",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-COMM"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// Seed an outbound message (the comment op needs a message_id anchor).
	// message_id column in ticket_message is a text dedup key (e.g. 'm-out-1'); the row's
	// PK (id) is the UUID we pass as MessageID to EnqueueOutboundComment.
	var noteMsgID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message
			(ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, body_text)
		VALUES ($1,$2,$2,'outbound',$3,'m-comm-1','test comment body')
		RETURNING id`,
		ticketID, seed.businessID, seed.principalID).Scan(&noteMsgID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Happy path: inserts a pending 'comment' op with the given message_id.
	if err := svc.EnqueueOutboundComment(ctx, seed.principalID, seed.businessID, ticketID, noteMsgID, "test comment body"); err != nil {
		t.Fatalf("EnqueueOutboundComment: %v", err)
	}

	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='comment' AND status='pending' AND body='test comment body'`,
		ticketID).Scan(&n); err != nil {
		t.Fatalf("count comment ops: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 pending comment op, got %d", n)
	}

	// Foreign business → ErrNotFound.
	other := seedConnectorTenant(ctx, t, tdb)
	err = svc.EnqueueOutboundComment(ctx, other.principalID, other.businessID, ticketID, uuid.New(), "hi")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign business: want ErrNotFound, got %v", err)
	}

	// Unknown ticket → ErrNotFound.
	err = svc.EnqueueOutboundComment(ctx, seed.principalID, seed.businessID, uuid.New(), uuid.New(), "hi")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown ticket: want ErrNotFound, got %v", err)
	}
}

func TestServiceEnqueueOutboundTransitionOwnership(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Seed a connector-linked native ticket.
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-TRANS", "https://acme.atlassian.net/browse/JIRA-TRANS", "Transition test",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-TRANS"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// Happy path: inserts a pending 'transition' op.
	if err := svc.EnqueueOutboundTransition(ctx, seed.principalID, seed.businessID, ticketID, "Done"); err != nil {
		t.Fatalf("EnqueueOutboundTransition: %v", err)
	}

	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='transition' AND status='pending' AND body='Done'`,
		ticketID).Scan(&n); err != nil {
		t.Fatalf("count transition ops: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 pending transition op, got %d", n)
	}

	// Duplicate in-flight call → dedup, still only 1 op.
	if err := svc.EnqueueOutboundTransition(ctx, seed.principalID, seed.businessID, ticketID, "Done"); err != nil {
		t.Fatalf("duplicate EnqueueOutboundTransition: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='transition' AND status IN ('pending','in_progress') AND body='Done'`,
		ticketID).Scan(&n); err != nil {
		t.Fatalf("count after duplicate: %v", err)
	}
	if n != 1 {
		t.Fatalf("dedup failed: want still 1 pending transition op, got %d", n)
	}

	// Foreign business → ErrNotFound.
	other := seedConnectorTenant(ctx, t, tdb)
	err = svc.EnqueueOutboundTransition(ctx, other.principalID, other.businessID, ticketID, "Done")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign business: want ErrNotFound, got %v", err)
	}

	// Unknown ticket → ErrNotFound.
	err = svc.EnqueueOutboundTransition(ctx, seed.principalID, seed.businessID, uuid.New(), "Done")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown ticket: want ErrNotFound, got %v", err)
	}
}
