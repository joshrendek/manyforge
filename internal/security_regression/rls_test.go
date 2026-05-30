//go:build integration

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// Fixed ids for two unrelated tenants.
var (
	a1  = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	a2  = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	p1  = uuid.MustParse("00000000-0000-0000-0000-0000000000f1")
	p2  = uuid.MustParse("00000000-0000-0000-0000-0000000000f2")
	b1  = uuid.MustParse("00000000-0000-0000-0000-0000000000b1") // tenant 1 master
	b1s = uuid.MustParse("00000000-0000-0000-0000-00000000b1f5") // tenant 1 sub
	b2  = uuid.MustParse("00000000-0000-0000-0000-0000000000b2") // tenant 2 master
)

// TestRLSIsolation proves, through the real manyforge_app role + the db
// package's principal context, that RLS is fail-closed, cross-tenant invisible,
// and that the resolver respects it (SC-002/003/009, FR-011).
func TestRLSIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seedTwoTenants(ctx, t, tdb)

	count := func(pid uuid.UUID) int {
		var n int
		if err := tdb.App.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM business").Scan(&n)
		}); err != nil {
			t.Fatalf("count businesses (principal=%s): %v", pid, err)
		}
		return n
	}

	// Fail closed: no principal context ⇒ zero rows.
	if got := count(uuid.Nil); got != 0 {
		t.Errorf("fail-closed: expected 0 visible businesses, got %d", got)
	}
	// p1 sees tenant 1 only (master + sub = 2).
	if got := count(p1); got != 2 {
		t.Errorf("p1: expected 2 visible businesses, got %d", got)
	}
	// p2 sees tenant 2 only (1).
	if got := count(p2); got != 1 {
		t.Errorf("p2: expected 1 visible business, got %d", got)
	}

	// Cross-tenant fetch by id is invisible (no oracle): p1 cannot see b2.
	var sawB2 int
	if err := tdb.App.WithPrincipal(ctx, p1, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM business WHERE id=$1", b2).Scan(&sawB2)
	}); err != nil {
		t.Fatalf("p1 cross-tenant probe: %v", err)
	}
	if sawB2 != 0 {
		t.Errorf("isolation breach: p1 can see tenant 2's business (count=%d)", sawB2)
	}

	// Resolver: p1 is Owner of tenant 1 ⇒ full permission set (incl. owner-only actions).
	if err := tdb.App.WithPrincipal(ctx, p1, func(tx pgx.Tx) error {
		perms, err := authz.Resolve(ctx, tx, p1, b1)
		if err != nil {
			return err
		}
		for _, key := range []string{"business.delete", "ownership.transfer", "members.manage", "roles.manage"} {
			if !perms.Has(key) {
				t.Errorf("p1 (owner) should hold %q", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve p1@b1: %v", err)
	}

	// Resolver respects RLS: p2 has no permissions on tenant 1's business.
	if err := tdb.App.WithPrincipal(ctx, p2, func(tx pgx.Tx) error {
		perms, err := authz.Resolve(ctx, tx, p2, b1)
		if err != nil {
			return err
		}
		if len(perms) != 0 {
			t.Errorf("p2 should have 0 permissions on tenant 1, got %d", len(perms))
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve p2@b1: %v", err)
	}
}

// seedTwoTenants inserts two unrelated tenants as the superuser (RLS-exempt) in a
// single transaction so the deferred last-Owner guard validates at commit.
func seedTwoTenants(ctx context.Context, t *testing.T, tdb *testdb.TestDB) {
	t.Helper()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'A1','active',now(),now(),now()),($3,$4,'A2','active',now(),now(),now())`, []any{a1, "a1@x.test", a2, "a2@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now()),($3,'human',$4,now())`, []any{p1, a1, p2, a2}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Acme','active',now(),now())`, []any{b1}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$2,'Acme-Sub','active',now(),now())`, []any{b1s, b1}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Globex','active',now(),now())`, []any{b2}},
		// self(b1s), self(b1), edge(b1 -> b1s); tenant root = b1
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2),($2,$2,0,$2),($2,$1,1,$2)`, []any{b1s, b1}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{b2}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{p1, b1, ownerRole}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{p2, b2, ownerRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}
