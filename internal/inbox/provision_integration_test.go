//go:build integration

package inbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// provisionKey is a fixed HMAC key for the system-address localpart derivation in
// tests; it must match what the Provisioner is constructed with so the derived
// address is stable across calls (idempotency under replay).
var provisionKey = []byte("test-system-domain-key-0123456789abc")

// seedProvisionBusiness seeds a master business WITHOUT a system inbound address
// via the RLS-exempt Super pool. The Provisioner under test is responsible for
// inserting the system address (production: in response to the business.created
// outbox event). Returns (businessID == tenantRootID for a master).
func seedProvisionBusiness(ctx context.Context, t *testing.T, tdb *testdb.TestDB) uuid.UUID {
	t.Helper()
	master := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'ProvCo','active',now(),now())`, []any{master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{master}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return master
}

// businessCreatedEvent synthesizes the outbox Event the worker would hand a
// subscriber for a freshly-created business (the same payload tenancy enqueues).
func businessCreatedEvent(t *testing.T, businessID, tenantRootID uuid.UUID) events.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"business_id":    businessID,
		"tenant_root_id": tenantRootID,
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("event id: %v", err)
	}
	return events.Event{
		ID:           id,
		TenantRootID: tenantRootID,
		Topic:        events.TopicBusinessCreated,
		Payload:      payload,
	}
}

// newProvisioner builds the Provisioner under test with the RLS-subject App pool —
// the same pool the production outbox worker drains under — and the fixed test key.
func newProvisioner(tdb *testdb.TestDB) *Provisioner {
	return NewProvisioner(tdb.App, ProvisionConfig{
		SystemDomain: systemDomain,
		SystemKey:    provisionKey,
	}, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
}

// systemAddressFor reads back the single system address provisioned for a business
// via the RLS-exempt Super pool (ground truth, bypasses RLS).
func systemAddressFor(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) string {
	t.Helper()
	var addr string
	err := tdb.Super.QueryRow(ctx,
		"SELECT address FROM inbound_address WHERE business_id=$1 AND kind='system'", businessID).Scan(&addr)
	if err != nil {
		t.Fatalf("read system address: %v", err)
	}
	return addr
}

// TestProvisionSystemAddressIdempotent (T030/FR-001) — handling business.created
// provisions EXACTLY ONE system inbound_address for the business, with the expected
// shape (kind='system', email_domain_id NULL, b-…@<domain>). Replaying the SAME
// event (at-least-once delivery) leaves exactly ONE row with the SAME address.
func TestProvisionSystemAddressIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	biz := seedProvisionBusiness(ctx, t, tdb)
	prov := newProvisioner(tdb)
	ev := businessCreatedEvent(t, biz, biz)

	// First delivery provisions the address inside the worker tx (principal-less).
	runHandlerInTx(ctx, t, tdb, prov.Handle, ev)

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM inbound_address WHERE business_id=$1 AND kind='system'", biz); n != 1 {
		t.Fatalf("system address count after first provision = %d, want 1", n)
	}
	addr := systemAddressFor(ctx, t, tdb, biz)
	if addr == "" {
		t.Fatalf("provisioned address is empty")
	}
	// Shape: b-…@<systemDomain>, email_domain_id NULL (CHECK enforces NULL for system).
	wantSuffix := "@" + systemDomain
	if got := addr[len(addr)-len(wantSuffix):]; got != wantSuffix {
		t.Errorf("address %q does not end with %q", addr, wantSuffix)
	}
	if addr[:2] != "b-" {
		t.Errorf("address %q does not start with the b- prefix", addr)
	}
	var domainIsNull bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT email_domain_id IS NULL FROM inbound_address WHERE business_id=$1 AND kind='system'", biz).Scan(&domainIsNull); err != nil {
		t.Fatalf("read email_domain_id: %v", err)
	}
	if !domainIsNull {
		t.Errorf("system address email_domain_id is not NULL")
	}

	// Replay the SAME event — at-least-once delivery means the handler may run again.
	runHandlerInTx(ctx, t, tdb, prov.Handle, ev)

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM inbound_address WHERE business_id=$1 AND kind='system'", biz); n != 1 {
		t.Errorf("system address count after replay = %d, want 1 (idempotent)", n)
	}
	if got := systemAddressFor(ctx, t, tdb, biz); got != addr {
		t.Errorf("address after replay = %q, want stable %q", got, addr)
	}
}

// TestProvisionedAddressRoutesAndIngests (T030/T018/FR-001) — the auto-provisioned
// system address RESOLVES (resolve_inbound_address) and ingesting a message to it
// opens a ticket, end-to-end with zero manual address config.
func TestProvisionedAddressRoutesAndIngests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	biz := seedProvisionBusiness(ctx, t, tdb)
	prov := newProvisioner(tdb)
	runHandlerInTx(ctx, t, tdb, prov.Handle, businessCreatedEvent(t, biz, biz))

	addr := systemAddressFor(ctx, t, tdb, biz)

	// The provisioned system address must resolve to exactly this business.
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		r, rerr := resolveRecipient(ctx, tx, addr)
		if rerr != nil {
			return rerr
		}
		if r.businessID != biz {
			t.Errorf("resolved business = %s, want %s", r.businessID, biz)
		}
		if r.emailDomainID != uuid.Nil {
			t.Errorf("system address resolved with non-nil email_domain_id %s", r.emailDomainID)
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve provisioned address: %v", err)
	}

	// Ingesting a message to the provisioned address opens a ticket (zero config).
	svc := newIngestService(ctx, t, tdb)
	res, err := svc.Ingest(ctx, rawTo(addr, "Edith Clarke <edith@example.com>", "help please", "prov-msg-1@example.com", "", "I need help"))
	if err != nil {
		t.Fatalf("ingest to provisioned address: %v", err)
	}
	if !res.Created {
		t.Errorf("result.Created = false, want true (auto-provisioned address must route to a new ticket)")
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", biz); n != 1 {
		t.Errorf("ticket count = %d, want 1", n)
	}
}

// runHandlerInTx runs an events.Handler inside a real DB transaction on the
// RLS-subject App pool — mirroring how the outbox Worker dispatches a handler on
// its (principal-less) tx — and commits, so the side-effects are observable.
func runHandlerInTx(ctx context.Context, t *testing.T, tdb *testdb.TestDB, h events.Handler, ev events.Event) {
	t.Helper()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return h(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("handler: %v", err)
	}
}
