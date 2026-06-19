//go:build integration

package connectors

// manyforge-xfj: retry/dismiss for failed connector_outbound_op. A connector goes 'degraded'
// the moment any outbound op is terminally 'failed' (healthState: failedOps>0 → degraded), and
// before this feature there was no way out of the terminal 'failed' state — a single transient
// failure pinned the connector degraded forever. These tests pin the service-layer recovery:
//   - RetryFailedOps: failed → pending (attempts reset, last_error cleared) so the dispatcher
//     re-claims it; health returns to healthy.
//   - DismissFailedOps: failed → dismissed (a terminal, non-degrading state kept for audit);
//     health returns to healthy without re-attempting.
// Both are ownership-scoped (unknown/foreign connector → ErrNotFound, no oracle).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// markOpFailed forces the seeded outbound op into the terminal 'failed' state (attempts maxed,
// last_error stored) via the RLS-exempt Super connection — simulating the dispatcher having
// exhausted its retry cap.
func markOpFailed(t *testing.T, ctx context.Context, tdb *testdb.TestDB, opID uuid.UUID) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE connector_outbound_op SET status='failed', attempts=5, last_error='jira: service unreachable' WHERE id=$1`,
		opID); err != nil {
		t.Fatalf("markOpFailed: %v", err)
	}
}

func opStatus(t *testing.T, ctx context.Context, tdb *testdb.TestDB, opID uuid.UUID) (status string, attempts int, lastErr *string) {
	t.Helper()
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status::text, attempts, last_error FROM connector_outbound_op WHERE id=$1`, opID).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("opStatus: %v", err)
	}
	return status, attempts, lastErr
}

func TestRetryFailedOpsReenqueues(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")
	svc := newConnService(t, tdb, nil)

	markOpFailed(t, ctx, tdb, out.OpID)

	// Precondition: a failed op pins the connector degraded.
	v, err := svc.Get(ctx, seed.principalID, seed.businessID, out.ConnectorID)
	if err != nil {
		t.Fatalf("get before retry: %v", err)
	}
	if v.Health.State != "degraded" || v.Health.FailedOutboundOps != 1 {
		t.Fatalf("precondition: want degraded/failed=1, got %s/failed=%d", v.Health.State, v.Health.FailedOutboundOps)
	}

	n, err := svc.RetryFailedOps(ctx, seed.principalID, seed.businessID, out.ConnectorID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("retry: want 1 op re-enqueued, got %d", n)
	}

	// Op is back to pending with a fresh attempt budget and no stale error.
	status, attempts, lastErr := opStatus(t, ctx, tdb, out.OpID)
	if status != "pending" || attempts != 0 || lastErr != nil {
		t.Fatalf("retry: want pending/0/nil, got %s/%d/%v", status, attempts, lastErr)
	}

	// Health recovered: no failed ops, the re-enqueued op shows as pending queue depth.
	v, err = svc.Get(ctx, seed.principalID, seed.businessID, out.ConnectorID)
	if err != nil {
		t.Fatalf("get after retry: %v", err)
	}
	if v.Health.State != "healthy" || v.Health.FailedOutboundOps != 0 || v.Health.PendingOutboundOps != 1 {
		t.Fatalf("after retry: want healthy/failed=0/pending=1, got %s/failed=%d/pending=%d",
			v.Health.State, v.Health.FailedOutboundOps, v.Health.PendingOutboundOps)
	}
}

func TestDismissFailedOpsMarksDismissed(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")
	svc := newConnService(t, tdb, nil)

	markOpFailed(t, ctx, tdb, out.OpID)

	n, err := svc.DismissFailedOps(ctx, seed.principalID, seed.businessID, out.ConnectorID)
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if n != 1 {
		t.Fatalf("dismiss: want 1 op dismissed, got %d", n)
	}

	// Op preserved (audit trail) but in the terminal, non-degrading 'dismissed' state.
	status, _, _ := opStatus(t, ctx, tdb, out.OpID)
	if status != "dismissed" {
		t.Fatalf("dismiss: want status 'dismissed', got %q", status)
	}

	// Health recovered: dismissed ops are neither failed nor pending.
	v, err := svc.Get(ctx, seed.principalID, seed.businessID, out.ConnectorID)
	if err != nil {
		t.Fatalf("get after dismiss: %v", err)
	}
	if v.Health.State != "healthy" || v.Health.FailedOutboundOps != 0 || v.Health.PendingOutboundOps != 0 {
		t.Fatalf("after dismiss: want healthy/failed=0/pending=0, got %s/failed=%d/pending=%d",
			v.Health.State, v.Health.FailedOutboundOps, v.Health.PendingOutboundOps)
	}
}

func TestRetryDismissUnknownConnectorNotFound(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	if _, err := svc.RetryFailedOps(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("retry unknown: want ErrNotFound, got %v", err)
	}
	if _, err := svc.DismissFailedOps(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("dismiss unknown: want ErrNotFound, got %v", err)
	}
}

// A foreign tenant cannot retry/dismiss another tenant's failed ops: ErrNotFound (no oracle) and
// the op is left untouched (still failed).
func TestRetryDismissCrossTenantIsolation(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")
	svc := newConnService(t, tdb, nil)
	markOpFailed(t, ctx, tdb, out.OpID)

	other := seedOther(t, ctx, tdb)

	if _, err := svc.RetryFailedOps(ctx, other.principalID, other.businessID, out.ConnectorID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant retry: want ErrNotFound, got %v", err)
	}
	if _, err := svc.DismissFailedOps(ctx, other.principalID, other.businessID, out.ConnectorID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant dismiss: want ErrNotFound, got %v", err)
	}
	// The op must remain failed — the foreign caller changed nothing.
	if status, _, _ := opStatus(t, ctx, tdb, out.OpID); status != "failed" {
		t.Fatalf("cross-tenant: op must stay 'failed', got %q", status)
	}
}
