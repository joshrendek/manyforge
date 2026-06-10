//go:build integration

package connectors

// outbound_transition_integration_test.go — US6 T2 integration test for the transition
// dispatch path.
//
// TestDispatchTransitionPostsAndCompletes: seeds a pending 'transition' op + a recording fake
// whose TransitionStatus records the call; runs one dispatchOnce tick; asserts the fake saw
// (externalID,"Done"), op → status='done', audit 'connector.outbound.transitioned'.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// transitionRecorder is a TicketingConnector whose TransitionStatus records every
// (externalID, status) call. All other methods are no-ops.
type transitionRecorder struct {
	mu    sync.Mutex
	calls []transitionCall
}

type transitionCall struct {
	ExternalID string
	Status     string
}

var _ TicketingConnector = (*transitionRecorder)(nil)

func (r *transitionRecorder) TransitionStatus(_ context.Context, externalID, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, transitionCall{ExternalID: externalID, Status: status})
	return nil
}

func (r *transitionRecorder) FetchIssue(_ context.Context, _ string) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}
func (r *transitionRecorder) PostComment(_ context.Context, _, _ string) (ExternalComment, error) {
	return ExternalComment{}, nil
}
func (r *transitionRecorder) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (r *transitionRecorder) VerifyWebhook(_ http.Header, _ []byte) error { return nil }
func (r *transitionRecorder) DecodeWebhook(_ []byte) (WebhookEvent, error) {
	return WebhookEvent{}, nil
}
func (r *transitionRecorder) CreateIssue(_ context.Context, _ ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}

// transitionRecorderRegistry builds a Registry whose "jira" factory returns the given recorder.
func transitionRecorderRegistry(svc *Service, sealer *crypto.Sealer, rec *transitionRecorder) *Registry {
	reg := NewRegistry(svc)
	reg.Register("jira", func(_ ResolvedConnector) (TicketingConnector, error) {
		return rec, nil
	})
	return reg
}

func TestDispatchTransitionPostsAndCompletes(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// Build service with a shared sealer so the dispatcher can unseal the credential.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Seed a connector-linked native ticket with external_id "JIRA-TR".
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-TR", "https://acme.atlassian.net/browse/JIRA-TR", "Transition dispatch test",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-TR"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// Seed a pending 'transition' op directly (Super = RLS-exempt).
	var opID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO connector_outbound_op
			(business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
		VALUES ($1,$1,$2,$3,'transition','Done') RETURNING id`,
		seed.businessID, connID, ticketID).Scan(&opID); err != nil {
		t.Fatalf("seed transition op: %v", err)
	}

	rec := &transitionRecorder{}
	reg := transitionRecorderRegistry(svc, sealer, rec)

	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   sealer,
		Registry: reg,
		Batch:    10,
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	// The recorder must have seen exactly one call with (externalID="JIRA-TR", status="Done").
	rec.mu.Lock()
	calls := rec.calls
	rec.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("TransitionStatus called %d times, want 1", len(calls))
	}
	if calls[0].ExternalID != "JIRA-TR" || calls[0].Status != "Done" {
		t.Fatalf("TransitionStatus args = (%q,%q), want (JIRA-TR,Done)", calls[0].ExternalID, calls[0].Status)
	}

	// Op must be marked 'done'.
	var status string
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, opID).Scan(&status); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if status != "done" {
		t.Fatalf("op status = %q, want done", status)
	}

	// Audit row written for the transition.
	var auditCount int
	if err := tdb.Super.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM audit_entry WHERE action='connector.outbound.transitioned' AND target_id='%s'`, opID),
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount)
	}
}
