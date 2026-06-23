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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// fakeSystemDB implements systemDB for unit tests. It returns a scripted claim
// batch from ClaimCodeReviews and records Requeue/Fail calls for assertion.
type fakeSystemDB struct {
	claims []dbgen.ClaimCodeReviewsRow
	// recorded calls
	requeueCalls []dbgen.RequeueCodeReviewParams
	failCalls    []dbgen.FailCodeReviewParams
}

func (f *fakeSystemDB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	// We don't actually use a transaction in the fake — the fn is never called;
	// instead ClaimCodeReviews is called directly. Return nil to signal "ok".
	return nil
}

func (f *fakeSystemDB) ClaimCodeReviews(ctx context.Context, arg dbgen.ClaimCodeReviewsParams) ([]dbgen.ClaimCodeReviewsRow, error) {
	rows := f.claims
	f.claims = nil // drain so the loop doesn't re-claim on the next tick
	return rows, nil
}

func (f *fakeSystemDB) RequeueCodeReview(ctx context.Context, arg dbgen.RequeueCodeReviewParams) error {
	f.requeueCalls = append(f.requeueCalls, arg)
	return nil
}

func (f *fakeSystemDB) FailCodeReview(ctx context.Context, arg dbgen.FailCodeReviewParams) error {
	f.failCalls = append(f.failCalls, arg)
	return nil
}

// makeRow builds a ClaimCodeReviewsRow with all UUIDs filled.
func makeRow(attempts int32) dbgen.ClaimCodeReviewsRow {
	return dbgen.ClaimCodeReviewsRow{
		ID:              uuid.New(),
		BusinessID:      uuid.New(),
		PrincipalID:     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		AgentID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
		RepoConnectorID: uuid.New(),
		PrNumber:        42,
		Attempts:        attempts,
	}
}

// makeWorker returns a CodeReviewWorker with fast polling and the given
// fakeSystemDB. The runJobFn seam overrides actual runJob so no real
// service/DB is needed.
func makeWorker(db *fakeSystemDB, runJobFn func(ctx context.Context, job ClaimedReview) error) *CodeReviewWorker {
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
		claims: []dbgen.ClaimCodeReviewsRow{makeRow(1)},
	}
	var called atomic.Bool
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
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
	db := &fakeSystemDB{claims: []dbgen.ClaimCodeReviewsRow{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
		return errors.New("transient error")
	})
	runOnce(w)

	if len(db.requeueCalls) != 1 {
		t.Fatalf("expected 1 requeue call, got %d", len(db.requeueCalls))
	}
	if len(db.failCalls) != 0 {
		t.Fatalf("expected 0 fail calls, got %d", len(db.failCalls))
	}
	if db.requeueCalls[0].ID != row.ID {
		t.Errorf("requeue called with wrong ID: got %v want %v", db.requeueCalls[0].ID, row.ID)
	}
}

// TestWorkerFailOnMaxAttempts verifies that when runJob errors and
// attempts == MaxAttempts, FailCodeReview is called (not Requeue).
func TestWorkerFailOnMaxAttempts(t *testing.T) {
	row := makeRow(3) // attempts=3 == MaxAttempts=3
	db := &fakeSystemDB{claims: []dbgen.ClaimCodeReviewsRow{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
		return errors.New("final failure")
	})
	runOnce(w)

	if len(db.failCalls) != 1 {
		t.Fatalf("expected 1 fail call, got %d", len(db.failCalls))
	}
	if len(db.requeueCalls) != 0 {
		t.Fatalf("expected 0 requeue calls, got %d", len(db.requeueCalls))
	}
	if db.failCalls[0].ID != row.ID {
		t.Errorf("fail called with wrong ID: got %v want %v", db.failCalls[0].ID, row.ID)
	}
}

// TestWorkerCtxCancelStopsLoop verifies that ctx cancellation stops the loop
// promptly (no hang).
func TestWorkerCtxCancelStopsLoop(t *testing.T) {
	db := &fakeSystemDB{} // no claims
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
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
	db := &fakeSystemDB{claims: []dbgen.ClaimCodeReviewsRow{row}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
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
	rows := []dbgen.ClaimCodeReviewsRow{
		makeRow(1), // will succeed
		makeRow(2), // will fail → requeue (attempts=2 < 3)
		makeRow(3), // will fail → fail  (attempts=3 == MaxAttempts)
	}
	db := &fakeSystemDB{claims: rows}
	callIdx := 0
	errs := []error{nil, fmt.Errorf("err2"), fmt.Errorf("err3")}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview) error {
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
