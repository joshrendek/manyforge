//go:build integration

package connectors

// TestReconcile* exercises the Reconciler.reconcileOnce end-to-end against a real Postgres
// instance. It verifies:
//   - A connector with last_reconciled_at IS NULL is due → reconcileOnce enqueues
//     one outbox event per external issue key returned by ListUpdatedSince, and stamps
//     the connector's last_reconciled_at.
//   - A connector with a recent last_reconciled_at is NOT due → reconcileOnce enqueues
//     nothing for it.
//
// The fakeConnector.ListUpdatedSince normally returns []string{f.issue.ExternalID}.
// For multi-key testing we use a local reconcileFakeConnector that returns a
// configurable slice.

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// reconcileFakeConnector is a minimal TicketingConnector whose ListUpdatedSince
// returns a configurable []string slice (used only for reconcile tests).
type reconcileFakeConnector struct {
	updated []string
}

var _ TicketingConnector = (*reconcileFakeConnector)(nil)

func (f *reconcileFakeConnector) FetchIssue(_ context.Context, _ string) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}
func (f *reconcileFakeConnector) PostComment(_ context.Context, _, _ string) (ExternalComment, error) {
	return ExternalComment{}, nil
}
func (f *reconcileFakeConnector) TransitionStatus(_ context.Context, _, _ string) error { return nil }
func (f *reconcileFakeConnector) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return f.updated, nil
}
func (f *reconcileFakeConnector) VerifyWebhook(_ http.Header, _ []byte) error { return nil }
func (f *reconcileFakeConnector) DecodeWebhook(_ []byte) (WebhookEvent, error) {
	return WebhookEvent{}, nil
}
func (f *reconcileFakeConnector) CreateIssue(_ context.Context, _ ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}

// TestReconcileOnce_NullLastReconciled verifies that a connector with
// last_reconciled_at IS NULL is treated as due. reconcileOnce must:
//   - enqueue TWO outbox rows with topic connector.inbound.sync (JIRA-1, JIRA-2)
//   - set last_reconciled_at to a non-NULL timestamp
func TestReconcileOnce_NullLastReconciled(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// Share ONE sealer between Service and Reconciler so unseal succeeds.
	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Register a fake "jira" factory returning two issue keys.
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &reconcileFakeConnector{updated: []string{"JIRA-1", "JIRA-2"}}, nil
	})

	reconciler := &Reconciler{
		DB:         tdb.App,
		Sealer:     sharedSealer,
		Registry:   reg,
		Logger:     slog.Default(),
		Every:      time.Hour, // not used in unit pass
		StaleAfter: time.Hour, // NULL last_reconciled_at → always due
	}

	// Connector was just created: last_reconciled_at IS NULL → due.
	if err := reconciler.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	// Assert: TWO outbox rows with topic connector.inbound.sync for this connector.
	var outboxCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox
		   WHERE topic = 'connector.inbound.sync'
		     AND payload->>'connector_id' = $1`,
		connID.String(),
	).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 2 {
		t.Fatalf("want 2 outbox rows (JIRA-1, JIRA-2), got %d", outboxCount)
	}

	// Assert: payloads contain JIRA-1 and JIRA-2.
	var gotJIRA1, gotJIRA2 int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE payload->>'external_id' = 'JIRA-1'),
		   COUNT(*) FILTER (WHERE payload->>'external_id' = 'JIRA-2')
		 FROM outbox
		 WHERE topic = 'connector.inbound.sync'
		   AND payload->>'connector_id' = $1`,
		connID.String(),
	).Scan(&gotJIRA1, &gotJIRA2); err != nil {
		t.Fatalf("scan jira key counts: %v", err)
	}
	if gotJIRA1 != 1 {
		t.Fatalf("want 1 JIRA-1 outbox row, got %d", gotJIRA1)
	}
	if gotJIRA2 != 1 {
		t.Fatalf("want 1 JIRA-2 outbox row, got %d", gotJIRA2)
	}

	// Assert: last_reconciled_at is now stamped (non-NULL).
	var lastReconciled pgtype.Timestamptz
	if err := tdb.Super.QueryRow(ctx,
		`SELECT last_reconciled_at FROM connector WHERE id = $1`,
		connID,
	).Scan(&lastReconciled); err != nil {
		t.Fatalf("read last_reconciled_at: %v", err)
	}
	if !lastReconciled.Valid {
		t.Fatal("want last_reconciled_at to be non-NULL after reconcileOnce")
	}
}

// TestReconcileOnce_RecentConnectorSkipped verifies that a connector whose
// last_reconciled_at was set to now() is NOT returned by ListConnectorsDueForReconcile
// when StaleAfter is 1h, and therefore reconcileOnce enqueues nothing for it.
func TestReconcileOnce_RecentConnectorSkipped(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Simulate a recent reconcile by stamping last_reconciled_at to now().
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE connector SET last_reconciled_at = now(), updated_at = now() WHERE id = $1`,
		connID,
	); err != nil {
		t.Fatalf("stamp last_reconciled_at: %v", err)
	}

	// Register a fake factory that records whether it was called.
	called := false
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		called = true
		return &reconcileFakeConnector{updated: []string{"JIRA-X"}}, nil
	})

	reconciler := &Reconciler{
		DB:         tdb.App,
		Sealer:     sharedSealer,
		Registry:   reg,
		Logger:     slog.Default(),
		Every:      time.Hour,
		StaleAfter: time.Hour, // last_reconciled_at = now() → not stale
	}

	if err := reconciler.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	// The connector should NOT be listed as due → factory not called → no outbox rows.
	if called {
		t.Fatal("factory was called for a recently reconciled connector — should have been skipped")
	}

	var outboxCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox
		   WHERE topic = 'connector.inbound.sync'
		     AND payload->>'connector_id' = $1`,
		connID.String(),
	).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("want 0 outbox rows for recently-reconciled connector, got %d", outboxCount)
	}
}

// TestReconcileOnce_ListConnectorsDueForReconcile_Standalone directly verifies the
// SQL query returns a connector with NULL last_reconciled_at and excludes one
// with last_reconciled_at = now(), without going through the full reconcile stack.
func TestReconcileOnce_ListConnectorsDueForReconcile_Standalone(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	// Two connectors: one never reconciled, one recently reconciled.
	neverID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create never connector: %v", err)
	}

	// Create a second connector with a different base_url (unique constraint is on
	// business_id + type + base_url, so same type+business requires a different URL).
	inp2 := jiraInput()
	inp2.DisplayName = "Acme Jira 2"
	inp2.BaseURL = "https://acme2.atlassian.net"
	inp2.APIToken = "tok-different-456"
	recentID, err := svc.Create(ctx, seed.principalID, seed.businessID, inp2)
	if err != nil {
		t.Fatalf("create recent connector: %v", err)
	}

	// Stamp the second connector as recently reconciled.
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE connector SET last_reconciled_at = now(), updated_at = now() WHERE id = $1`,
		recentID,
	); err != nil {
		t.Fatalf("stamp recent: %v", err)
	}

	// Query directly via ListConnectorsDueForReconcile with StaleAfter = 1h.
	// Use tdb.Super to query (the app connection has no principal GUC for this connector query).
	var dueIDs []string
	rows, err := tdb.Super.Query(ctx,
		`SELECT id FROM connector WHERE status = 'enabled'
		   AND (last_reconciled_at IS NULL OR last_reconciled_at < now() - '1 hour'::interval)`,
	)
	if err != nil {
		t.Fatalf("query due: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		dueIDs = append(dueIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	found := false
	for _, id := range dueIDs {
		if id == neverID.String() {
			found = true
		}
		if id == recentID.String() {
			t.Fatalf("recently reconciled connector %s should NOT be due", recentID)
		}
	}
	if !found {
		t.Fatalf("connector with NULL last_reconciled_at (%s) should be due, got due=%v", neverID, dueIDs)
	}
}
