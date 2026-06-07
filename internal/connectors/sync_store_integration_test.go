//go:build integration

package connectors

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func recordDelivery(t *testing.T, ctx context.Context, tdb *testdb.TestDB, svc *Service, principalID, businessID, connectorID uuid.UUID, deliveryID string) bool {
	t.Helper()
	var fresh bool
	if err := tdb.App.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		ok, e := svc.RecordWebhookDelivery(ctx, tx, businessID, connectorID, deliveryID)
		fresh = ok
		return e
	}); err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	return fresh
}

func TestRecordWebhookDeliveryIdempotent(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	if !recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-1") {
		t.Fatalf("first delivery should be newly recorded")
	}
	if recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-1") {
		t.Fatalf("replayed delivery-1 should be a duplicate (false)")
	}
	if !recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-2") {
		t.Fatalf("new delivery-2 should be recorded")
	}
}

func TestRecordWebhookDeliveryCrossBusiness(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Tenant B cannot record a delivery against tenant A's connector (EXISTS guard +
	// RLS → zero rows → false, no row written).
	if recordDelivery(t, ctx, tdb, svc, b.principalID, b.businessID, connID, "x") {
		t.Fatalf("cross-business record must not succeed")
	}
	// Defence-in-depth: confirm nothing was persisted, not just that the call returned false.
	var count int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM connector_webhook_delivery WHERE connector_id = $1 AND external_delivery_id = 'x'",
		connID,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("cross-business delivery must not be persisted (got %d rows)", count)
	}
}
