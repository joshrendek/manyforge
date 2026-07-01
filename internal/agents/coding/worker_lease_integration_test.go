//go:build integration

package coding

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestCodeReviewLeaseRenewalPreventsReclaim is the load-bearing test for the
// long-running-review fix (manyforge-206). It drives the real CodeReviewWorker over a
// blocking runJob seam and asserts the heartbeat (1) advances lease_expires_at while
// the job is in flight, (2) persists a non-null progress snapshot, and (3) keeps a
// concurrent ClaimCodeReviews from re-claiming the still-running row — the exact
// double-claim the lease renewal exists to prevent. Then it releases the job and
// asserts it finalizes to succeeded.
func TestCodeReviewLeaseRenewalPreventsReclaim(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic",
	}}
	svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A runJob seam that signals start, blocks until released, then finalizes the row
	// (mimicking the real runJob's success path) so the post-release assertion holds.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	seam := func(ctx context.Context, job ClaimedReview, prog *Progress) error {
		prog.SetPhase("reviewing")
		prog.UpdateStream(3, "partial streamed review output")
		once.Do(func() { close(started) })
		<-release
		_, _ = tdb.Super.Exec(ctx,
			`UPDATE code_review SET status='succeeded', lease_expires_at=NULL, updated_at=now() WHERE id=$1`, job.ID)
		return nil
	}

	w := &CodeReviewWorker{
		DB:                &AppDBAdapter{DB: tdb.App},
		Logger:            slog.Default(),
		Poll:              10 * time.Millisecond,
		LeaseSeconds:      2, // short so renewal is observable
		HeartbeatInterval: 100 * time.Millisecond,
		MaxAttempts:       3,
		Batch:             2,
	}
	w.runJobSeam = seam
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel() // safety net if an assertion t.Fatal's before the explicit stop below
	done := make(chan struct{})
	go func() { w.Run(wctx); close(done) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runJob seam never started — worker did not claim the row")
	}

	// (1) The heartbeat advances the lease.
	lease1 := readLeaseExpires(ctx, t, tdb, cr.ID)
	time.Sleep(600 * time.Millisecond) // ~6 heartbeats at 100ms
	lease2 := readLeaseExpires(ctx, t, tdb, cr.ID)
	if !lease2.After(lease1) {
		t.Fatalf("lease not renewed: lease1=%v lease2=%v — heartbeat is not advancing lease_expires_at", lease1, lease2)
	}

	// (2) Progress is persisted mid-run.
	if !progressNonNull(ctx, t, tdb, cr.ID) {
		t.Fatal("progress is NULL mid-run — heartbeat did not persist the snapshot")
	}

	// (3) THE FIX: a concurrent claim must NOT re-claim the running row (fresh lease).
	// lease arg irrelevant here — asserting the row is NOT reclaimed
	again, err := (&AppDBAdapter{DB: tdb.App}).ClaimCodeReviews(ctx, 900, 10)
	if err != nil {
		t.Fatalf("concurrent claim: %v", err)
	}
	for _, r := range again {
		if r.ID == cr.ID {
			t.Fatal("running row with a fresh lease was re-claimed — lease renewal failed to prevent the double-claim")
		}
	}

	// Release; the row finalizes and stays succeeded (post-terminal renew no-ops).
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if readStatus(ctx, t, tdb, cr.ID) == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row did not reach succeeded after release; status=%s", readStatus(ctx, t, tdb, cr.ID))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Stop the worker and join its goroutine so teardown (closing the testcontainers
	// Postgres pool) never races a query still in flight on wctx.
	wcancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("worker did not stop within 2s of context cancel")
	}
}

func readLeaseExpires(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) time.Time {
	t.Helper()
	var ts pgtype.Timestamptz
	if err := tdb.Super.QueryRow(ctx, `SELECT lease_expires_at FROM code_review WHERE id=$1`, id).Scan(&ts); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

func progressNonNull(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) bool {
	t.Helper()
	var nonNull bool
	if err := tdb.Super.QueryRow(ctx, `SELECT progress IS NOT NULL FROM code_review WHERE id=$1`, id).Scan(&nonNull); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return nonNull
}
