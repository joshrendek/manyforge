//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// seedFounder creates a verified account + human principal + master business with
// the principal as Owner (via the RLS-exempt superuser pool). Returns (principal, master).
func seedFounder(ctx context.Context, t *testing.T, tdb *testdb.TestDB, email string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	account := uuid.New()
	principal := uuid.New()
	master := uuid.New()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'F','active',now(),now(),now())`, []any{account, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{principal, account}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Root','active',now(),now())`, []any{master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{master}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{principal, master, ownerRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return principal, master
}

func ancestorIDs(ctx context.Context, t *testing.T, tdb *testdb.TestDB, node uuid.UUID) map[string]int32 {
	t.Helper()
	rows, err := tdb.Super.Query(ctx, "SELECT ancestor_id, depth FROM business_closure WHERE descendant_id=$1", node)
	if err != nil {
		t.Fatalf("ancestors: %v", err)
	}
	defer rows.Close()
	out := map[string]int32{}
	for rows.Next() {
		var a uuid.UUID
		var d int32
		_ = rows.Scan(&a, &d)
		out[a.String()] = d
	}
	return out
}

func TestHierarchy_Lifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	p, master := seedFounder(ctx, t, tdb, "founder@x.test")
	svc := &tenancy.Service{DB: tdb.App}

	// nest: master -> s1 -> g1
	s1, err := svc.CreateSubBusiness(ctx, p, master, "Sub 1")
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	g1, err := svc.CreateSubBusiness(ctx, p, s1.ID, "Grandchild")
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	if bs, _ := svc.ListBusinesses(ctx, p); len(bs) != 3 {
		t.Errorf("expected 3 businesses, got %d", len(bs))
	}
	anc := ancestorIDs(ctx, t, tdb, g1.ID)
	if anc[g1.ID.String()] != 0 || anc[s1.ID.String()] != 1 || anc[master.String()] != 2 {
		t.Errorf("grandchild closure wrong: %v", anc)
	}

	// move g1 directly under master
	if err := svc.Move(ctx, p, g1.ID, master); err != nil {
		t.Fatalf("move: %v", err)
	}
	anc = ancestorIDs(ctx, t, tdb, g1.ID)
	if len(anc) != 2 || anc[master.String()] != 1 || anc[g1.ID.String()] != 0 {
		t.Errorf("after move, closure wrong: %v", anc)
	}

	// archive + restore s1
	if err := svc.Archive(ctx, p, s1.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var status string
	_ = tdb.Super.QueryRow(ctx, "SELECT status FROM business WHERE id=$1", s1.ID).Scan(&status)
	if status != "archived" {
		t.Errorf("s1 should be archived, got %s", status)
	}
	if err := svc.Restore(ctx, p, s1.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// rename
	if err := svc.RenameBusiness(ctx, p, s1.ID, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// delete leaf ok; delete node with a child conflicts
	if err := svc.Delete(ctx, p, g1.ID); err != nil {
		t.Fatalf("delete leaf: %v", err)
	}
	if _, e := svc.CreateSubBusiness(ctx, p, s1.ID, "child"); e != nil {
		t.Fatalf("re-create child: %v", e)
	}
	if err := svc.Delete(ctx, p, s1.ID); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("delete with children: want ErrConflict, got %v", err)
	}
}

func TestHierarchy_Guards(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	p1, master1 := seedFounder(ctx, t, tdb, "a@x.test")
	p2, master2 := seedFounder(ctx, t, tdb, "b@x.test")
	svc := &tenancy.Service{DB: tdb.App}

	a, _ := svc.CreateSubBusiness(ctx, p1, master1, "A")
	b, _ := svc.CreateSubBusiness(ctx, p1, a.ID, "B")

	// cycle: move A under its own descendant B
	if err := svc.Move(ctx, p1, a.ID, b.ID); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("cycle move: want ErrConflict, got %v", err)
	}
	// cannot move a master
	if err := svc.Move(ctx, p1, master1, a.ID); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("master move: want ErrConflict, got %v", err)
	}
	// cross-tenant: p1 moving its node under p2's master → that master is invisible → not found
	if err := svc.Move(ctx, p1, b.ID, master2); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-tenant move: want ErrNotFound, got %v", err)
	}
	// p2 cannot see p1's businesses either
	if _, err := svc.CreateSubBusiness(ctx, p2, master1, "X"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-tenant create: want ErrNotFound, got %v", err)
	}
}

func TestHierarchy_ConcurrentCreates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	p, master := seedFounder(ctx, t, tdb, "c@x.test")
	svc := &tenancy.Service{DB: tdb.App}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.CreateSubBusiness(ctx, p, master, "concurrent")
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("concurrent create %d failed: %v", i, e)
		}
	}
	// All n children created, each a direct child of master (depth 1), tree consistent.
	var count int
	_ = tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM business_closure WHERE ancestor_id=$1 AND depth=1", master).Scan(&count)
	if count != n {
		t.Errorf("expected %d direct children of master, got %d", n, count)
	}
	// No orphans: every non-master business in the tenant is reachable from master.
	var unreachable int
	_ = tdb.Super.QueryRow(ctx, `
		SELECT count(*) FROM business b
		WHERE b.tenant_root_id=$1 AND b.id<>$1
		  AND NOT EXISTS (SELECT 1 FROM business_closure c WHERE c.ancestor_id=$1 AND c.descendant_id=b.id)`,
		master).Scan(&unreachable)
	if unreachable != 0 {
		t.Errorf("found %d businesses not reachable from master (orphans)", unreachable)
	}
}
