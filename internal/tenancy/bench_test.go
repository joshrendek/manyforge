//go:build integration

package tenancy_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// seedTree builds a near-complete binary tree of n sub-businesses under master
// (so depth ≈ log2(n) ≈ 10 levels at n=1000), with full closure rows, via the
// RLS-exempt superuser. Returns every node id including the master at index 0;
// the founder owns the master, so RLS exposes the whole subtree to them.
func seedTree(ctx context.Context, t *testing.T, tdb *testdb.TestDB, master uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tree: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	nodes := make([]uuid.UUID, n+1)
	nodes[0] = master
	for i := 1; i <= n; i++ {
		id := uuid.New()
		nodes[i] = id
		parent := nodes[(i-1)/2] // binary tree → ~log2(n) depth
		if _, err := tx.Exec(ctx,
			`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,$4,'active',now(),now())`,
			id, parent, master, fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("insert business %d: %v", i, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2)`, id, master); err != nil {
			t.Fatalf("insert self closure %d: %v", i, err)
		}
		// Inherit the parent's ancestor chain at +1 depth (same as InsertChildClosure).
		if _, err := tx.Exec(ctx,
			`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
			 SELECT ancestor_id, $1, depth+1, $2 FROM business_closure WHERE descendant_id = $3`,
			id, master, parent); err != nil {
			t.Fatalf("insert ancestor closure %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tree: %v", err)
	}
	return nodes
}

// p95 runs fn iters times and returns the 95th-percentile wall time.
func p95(iters int, fn func()) time.Duration {
	ds := make([]time.Duration, iters)
	for i := range ds {
		start := time.Now()
		fn()
		ds[i] = time.Since(start)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[int(float64(iters)*0.95)]
}

// TestSC007_ListAndAccessCheckLatency pins SC-007: at ~1,000 businesses across
// ~10 levels with RLS enabled, both listing and an access check stay under the
// 200 ms p95 budget. Runs through the RLS-subject app role, exactly as production.
func TestSC007_ListAndAccessCheckLatency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	founder, master := seedFounder(ctx, t, tdb, "bench-owner@x.test")
	nodes := seedTree(ctx, t, tdb, master, 1000)
	deep := nodes[len(nodes)-1] // a leaf at ~10 levels deep

	// Warm up so the first measured run isn't dominated by plan/connection setup.
	if bs, err := svc.ListBusinesses(ctx, founder); err != nil || len(bs) < 1000 {
		t.Fatalf("warmup list: want >=1000 businesses, got %d (err %v)", len(bs), err)
	}

	const budget = 200 * time.Millisecond
	const iters = 30

	listP95 := p95(iters, func() {
		if _, err := svc.ListBusinesses(ctx, founder); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if listP95 > budget {
		t.Errorf("listing p95 = %v, want < %v (SC-007)", listP95, budget)
	}

	checkP95 := p95(iters, func() {
		if err := tdb.App.WithPrincipal(ctx, founder, func(tx pgx.Tx) error {
			_, err := authz.Resolve(ctx, tx, founder, deep)
			return err
		}); err != nil {
			t.Fatalf("access check: %v", err)
		}
	})
	if checkP95 > budget {
		t.Errorf("access-check p95 = %v, want < %v (SC-007)", checkP95, budget)
	}

	t.Logf("SC-007: list p95=%v, access-check p95=%v (budget %v, 1000 businesses)", listP95, checkP95, budget)
}
