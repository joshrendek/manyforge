//go:build integration

package connectors

// agent_tools_e2e_integration_test.go — US6 T6: end-to-end integration for the agent
// read + gated write round-trip (Spec §10 demo).
//
// Tests:
//   - TestAgentReadThenGatedTransitionE2E: seed a connector-linked ticket; gw.ReadTicketExternal
//     returns the canned issue; gw.EnqueueTransition enqueues the op; dispatchOnce drives the
//     transition through a transitionRecorder; op is status='done' + audited.
//   - TestAgentGatedCommentE2E: gw.EnqueueComment enqueues a comment op; dispatchOnce posts it
//     via the httptest stub; ticket_message.external_id is stamped; a subsequent
//     sync_inbound_external_comment with the SAME external_id is deduped (count stays 1).

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// ---------------------------------------------------------------------------
// Helpers local to the E2E tests
// ---------------------------------------------------------------------------

// e2eConnector is a TicketingConnector used for the transition E2E: FetchIssue returns the
// canned issue, TransitionStatus records the call. All other methods are no-ops.
type e2eConnector struct {
	issue ExternalIssue
	mu    sync.Mutex
	calls []transitionCall // reuse transitionCall from outbound_transition_integration_test.go
}

var _ TicketingConnector = (*e2eConnector)(nil)

func (c *e2eConnector) FetchIssue(_ context.Context, externalID string) (ExternalIssue, error) {
	iss := c.issue
	if iss.ExternalID == "" {
		iss.ExternalID = externalID
	}
	return iss, nil
}

func (c *e2eConnector) TransitionStatus(_ context.Context, externalID, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, transitionCall{ExternalID: externalID, Status: status})
	return nil
}

func (c *e2eConnector) PostComment(_ context.Context, _, _ string) (ExternalComment, error) {
	return ExternalComment{}, nil
}
func (c *e2eConnector) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (c *e2eConnector) VerifyWebhook(_ http.Header, _ []byte) error { return nil }
func (c *e2eConnector) DecodeWebhook(_ []byte) (WebhookEvent, error) {
	return WebhookEvent{}, nil
}
func (c *e2eConnector) CreateIssue(_ context.Context, _ ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}

// newE2ERegistry builds a Registry whose "jira" factory returns the given e2eConnector.
func newE2ERegistry(svc *Service, conn *e2eConnector) *Registry {
	reg := NewRegistry(svc)
	reg.Register("jira", func(_ ResolvedConnector) (TicketingConnector, error) {
		return conn, nil
	})
	return reg
}

// ---------------------------------------------------------------------------
// TestAgentReadThenGatedTransitionE2E
// ---------------------------------------------------------------------------

// TestAgentReadThenGatedTransitionE2E proves the full read+transition round-trip:
//  1. Agent reads the external ticket via gw.ReadTicketExternal (canned issue returned).
//  2. Agent enqueues a transition via gw.EnqueueTransition.
//  3. dispatchOnce drives the op through the e2eConnector.TransitionStatus recorder.
//  4. Op is status='done' and audited with action='connector.outbound.transitioned'.
func TestAgentReadThenGatedTransitionE2E(t *testing.T) {
	ctx, tdb, tenant := startConn(t)

	// Shared sealer: svc.Create seals credential; dispatcher opens it.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, tenant.principalID, tenant.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Seed a connector-linked native ticket with external_id "JIRA-E2E-TR".
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-E2E-TR", "https://acme.atlassian.net/browse/JIRA-E2E-TR", "E2E transition test",
			"open", "normal", "e2e-reporter@example.com", "E2E Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-E2E-TR"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// Build the e2eConnector: FetchIssue returns a canned issue, TransitionStatus records.
	cannedIssue := ExternalIssue{
		ExternalID: "JIRA-E2E-TR",
		Title:      "E2E transition test",
		Status:     "In Progress",
		Priority:   "Normal",
		Comments: []ExternalComment{
			{ExternalID: "c-e2e-1", Author: "Agent", Body: "looking into it"},
		},
	}
	conn := &e2eConnector{issue: cannedIssue}
	reg := newE2ERegistry(svc, conn)

	gw := NewAgentGateway(svc, reg)

	// Step 1: agent reads the external ticket.
	got, err := gw.ReadTicketExternal(ctx, tenant.principalID, tenant.businessID, ticketID)
	if err != nil {
		t.Fatalf("ReadTicketExternal: %v", err)
	}
	if got.ExternalID != "JIRA-E2E-TR" {
		t.Fatalf("ReadTicketExternal: got ExternalID=%q, want JIRA-E2E-TR", got.ExternalID)
	}
	if len(got.Comments) != 1 {
		t.Fatalf("ReadTicketExternal: got %d comments, want 1", len(got.Comments))
	}

	// Step 2: agent enqueues a transition to "Done".
	if err := gw.EnqueueTransition(ctx, tenant.principalID, tenant.businessID, ticketID, "Done"); err != nil {
		t.Fatalf("EnqueueTransition: %v", err)
	}

	// Confirm a pending 'transition' op was enqueued, and capture its id so the audit
	// assertion below pins the exact row the DEFINER wrote (target_id), not a
	// fresh-DB-only connector_id match.
	var opID uuid.UUID
	var opStatusEnq string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id, status FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='transition' AND body='Done'`,
		ticketID).Scan(&opID, &opStatusEnq); err != nil {
		t.Fatalf("fetch transition op: %v", err)
	}
	if opStatusEnq != "pending" {
		t.Fatalf("want 1 pending transition op, got status=%q", opStatusEnq)
	}

	// Step 3: run the dispatcher — should call conn.TransitionStatus("JIRA-E2E-TR","Done").
	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   sealer,
		Registry: reg,
		Logger:   slog.Default(),
		Batch:    10,
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	// Step 4a: stub connector must have received exactly one transition call.
	conn.mu.Lock()
	calls := conn.calls
	conn.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("TransitionStatus called %d times, want 1", len(calls))
	}
	if calls[0].ExternalID != "JIRA-E2E-TR" || calls[0].Status != "Done" {
		t.Fatalf("TransitionStatus args = (%q,%q), want (JIRA-E2E-TR,Done)",
			calls[0].ExternalID, calls[0].Status)
	}

	// Step 4b: op must be status='done'.
	var opStatus string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status FROM connector_outbound_op WHERE id=$1`, opID).Scan(&opStatus); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if opStatus != "done" {
		t.Fatalf("op status = %q, want done", opStatus)
	}

	// Step 4c: audit row written by complete_outbound_transition. Pin the exact row via
	// target_id=opID (target_type='connector_outbound_op'), matching the DEFINER precisely.
	var auditCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry
		   WHERE action='connector.outbound.transitioned' AND target_id=$1`,
		opID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount)
	}
}

// ---------------------------------------------------------------------------
// TestAgentGatedCommentE2E
// ---------------------------------------------------------------------------

// TestAgentGatedCommentE2E proves the full comment round-trip + inbound dedup:
//  1. Seed a connector-linked ticket + outbound message (external_id NULL).
//  2. gw.EnqueueComment enqueues a pending 'comment' op.
//  3. dispatchOnce posts the comment via the httptest stub.
//  4. ticket_message.external_id is stamped by complete_outbound_comment.
//  5. A subsequent sync_inbound_external_comment with the SAME external_id is deduped:
//     the count of messages with that external_id stays exactly 1 (no duplicate row).
func TestAgentGatedCommentE2E(t *testing.T) {
	ctx, tdb, tenant := startConn(t)

	// httptest stub: records POST and returns a new comment id "C-E2E-COMM-1".
	var (
		stubMu    sync.Mutex
		stubPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stubMu.Lock()
		stubPaths = append(stubPaths, r.Method+":"+r.URL.Path)
		stubMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"C-E2E-COMM-1","author":{"displayName":"agent"}}`))
	}))
	defer srv.Close()

	// Shared sealer: svc.Create seals credential; dispatcher opens it.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	// Connector with allow_private_base_url=true (httptest is 127.0.0.1).
	in := jiraInput()
	in.BaseURL = srv.URL
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, tenant.principalID, tenant.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Registry with SSRF-aware HTTP stub (registerStubJira pattern).
	reg := registerStubJira(svc)

	// Seed a connector-linked native ticket with external_id "JIRA-E2E-CM".
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-E2E-CM", srv.URL+"/browse/JIRA-E2E-CM", "E2E comment test",
			"open", "normal", "cm-reporter@example.com", "CM Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-E2E-CM"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// Seed a pending outbound message (external_id NULL); direction='outbound' requires
	// a non-NULL author_principal_id (ticket_message CHECK).
	var msgID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message
			(ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, body_text)
		VALUES ($1,$2,$2,'outbound',$3,'m-e2e-cm-1','please close this ticket')
		RETURNING id`,
		ticketID, tenant.businessID, tenant.principalID).Scan(&msgID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Build the agent gateway (uses the same registry for both read and write).
	gw := NewAgentGateway(svc, reg)

	// Step 2: agent enqueues a comment op anchored to msgID.
	if err := gw.EnqueueComment(ctx, tenant.principalID, tenant.businessID, ticketID, msgID, "please close this ticket"); err != nil {
		t.Fatalf("EnqueueComment: %v", err)
	}

	// Confirm the pending op was enqueued.
	var pendingOps int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='comment' AND status='pending'`,
		ticketID).Scan(&pendingOps); err != nil {
		t.Fatalf("count pending comment ops: %v", err)
	}
	if pendingOps != 1 {
		t.Fatalf("want 1 pending comment op after EnqueueComment, got %d", pendingOps)
	}

	// Step 3: run the dispatcher — should POST to the stub + write-back external_id.
	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   sealer,
		Registry: reg,
		Logger:   slog.Default(),
		Batch:    10,
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	// Step 4a: stub must have received the comment POST.
	stubMu.Lock()
	paths := stubPaths
	stubMu.Unlock()
	if len(paths) == 0 {
		t.Fatalf("stub never received any request; want a comment POST")
	}
	foundPost := false
	for _, p := range paths {
		if p == "POST:/rest/api/3/issue/JIRA-E2E-CM/comment" {
			foundPost = true
			break
		}
	}
	if !foundPost {
		t.Fatalf("stub did not receive POST:/rest/api/3/issue/JIRA-E2E-CM/comment; got: %v", paths)
	}

	// Step 4b: ticket_message.external_id must be stamped (write-back by complete_outbound_comment).
	var extID *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT external_id FROM ticket_message WHERE id=$1`, msgID).Scan(&extID); err != nil {
		t.Fatalf("read message external_id: %v", err)
	}
	if extID == nil || *extID != "C-E2E-COMM-1" {
		t.Fatalf("message external_id = %v, want C-E2E-COMM-1", extID)
	}

	// Step 4b': complete_outbound_comment must have written its audit, closing the e2e loop.
	// The DEFINER (migration 0045) writes action='connector.outbound.commented' with
	// target_type='ticket_message', target_id=p_message_id (the note message id, msgID).
	var commentAuditCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry
		   WHERE action='connector.outbound.commented' AND target_id=$1`,
		msgID).Scan(&commentAuditCount); err != nil {
		t.Fatalf("count comment audit: %v", err)
	}
	if commentAuditCount != 1 {
		t.Fatalf("comment audit rows = %d, want 1", commentAuditCount)
	}

	// Step 4c: op must be status='done'.
	var opStatus string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='comment' LIMIT 1`,
		ticketID).Scan(&opStatus); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if opStatus != "done" {
		t.Fatalf("op status = %q, want done", opStatus)
	}

	// Step 5: inbound dedup — simulate an inbound sync that carries a comment with the same
	// external_id ("C-E2E-COMM-1"). sync_inbound_external_comment dedupes by
	// (connector_id, external_id) and returns NULL on a dup; count of messages with that
	// external_id must remain exactly 1.
	var dedupMsgID pgtype.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT sync_inbound_external_comment($1,$2,$3,$4)`,
			ticketID, connID, "C-E2E-COMM-1", "please close this ticket",
		).Scan(&dedupMsgID)
	}); err != nil {
		t.Fatalf("sync_inbound_external_comment (dedup attempt): %v", err)
	}
	if dedupMsgID.Valid {
		t.Fatalf("dedup: sync_inbound_external_comment returned a non-NULL id, want NULL (duplicate suppressed)")
	}

	// There must still be exactly ONE ticket_message row with external_id="C-E2E-COMM-1".
	var msgCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM ticket_message WHERE connector_id=$1 AND external_id=$2`,
		connID, "C-E2E-COMM-1").Scan(&msgCount); err != nil {
		t.Fatalf("count messages with external_id: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("dedup failed: want 1 message with external_id=C-E2E-COMM-1, got %d", msgCount)
	}
}
