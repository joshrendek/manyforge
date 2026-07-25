//go:build integration

package timeseries_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
)

// start brings up a migrated Postgres for one test. Each test gets its own container so partition
// DDL and the rollup watermark cannot leak between tests.
func start(t *testing.T) (context.Context, *testdb.TestDB) {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start test db: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb
}

func todayPartition(table string) string {
	return fmt.Sprintf("%s_%s", table, time.Now().UTC().Format("20060102"))
}

// ---------------------------------------------------------------------------
// Partition lifecycle
// ---------------------------------------------------------------------------

func TestPartitionMaintenance_CreatesAheadAndIsIdempotent(t *testing.T) {
	ctx, tdb := start(t)
	w := &timeseries.MaintenanceWorker{DB: tdb.App}

	created, _, err := w.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// analytics_event is daily with precreate_ahead=3 (today + 3), crash_event is monthly with
	// the same depth ⇒ 4 + 4.
	if created != 8 {
		t.Fatalf("expected 8 partitions created, got %d", created)
	}

	again, _, err := w.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("sweep is not idempotent: created %d more on rerun", again)
	}
}

// The parent-only grant is the invariant that keeps a freshly created partition from becoming an
// RLS hole: with no privilege on the child there is simply no reachable path that skips the
// parent's tenant policy. This test is the proof, not the docs comment.
func TestPartitionChild_IsNotDirectlyReachableByAppRole(t *testing.T) {
	ctx, tdb := start(t)
	if _, _, err := (&timeseries.MaintenanceWorker{DB: tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	child := todayPartition("analytics_event")

	// Through the parent: allowed (RLS may filter rows to zero, but access is permitted).
	err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, "SELECT count(*) FROM analytics_event").Scan(&n)
	})
	if err != nil {
		t.Fatalf("parent select should be permitted: %v", err)
	}

	// Directly at the child: denied.
	err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", child)).Scan(&n)
	})
	if err == nil {
		t.Fatalf("SECURITY REGRESSION: %s is directly selectable by manyforge_app; "+
			"a GRANT must have been added on a partition child", child)
	}
}

func TestDropExpiredPartitions_DropsOnlyExpired(t *testing.T) {
	ctx, tdb := start(t)
	w := &timeseries.MaintenanceWorker{DB: tdb.App}
	if _, _, err := w.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Hand-create a partition well past analytics_event's 90-day retention.
	old := time.Now().UTC().AddDate(0, 0, -200)
	oldName := fmt.Sprintf("analytics_event_%s", old.Format("20060102"))
	if _, err := tdb.Super.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF analytics_event FOR VALUES FROM ('%s') TO ('%s')`,
		oldName, old.Format("2006-01-02"), old.AddDate(0, 0, 1).Format("2006-01-02"),
	)); err != nil {
		t.Fatalf("create old partition: %v", err)
	}

	_, dropped, err := w.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep with expired partition: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("expected exactly 1 partition dropped, got %d", dropped)
	}

	var exists bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT to_regclass($1) IS NOT NULL", oldName).Scan(&exists); err != nil {
		t.Fatalf("check dropped: %v", err)
	}
	if exists {
		t.Fatalf("%s should have been dropped", oldName)
	}
	// Today's partition must survive.
	if err := tdb.Super.QueryRow(ctx,
		"SELECT to_regclass($1) IS NOT NULL", todayPartition("analytics_event")).Scan(&exists); err != nil {
		t.Fatalf("check current: %v", err)
	}
	if !exists {
		t.Fatal("current partition was dropped; retention is over-eager")
	}
}

// ---------------------------------------------------------------------------
// Rollup
// ---------------------------------------------------------------------------

type seed struct{ tenant, business, client uuid.UUID }

func seedEvents(t *testing.T, ctx context.Context, tdb *testdb.TestDB, s seed, occurredAt time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := tdb.Super.Exec(ctx,
			`INSERT INTO analytics_event (tenant_root_id, business_id, client_id, occurred_at, name)
			 VALUES ($1,$2,$3,$4,'pageview')`,
			s.tenant, s.business, s.client, occurredAt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

func dailyCount(t *testing.T, ctx context.Context, tdb *testdb.TestDB, s seed) int64 {
	t.Helper()
	var c int64
	if err := tdb.Super.QueryRow(ctx,
		`SELECT coalesce(sum(event_count),0) FROM analytics_event_daily
		 WHERE tenant_root_id=$1 AND client_id=$2`, s.tenant, s.client).Scan(&c); err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	return c
}

func rewindWatermark(t *testing.T, ctx context.Context, tdb *testdb.TestDB) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at='-infinity' WHERE rollup_name='analytics_daily'`,
	); err != nil {
		t.Fatalf("rewind watermark: %v", err)
	}
}

// The load-bearing test. Worker execution is at-least-once, so if the rollup ever incremented
// instead of recomputing, a retried window would double-count — silently, and only under load.
func TestRollupAnalyticsDaily_IsIdempotent(t *testing.T) {
	ctx, tdb := start(t)
	if _, _, err := (&timeseries.MaintenanceWorker{DB: tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("partition sweep: %v", err)
	}
	s := seed{uuid.New(), uuid.New(), uuid.New()}
	seedEvents(t, ctx, tdb, s, time.Now().UTC(), 25)

	w := &timeseries.RollupWorker{DB: tdb.App, Lag: -1} // -1 ⇒ clamped to zero lag
	if _, err := w.SweepOnce(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	first := dailyCount(t, ctx, tdb, s)
	if first != 25 {
		t.Fatalf("expected 25 events rolled up, got %d", first)
	}

	rewindWatermark(t, ctx, tdb)
	if _, err := w.SweepOnce(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second := dailyCount(t, ctx, tdb, s); second != first {
		t.Fatalf("rollup is not idempotent: %d then %d — recompute was replaced by increment?",
			first, second)
	}
}

// A late event lands in a bucket the rollup already closed. Recomputation must fold it in.
func TestRollupAnalyticsDaily_LateArrivalRecomputesClosedBucket(t *testing.T) {
	ctx, tdb := start(t)
	if _, _, err := (&timeseries.MaintenanceWorker{DB: tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("partition sweep: %v", err)
	}
	s := seed{uuid.New(), uuid.New(), uuid.New()}
	occurred := time.Now().UTC().Add(-2 * time.Hour)
	seedEvents(t, ctx, tdb, s, occurred, 10)

	w := &timeseries.RollupWorker{DB: tdb.App, Lag: -1}
	if _, err := w.SweepOnce(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if got := dailyCount(t, ctx, tdb, s); got != 10 {
		t.Fatalf("expected 10, got %d", got)
	}

	// Arrives now, but belongs to the same (already rolled) occurred_at day.
	seedEvents(t, ctx, tdb, s, occurred, 3)
	if _, err := w.SweepOnce(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := dailyCount(t, ctx, tdb, s); got != 13 {
		t.Fatalf("late arrival not folded into closed bucket: expected 13, got %d", got)
	}
}

// Two replicas sweeping at once must not error or double-write; the advisory lock serialises them.
func TestRollup_ConcurrentSweepsAreSafe(t *testing.T) {
	ctx, tdb := start(t)
	if _, _, err := (&timeseries.MaintenanceWorker{DB: tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("partition sweep: %v", err)
	}
	s := seed{uuid.New(), uuid.New(), uuid.New()}
	seedEvents(t, ctx, tdb, s, time.Now().UTC(), 12)

	w := &timeseries.RollupWorker{DB: tdb.App, Lag: -1}
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := w.SweepOnce(ctx); errCh <- err }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent sweep %d: %v", i, err)
		}
	}
	if got := dailyCount(t, ctx, tdb, s); got != 12 {
		t.Fatalf("concurrent sweeps corrupted the bucket: expected 12, got %d", got)
	}
}

// Concurrent partition maintenance from two replicas must not race on DDL — and must still do the
// work. Asserting only "no error" would pass if both sweeps silently created nothing.
func TestPartitionMaintenance_ConcurrentSweepsAreSafe(t *testing.T) {
	ctx, tdb := start(t)
	w := &timeseries.MaintenanceWorker{DB: tdb.App}

	type res struct {
		created int
		err     error
	}
	resCh := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() { c, _, err := w.SweepOnce(ctx); resCh <- res{c, err} }()
	}
	total := 0
	for i := 0; i < 2; i++ {
		r := <-resCh
		if r.err != nil {
			t.Fatalf("concurrent maintenance sweep %d: %v", i, r.err)
		}
		total += r.created
	}
	// Exactly one sweep does the work; the other finds everything present. Between them the full
	// set is created exactly once — never twice, never zero times.
	if total != 8 {
		t.Fatalf("concurrent sweeps created %d partitions in total, want exactly 8", total)
	}
	// And the partitions genuinely exist.
	for _, name := range []string{
		todayPartition("analytics_event"),
		fmt.Sprintf("crash_event_%s", time.Now().UTC().Format("200601")),
	} {
		var exists bool
		if err := tdb.Super.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		if !exists {
			t.Fatalf("%s was not created by the concurrent sweeps", name)
		}
	}
}
