//go:build integration

package agents

// manyforge-67i: reaper for orphaned 'running' agent runs. The runner sets a run 'running'
// (agent_run.updated_at = start time) then executes a loop capped at defaultWallClock (120s).
// If the worker goroutine dies (backend restart or crash) the row never transitions to a
// terminal state — it is stuck 'running' forever, which would make any "agent working"
// indicator lie. ReapOnce marks 'running' runs whose updated_at is older than StaleAfter (chosen
// >> the 120s cap, so a genuinely-live run is never reaped) as 'failed'. These tests pin that:
//   - a stale 'running' run is reaped (→ failed) with a reason;
//   - a fresh 'running' run is left alone;
//   - non-running states (queued / awaiting_approval / succeeded) are never touched.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// seedRunWithStatus inserts an agent_run with an exact status + updated_at via the RLS-exempt
// Super pool, so the reaper's staleness window can be exercised deterministically.
func seedRunWithStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, agent Agent, status string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger, status, correlation_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $3, 'manual', $4, $5, $6, $6)`,
		runID, agent.ID, s.businessID, status, "reap-"+runID.String(), updatedAt); err != nil {
		t.Fatalf("seedRunWithStatus(%s): %v", status, err)
	}
	return runID
}

func TestReapStaleAgentRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	agent := seedAccountingAgent(ctx, t, tdb, seed, "Reaper Agent", 0)
	now := time.Now().UTC()

	staleRunning := seedRunWithStatus(ctx, t, tdb, seed, agent, "running", now.Add(-20*time.Minute))
	freshRunning := seedRunWithStatus(ctx, t, tdb, seed, agent, "running", now.Add(-1*time.Minute))
	staleQueued := seedRunWithStatus(ctx, t, tdb, seed, agent, "queued", now.Add(-20*time.Minute))
	staleAwaiting := seedRunWithStatus(ctx, t, tdb, seed, agent, "awaiting_approval", now.Add(-20*time.Minute))
	oldSucceeded := seedRunWithStatus(ctx, t, tdb, seed, agent, "succeeded", now.Add(-20*time.Minute))

	reaper := &Reaper{DB: tdb.App, StaleAfter: 10 * time.Minute}
	n, err := reaper.ReapOnce(ctx)
	if err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapOnce reaped %d, want exactly 1 (the stale running run)", n)
	}

	// Only the stale running run flips to failed, with a non-empty reason recorded.
	if got := superRunStatus(ctx, t, tdb, staleRunning); got != "failed" {
		t.Fatalf("stale running run: status=%q, want failed", got)
	}
	var reason *string
	if err := tdb.Super.QueryRow(ctx, "SELECT error FROM agent_run WHERE id=$1", staleRunning).Scan(&reason); err != nil {
		t.Fatalf("read reaped error: %v", err)
	}
	if reason == nil || *reason == "" {
		t.Fatalf("reaped run must record a failure reason, got %v", reason)
	}

	// Everything else is untouched: a fresh running run, and any non-running state regardless of age.
	for _, c := range []struct {
		id   uuid.UUID
		want string
	}{
		{freshRunning, "running"},
		{staleQueued, "queued"},
		{staleAwaiting, "awaiting_approval"},
		{oldSucceeded, "succeeded"},
	} {
		if got := superRunStatus(ctx, t, tdb, c.id); got != c.want {
			t.Fatalf("run %s: status=%q, want %q (reaper must not touch it)", c.id, got, c.want)
		}
	}
}

// A zero-valued Reaper.StaleAfter must NOT reap-all — it falls back to the safe default window, so
// a fresh running run survives. (The reap-all-on-sight semantics live only in the DEFINER, tested
// separately below.)
func TestReaperZeroStaleAfterUsesSafeDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	agent := seedAccountingAgent(ctx, t, tdb, seed, "Reaper Default", 0)
	fresh := seedRunWithStatus(ctx, t, tdb, seed, agent, "running", time.Now().UTC().Add(-1*time.Second))

	reaper := &Reaper{DB: tdb.App, StaleAfter: 0} // zero → default 10m window, not reap-all
	n, err := reaper.ReapOnce(ctx)
	if err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("zero StaleAfter reaped %d, want 0 (a 1s-old run must survive the default window)", n)
	}
	if got := superRunStatus(ctx, t, tdb, fresh); got != "running" {
		t.Fatalf("fresh run status=%q, want running", got)
	}
}

// The DEFINER itself reaps every running run when handed a 0-second window (the semantics a
// single-instance startup sweep would use) and leaves non-running states alone. This pins the SQL
// boundary the Go Reaper deliberately never exposes.
func TestReapStaleAgentRunsDefinerReapAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	agent := seedAccountingAgent(ctx, t, tdb, seed, "Reaper ReapAll", 0)
	now := time.Now().UTC()
	r1 := seedRunWithStatus(ctx, t, tdb, seed, agent, "running", now.Add(-20*time.Minute))
	r2 := seedRunWithStatus(ctx, t, tdb, seed, agent, "running", now.Add(-1*time.Second))
	q := seedRunWithStatus(ctx, t, tdb, seed, agent, "queued", now)

	var n int64
	if err := tdb.Super.QueryRow(ctx, "SELECT reap_stale_agent_runs(0::double precision)").Scan(&n); err != nil {
		t.Fatalf("reap_stale_agent_runs(0): %v", err)
	}
	if n != 2 {
		t.Fatalf("DEFINER reap-all reaped %d, want 2 (both running)", n)
	}
	if got := superRunStatus(ctx, t, tdb, r1); got != "failed" {
		t.Fatalf("r1 status=%q, want failed", got)
	}
	if got := superRunStatus(ctx, t, tdb, r2); got != "failed" {
		t.Fatalf("r2 status=%q, want failed", got)
	}
	if got := superRunStatus(ctx, t, tdb, q); got != "queued" {
		t.Fatalf("queued run status=%q, want queued (untouched)", got)
	}
}
