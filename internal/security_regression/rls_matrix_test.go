//go:build integration

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestRLSMatrix sweeps every RLS-protected tenant table against the principal
// contexts that must see nothing: an absent (nil) principal (fail-closed), a
// foreign tenant's Owner (cross-root), and an unknown principal. For each it
// asserts both halves of isolation at the database boundary — reads return zero
// cross-tenant rows, and writes (UPDATE/DELETE) targeting cross-tenant rows affect
// zero rows. This is the DB-level guarantee; T065 covers the same boundary at the
// service layer (uniform 404), so the two are exercised separately. Runs as the
// real, non-bypass manyforge_app role.
func TestRLSMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	t1 := seedEscalationTenant(ctx, t, tdb)
	t2 := seedEscalationTenant(ctx, t, tdb)

	// Enrich tenant 2 so every RLS-protected table holds at least one t2 row.
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO invitation (business_id, tenant_root_id, email, role_id, token_hash, expires_at)
		 VALUES ($1,$1,$2,$3,$4, now() + interval '7 days')`,
		t2.master, "rls-matrix@x.test", t2.memberRole, "tok-"+t2.master.String()); err != nil {
		t.Fatalf("seed t2 invitation: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO audit_entry (business_id, tenant_root_id, action) VALUES ($1,$1,'test.event')`,
		t2.master); err != nil {
		t.Fatalf("seed t2 audit: %v", err)
	}

	// Count of tenant-2-owned rows per RLS-protected table.
	tables := []struct{ name, query string }{
		{"business", "SELECT count(*) FROM business WHERE tenant_root_id=$1"},
		{"business_closure", "SELECT count(*) FROM business_closure WHERE tenant_root_id=$1"},
		{"membership", "SELECT count(*) FROM membership WHERE tenant_root_id=$1"},
		{"invitation", "SELECT count(*) FROM invitation WHERE tenant_root_id=$1"},
		{"audit_entry", "SELECT count(*) FROM audit_entry WHERE tenant_root_id=$1"},
		{"role", "SELECT count(*) FROM role WHERE tenant_root_id=$1"},
		{"role_permission", "SELECT count(*) FROM role_permission rp JOIN role r ON r.id=rp.role_id WHERE r.tenant_root_id=$1"},
	}

	// Sanity: the superuser DOES see t2's rows — otherwise a 0 below would be
	// vacuous rather than proof that RLS hid the rows.
	for _, tb := range tables {
		var n int
		if err := tdb.Super.QueryRow(ctx, tb.query, t2.master).Scan(&n); err != nil {
			t.Fatalf("super count %s: %v", tb.name, err)
		}
		if n == 0 {
			t.Fatalf("seed gap: superuser sees 0 %s rows for t2 (assertion would be vacuous)", tb.name)
		}
	}

	// READ isolation: no non-authorized context sees any of t2's rows.
	viewers := []struct {
		name string
		pid  uuid.UUID
	}{
		{"absent (nil principal)", uuid.Nil},
		{"foreign tenant owner", t1.owner},
		{"unknown principal", uuid.New()},
	}
	for _, v := range viewers {
		for _, tb := range tables {
			var n int
			if err := tdb.App.WithPrincipal(ctx, v.pid, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, tb.query, t2.master).Scan(&n)
			}); err != nil {
				t.Fatalf("%s reading %s: %v", v.name, tb.name, err)
			}
			if n != 0 {
				t.Errorf("RLS read breach: %q sees %d %s row(s) of tenant 2", v.name, n, tb.name)
			}
		}
	}

	// WRITE isolation: a foreign Owner's UPDATE/DELETE on t2 rows affects nothing
	// (the USING predicate filters the rows out before the write applies).
	writes := []struct{ name, sql string }{
		{"update t2 business", "UPDATE business SET name='pwned' WHERE tenant_root_id=$1"},
		{"delete t2 membership", "DELETE FROM membership WHERE tenant_root_id=$1"},
		{"update t2 invitation", "UPDATE invitation SET status='revoked' WHERE tenant_root_id=$1"},
	}
	for _, wr := range writes {
		if err := tdb.App.WithPrincipal(ctx, t1.owner, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, wr.sql, t2.master)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("RLS write breach: foreign owner %q affected %d row(s)", wr.name, tag.RowsAffected())
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", wr.name, err)
		}
	}

	// Ground truth: t2's data survives every foreign write attempt.
	if n := membershipCountSuper(ctx, t, tdb, t2.member, t2.master); n != 1 {
		t.Errorf("t2 membership should be intact, count=%d", n)
	}
}
