//go:build integration

// Finding MF-002-THREAD-IDEMPOTENCY behavioral matrix (real Postgres + the inbox
// ingestion Service): replay → zero duplicate (SC-002) and header threading is
// 100% correct / 0% mis-thread (SC-003). The forged-token pin lives (infra-free)
// in threading_idempotency_test.go.

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestReplayZeroDuplicate (MF-002-THREAD-IDEMPOTENCY / SC-002): a Message-ID
// re-delivered into the same tenant produces exactly ONE ticket_message and never
// a second ticket — the re-delivery is an idempotent no-op.
func TestReplayZeroDuplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedSupportTenant(ctx, t, tdb)
	svc := newSupportIngest(ctx, t, tdb)

	msg := supportRaw(ten.address, "Ada <ada@example.com>", "help", "replay-1@example.com", "", "body")
	if _, err := svc.Ingest(ctx, msg); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	res, err := svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if !res.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1 AND message_id='replay-1@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("ticket_message count for replayed id = %d, want exactly 1 (zero dup)", n)
	}
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.business); n != 1 {
		t.Errorf("ticket count after replay = %d, want 1", n)
	}
}

// TestHeaderThreadingNeverMisthreads (MF-002-THREAD-IDEMPOTENCY / SC-003):
//   - message B whose In-Reply-To == A's Message-ID appends to A's ticket;
//   - message C with an UNRELATED In-Reply-To creates its own ticket (never
//     attaches to A).
//
// 100% correct threading, 0% mis-thread.
func TestHeaderThreadingNeverMisthreads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedSupportTenant(ctx, t, tdb)
	svc := newSupportIngest(ctx, t, tdb)

	// A: opens a new ticket.
	resA, err := svc.Ingest(ctx, supportRaw(ten.address, "Ada <ada@example.com>", "original", "thread-A@example.com", "", "first"))
	if err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if !resA.Created {
		t.Errorf("A should create a new ticket")
	}

	// B: In-Reply-To A → must land on A's ticket, NOT a new one.
	resB, err := svc.Ingest(ctx, supportRaw(ten.address, "Ada <ada@example.com>", "Re: original", "thread-B@example.com", "thread-A@example.com", "second"))
	if err != nil {
		t.Fatalf("ingest B: %v", err)
	}
	if resB.TicketID != resA.TicketID {
		t.Errorf("B.TicketID = %s, want A's ticket %s (B must thread onto A)", resB.TicketID, resA.TicketID)
	}
	if resB.Created {
		t.Errorf("B should append to A, not create a new ticket")
	}

	// C: In-Reply-To an UNRELATED, never-seen Message-ID → its own ticket, not A's.
	resC, err := svc.Ingest(ctx, supportRaw(ten.address, "Ada <ada@example.com>", "unrelated", "thread-C@example.com", "does-not-exist@elsewhere.test", "third"))
	if err != nil {
		t.Fatalf("ingest C: %v", err)
	}
	if resC.TicketID == resA.TicketID {
		t.Errorf("C mis-threaded onto A's ticket %s via an unrelated In-Reply-To", resA.TicketID)
	}
	if !resC.Created {
		t.Errorf("C should create its own ticket (unrelated In-Reply-To, no match)")
	}

	// Ground truth: exactly two tickets (A=B share one, C its own).
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.business); n != 2 {
		t.Errorf("ticket count = %d, want 2 (A+B threaded, C separate)", n)
	}
	// A's ticket carries two messages (A and B).
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", resA.TicketID); n != 2 {
		t.Errorf("A's ticket message count = %d, want 2 (A + threaded B)", n)
	}
}
