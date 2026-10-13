//go:build integration

package inbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// seedSiblingBusiness adds a child business under ten's master with its own
// system inbound address on inbound.localhost, and returns (business id, address).
func seedSiblingBusiness(ctx context.Context, t *testing.T, tdb *testdb.TestDB, ten ingestTenant, name string) (uuid.UUID, string) {
	t.Helper()
	child := uuid.New()
	addr := fmt.Sprintf("b-%s@%s", child.String()[:8], systemDomain)
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$2,$3,'active',now(),now())`, []any{child, ten.master, name}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2)`, []any{child, ten.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($2,$1,1,$2)`, []any{child, ten.master}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$3,$4,'system',NULL,now(),now())`, []any{uuid.New(), child, ten.master, addr}},
	} {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed sibling: %v\nSQL: %s", err, s.sql)
		}
	}
	return child, addr
}

// TestIngestMessageIDDedupIsPerBusiness (SECREVIEW-M2) — one email delivered to
// two sibling businesses under the same tenant root (or a sender reusing a
// Message-ID across them) must open a ticket in EACH business. Before 0141 the
// idempotency key was (tenant_root_id, message_id), so the second business's
// delivery was reported as a duplicate and silently dropped.
func TestIngestMessageIDDedupIsPerBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	bizA, addrA := seedSiblingBusiness(ctx, t, tdb, ten, "SiblingA")
	bizB, addrB := seedSiblingBusiness(ctx, t, tdb, ten, "SiblingB")
	svc := newIngestService(ctx, t, tdb)

	const mid = "shared-across-siblings@example.com"
	for _, c := range []struct {
		biz  uuid.UUID
		addr string
	}{{bizA, addrA}, {bizB, addrB}} {
		res, err := svc.Ingest(ctx, rawTo(c.addr, "Ada <ada@example.com>", "hello", mid, "", "body"))
		if err != nil {
			t.Fatalf("ingest to %s: %v", c.addr, err)
		}
		if res.Duplicate || !res.Created {
			t.Fatalf("ingest to %s: Duplicate=%v Created=%v, want a new ticket", c.addr, res.Duplicate, res.Created)
		}
		if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", c.biz); n != 1 {
			t.Errorf("business %s ticket count = %d, want 1", c.biz, n)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1 AND message_id=$2", ten.tenantRootID, mid); n != 2 {
		t.Errorf("ticket_message rows for shared Message-ID = %d, want 2 (one per business)", n)
	}

	// Replays stay idempotent WITHIN each business.
	for _, addr := range []string{addrA, addrB} {
		res, err := svc.Ingest(ctx, rawTo(addr, "Ada <ada@example.com>", "hello", mid, "", "body"))
		if err != nil {
			t.Fatalf("replay to %s: %v", addr, err)
		}
		if !res.Duplicate {
			t.Errorf("replay to %s: Duplicate=false, want true", addr)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE tenant_root_id=$1", ten.tenantRootID); n != 2 {
		t.Errorf("tenant ticket count after replays = %d, want 2", n)
	}
}

// callIngestSQL invokes ingest_inbound_message directly inside tx with a fresh
// ticket id / reply token, as the Go service does.
func callIngestSQL(ctx context.Context, tx pgx.Tx, ten ingestTenant, messageID string) (ticketID, msgID uuid.UUID, created, dup bool, err error) {
	newTicket := uuid.New()
	err = tx.QueryRow(ctx, `
		SELECT out_ticket_id, out_message_id, out_created, out_duplicate
		FROM ingest_inbound_message($1, $2, $3, 'ada@example.com', 'Ada', 'race', $4, NULL, '{}',
			'body', NULL, '{}'::jsonb, false, NULL, $5, $6, '[]'::jsonb, 'inbox:test', 0)`,
		ten.master, ten.tenantRootID, ten.address, messageID, newTicket, "tok-"+newTicket.String(),
	).Scan(&ticketID, &msgID, &created, &dup)
	return
}

// TestIngestConcurrentRedeliveryNoOrphanTicket (SECREVIEW-M4) — two transactions
// ingesting the same Message-ID concurrently. The first is held open after its
// insert; the second starts, must block, and once the first commits it must see
// the first's message and report a duplicate WITHOUT having opened a ticket.
// Before 0141 the second transaction saw no committed message, created its own
// ticket, lost the ON CONFLICT race and committed that empty orphan ticket.
func TestIngestConcurrentRedeliveryNoOrphanTicket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	const mid = "concurrent-redelivery@example.com"

	tx1, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	firstTicket, firstMsg, created, dup, err := callIngestSQL(ctx, tx1, ten, mid)
	if err != nil || !created || dup {
		t.Fatalf("first ingest: created=%v dup=%v err=%v", created, dup, err)
	}

	// Second delivery on its own connection; record its backend pid so we can see it block.
	conn2, err := tdb.Super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Release()
	var pid2 int
	if err := conn2.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid2); err != nil {
		t.Fatal(err)
	}

	type result struct {
		ticket, msg  uuid.UUID
		created, dup bool
		err          error
	}
	start := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		<-start
		var r result
		r.err = func() error {
			tx2, err := conn2.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx2.Rollback(ctx) }()
			r.ticket, r.msg, r.created, r.dup, err = callIngestSQL(ctx, tx2, ten, mid)
			if err != nil {
				return err
			}
			return tx2.Commit(ctx)
		}()
		done <- r
	}()
	close(start)

	// Wait until the second transaction is parked on a lock held by the first.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting bool
		if err := tdb.Super.QueryRow(ctx,
			"SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", pid2).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("second ingest finished before the first committed (did not serialize): %+v", r)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("second ingest never blocked on the first transaction")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit first: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("second ingest: %v", r.err)
	}
	if !r.dup || r.created || r.ticket != firstTicket || r.msg != firstMsg {
		t.Errorf("second ingest = %+v, want duplicate of ticket %s / message %s", r, firstTicket, firstMsg)
	}

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("ticket count = %d, want 1 (no orphan ticket from the losing redelivery)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE business_id=$1 AND message_id=$2", ten.master, mid); n != 1 {
		t.Errorf("ticket_message count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM ticket t WHERE t.business_id=$1
		AND NOT EXISTS (SELECT 1 FROM ticket_message m WHERE m.ticket_id = t.id)`, ten.master); n != 0 {
		t.Errorf("empty (message-less) tickets = %d, want 0", n)
	}
}
