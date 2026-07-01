package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSystemDB implements systemDB for unit tests. It returns a scripted claim
// batch from ClaimCodeReviews and records Requeue/Fail calls for assertion.
type fakeSystemDB struct {
	claims []ClaimedReview
	// recorded calls
	requeueCalls []requeueCall
	failCalls    []failCall
	renewCalls   atomic.Int64 // heartbeat renewals (goroutine-concurrent → atomic)
	renewPanics  bool         // when set, RenewLease panics (heartbeat-goroutine crash test)
}

type requeueCall struct {
	id           uuid.UUID
	delaySeconds int
	lastError    string
}

type failCall struct {
	id        uuid.UUID
	lastError string
}

func (f *fakeSystemDB) ClaimCodeReviews(ctx context.Context, leaseSeconds, limit int) ([]ClaimedReview, error) {
	rows := f.claims
	f.claims = nil // drain so the loop doesn't re-claim on the next tick
	return rows, nil
}

func (f *fakeSystemDB) RequeueCodeReview(ctx context.Context, id uuid.UUID, delaySeconds int, lastError string) error {
	f.requeueCalls = append(f.requeueCalls, requeueCall{id: id, delaySeconds: delaySeconds, lastError: lastError})
	return nil
}

func (f *fakeSystemDB) FailCodeReview(ctx context.Context, id uuid.UUID, lastError string) error {
	f.failCalls = append(f.failCalls, failCall{id: id, lastError: lastError})
	return nil
}

func (f *fakeSystemDB) RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error {
	f.renewCalls.Add(1)
	if f.renewPanics {
		panic("boom: simulated RenewLease panic")
	}
	return nil
}

// makeRow builds a ClaimedReview with all UUIDs filled.
func makeRow(attempts int) ClaimedReview {
	return ClaimedReview{
		ID:              uuid.New(),
		BusinessID:      uuid.New(),
		PrincipalID:     uuid.New(),
		AgentID:         uuid.New(),
		RepoConnectorID: uuid.New(),
		PRNumber:        42,
		Attempts:        attempts,
	}
}

// makeWorker returns a CodeReviewWorker with fast polling and the given
// fakeSystemDB. The runJobFn seam overrides actual runJob so no real
// service/DB is needed.
func makeWorker(db *fakeSystemDB, runJobFn func(ctx context.Context, job ClaimedReview, prog *Progress) error) *CodeReviewWorker {
	w := &CodeReviewWorker{
		DB:           db,
		Svc:          nil, // not used when runJobSeam is set
		Logger:       slog.Default(),
		Poll:         5 * time.Millisecond, // fast for tests
		LeaseSeconds: 900,
		MaxAttempts:  3,
		Batch:        2,
	}
	w.runJobSeam = runJobFn
	return w
}

// runOnce drives the worker for exactly one poll tick then cancels.
func runOnce(w *CodeReviewWorker) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)
}

// TestWorkerSuccess verifies that when runJob succeeds, neither Requeue nor
// Fail is called.
func TestWorkerSuccess(t *testing.T) {
	db := &fakeSystemDB{
		claims: []ClaimedReview{makeRow(1)},
	}
	var called atomic.Bool
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		called.Store(true)
		return nil
	})
	runOnce(w)

	if !called.Load() {
		t.Fatal("runJob was not called")
	}
	if len(db.requeueCalls) != 0 {
		t.Fatalf("expected 0 requeue calls, got %d", len(db.requeueCalls))
	}
	if len(db.failCalls) != 0 {
		t.Fatalf("expected 0 fail calls, got %d", len(db.failCalls))
	}
}

// TestWorkerRequeueOnFailureUnderMax verifies that when runJob errors and
// attempts < MaxAttempts, RequeueCodeReview is called (not Fail).
func TestWorkerRequeueOnFailureUnderMax(t *testing.T) {
	row := makeRow(1) // attempts=1 < MaxAttempts=3
	db := &fakeSystemDB{claims: []ClaimedReview{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		return errors.New("transient error")
	})
	runOnce(w)

	if len(db.requeueCalls) != 1 {
		t.Fatalf("expected 1 requeue call, got %d", len(db.requeueCalls))
	}
	if len(db.failCalls) != 0 {
		t.Fatalf("expected 0 fail calls, got %d", len(db.failCalls))
	}
	if db.requeueCalls[0].id != row.ID {
		t.Errorf("requeue called with wrong ID: got %v want %v", db.requeueCalls[0].id, row.ID)
	}
}

// TestWorkerFailOnMaxAttempts verifies that when runJob errors and
// attempts == MaxAttempts, FailCodeReview is called (not Requeue).
func TestWorkerFailOnMaxAttempts(t *testing.T) {
	row := makeRow(3) // attempts=3 == MaxAttempts=3
	db := &fakeSystemDB{claims: []ClaimedReview{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		return errors.New("final failure")
	})
	runOnce(w)

	if len(db.failCalls) != 1 {
		t.Fatalf("expected 1 fail call, got %d", len(db.failCalls))
	}
	if len(db.requeueCalls) != 0 {
		t.Fatalf("expected 0 requeue calls, got %d", len(db.requeueCalls))
	}
	if db.failCalls[0].id != row.ID {
		t.Errorf("fail called with wrong ID: got %v want %v", db.failCalls[0].id, row.ID)
	}
}

// TestWorkerCtxCancelStopsLoop verifies that ctx cancellation stops the loop
// promptly (no hang).
func TestWorkerCtxCancelStopsLoop(t *testing.T) {
	db := &fakeSystemDB{} // no claims
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		return nil
	})
	// Use a very short timeout — the loop must exit within 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("worker did not stop after ctx cancellation")
	}
}

// TestWorkerPanicRecovery verifies that a panicking runJob is recovered and
// treated as a failure (Requeue since attempts < MaxAttempts) rather than
// crashing the worker.
func TestWorkerPanicRecovery(t *testing.T) {
	row := makeRow(1) // attempts=1 < MaxAttempts=3 → requeue expected
	db := &fakeSystemDB{claims: []ClaimedReview{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		panic("simulated panic in runJob")
	})
	// Should not panic — Run must recover and continue.
	runOnce(w)

	if len(db.requeueCalls) != 1 {
		t.Fatalf("expected 1 requeue call after panic, got %d", len(db.requeueCalls))
	}
	if len(db.failCalls) != 0 {
		t.Fatalf("expected 0 fail calls after panic, got %d", len(db.failCalls))
	}
}

// TestWorkerMultipleRowsInBatch verifies that the worker processes all rows
// in a claim batch independently.
func TestWorkerMultipleRowsInBatch(t *testing.T) {
	rows := []ClaimedReview{
		makeRow(1), // will succeed
		makeRow(2), // will fail → requeue (attempts=2 < 3)
		makeRow(3), // will fail → fail  (attempts=3 == MaxAttempts)
	}
	db := &fakeSystemDB{claims: rows}
	callIdx := 0
	errs := []error{nil, fmt.Errorf("err2"), fmt.Errorf("err3")}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		idx := callIdx
		callIdx++
		return errs[idx]
	})
	runOnce(w)

	if len(db.requeueCalls) != 1 {
		t.Fatalf("expected 1 requeue call, got %d", len(db.requeueCalls))
	}
	if len(db.failCalls) != 1 {
		t.Fatalf("expected 1 fail call, got %d", len(db.failCalls))
	}
}

// TestWorkerHeartbeatRenewsLease verifies the worker spawns a heartbeat that calls
// RenewLease while runJob is in flight (the long-running lease-renewal mechanism).
func TestWorkerHeartbeatRenewsLease(t *testing.T) {
	db := &fakeSystemDB{claims: []ClaimedReview{makeRow(1)}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, prog *Progress) error {
		prog.SetPhase("reviewing") // makes Snapshot non-nil (heartbeat persists it)
		time.Sleep(80 * time.Millisecond)
		return nil
	})
	w.HeartbeatInterval = 10 * time.Millisecond // fast for the test (default is 5s)
	runOnce(w)

	if db.renewCalls.Load() == 0 {
		t.Fatal("heartbeat never called RenewLease during a runJob in flight")
	}
}

// TestWorkerHeartbeatPanicDoesNotCrash pins that a panic inside the heartbeat goroutine
// (e.g. RenewLease/Snapshot panicking) is recovered and does NOT crash the process — the
// in-flight job still completes. Without the recover the panic would be fatal (an
// unrecovered goroutine panic takes down the whole server).
func TestWorkerHeartbeatPanicDoesNotCrash(t *testing.T) {
	db := &fakeSystemDB{claims: []ClaimedReview{makeRow(1)}, renewPanics: true}
	var completed atomic.Bool
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, prog *Progress) error {
		prog.SetPhase("reviewing")
		time.Sleep(60 * time.Millisecond) // long enough for a heartbeat tick to fire (and panic)
		completed.Store(true)
		return nil
	})
	w.HeartbeatInterval = 10 * time.Millisecond
	runOnce(w)

	if db.renewCalls.Load() == 0 {
		t.Fatal("heartbeat never fired — the panic path was not exercised")
	}
	if !completed.Load() {
		t.Fatal("runJob did not complete — a heartbeat panic must not abort the in-flight job")
	}
	if len(db.failCalls) != 0 || len(db.requeueCalls) != 0 {
		t.Fatalf("job should have succeeded despite the heartbeat panic; fail=%d requeue=%d",
			len(db.failCalls), len(db.requeueCalls))
	}
}
