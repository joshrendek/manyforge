//go:build integration

// Finding MF-002-INGEST-SCOPE behavioral matrix (FR-017): ingestion is scoped to
// the single business the recipient resolves to. Seed two businesses, each with
// its own system address; ingest to business-1's address; business-2 must have
// ZERO tickets / requesters / messages — the controlled DEFINER path cannot widen
// beyond the one resolved business.

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestIngestionScopedToSingleBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	b1 := seedSupportTenant(ctx, t, tdb)
	b2 := seedSupportTenant(ctx, t, tdb)
	svc := newSupportIngest(ctx, t, tdb)

	if _, err := svc.Ingest(ctx, supportRaw(b1.address, "Ada <ada@example.com>", "scoped", "scope-1@example.com", "", "body")); err != nil {
		t.Fatalf("ingest to b1: %v", err)
	}

	// b1 received exactly the one message; b2 is entirely untouched.
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", b1.business); n != 1 {
		t.Errorf("b1 ticket count = %d, want 1", n)
	}
	for _, tbl := range []string{"ticket", "requester", "ticket_message"} {
		if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM "+tbl+" WHERE business_id=$1", b2.business); n != 0 {
			t.Errorf("b2 %s count = %d, want 0 (ingestion must not widen beyond the resolved business)", tbl, n)
		}
	}
}
