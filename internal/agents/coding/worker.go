package coding

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// systemDB is the minimal cross-tenant DB surface required by CodeReviewWorker.
// The claim, requeue, and fail queries run WITHOUT RLS (system path); per-row
// runJob re-enters WithPrincipal internally via CodeReviewService.
//
// On the real path, satisfy this interface with a thin wrapper around *appdb.DB
// that calls dbgen.New(tx) inside WithTx closures. In unit tests, inject a
// struct that returns a scripted claim batch and records requeue/fail calls.
type systemDB interface {
	// ClaimCodeReviews atomically leases up to limit pending/expired rows across
	// all tenants (system-path query, no RLS set).
	ClaimCodeReviews(ctx context.Context, arg dbgen.ClaimCodeReviewsParams) ([]dbgen.ClaimCodeReviewsRow, error)
	// RequeueCodeReview resets a row to pending after a retriable failure.
	RequeueCodeReview(ctx context.Context, arg dbgen.RequeueCodeReviewParams) error
	// FailCodeReview marks a row terminally failed (max attempts exhausted).
	FailCodeReview(ctx context.Context, arg dbgen.FailCodeReviewParams) error
}

// CodeReviewWorker polls the code_review queue and runs pending jobs.
// It is a system-level process: claim queries run WITHOUT RLS (cross-tenant);
// each job's heavy pipeline runs under the owning principal via
// CodeReviewService.runJob, which calls WithPrincipal internally.
type CodeReviewWorker struct {
	// DB is the system/cross-tenant DB handle used for claim/requeue/fail.
	DB systemDB
	// Svc is the service whose runJob drives the heavy code-review pipeline.
	// May be nil in unit tests when runJobSeam is set.
	Svc *CodeReviewService
	// Logger for structured output on each job transition.
	Logger *slog.Logger
	// Poll is the interval between claim attempts (default 3s).
	Poll time.Duration
	// LeaseSeconds is the lease duration granted to claimed rows (default 900s).
	// Must exceed the sandbox wall-clock cap so a live run is not reclaimed.
	LeaseSeconds int
	// MaxAttempts is the maximum number of attempts before a row is failed
	// terminally (default 3).
	MaxAttempts int
	// Batch is the number of rows claimed per tick (default 2).
	Batch int

	// runJobSeam is an injectable function seam used by unit tests to override
	// the real w.Svc.runJob without requiring a live service or DB. On the real
	// path it is nil and defaults to w.Svc.runJob at first use.
	runJobSeam func(ctx context.Context, job ClaimedReview) error
}

// applyDefaults fills zero/unset fields with package defaults.
func (w *CodeReviewWorker) applyDefaults() {
	if w.Poll <= 0 {
		w.Poll = 3 * time.Second
	}
	if w.LeaseSeconds <= 0 {
		w.LeaseSeconds = 900 // > 10-min sandbox cap → live run never reclaimed
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 3
	}
	if w.Batch <= 0 {
		w.Batch = 2
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// effectiveRunJob returns the runJobSeam if set, otherwise w.Svc.runJob.
func (w *CodeReviewWorker) effectiveRunJob() func(ctx context.Context, job ClaimedReview) error {
	if w.runJobSeam != nil {
		return w.runJobSeam
	}
	return w.Svc.runJob
}

// Run polls the code_review queue at w.Poll intervals until ctx is cancelled.
// Each tick claims up to w.Batch pending rows (cross-tenant, no RLS) and
// processes each one via runJob. On error: requeue if attempts < MaxAttempts;
// fail terminally otherwise. Panics inside runJob are recovered and treated as
// failures so one bad job never kills the worker.
func (w *CodeReviewWorker) Run(ctx context.Context) {
	w.applyDefaults()
	runJob := w.effectiveRunJob()
	w.Logger.InfoContext(ctx, "code review worker started",
		"poll", w.Poll, "lease_seconds", w.LeaseSeconds,
		"max_attempts", w.MaxAttempts, "batch", w.Batch)

	t := time.NewTicker(w.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Logger.InfoContext(ctx, "code review worker stopping")
			return
		case <-t.C:
			w.tick(ctx, runJob)
		}
	}
}

// tick claims one batch and processes each row.
func (w *CodeReviewWorker) tick(ctx context.Context, runJob func(context.Context, ClaimedReview) error) {
	rows, err := w.DB.ClaimCodeReviews(ctx, dbgen.ClaimCodeReviewsParams{
		LeaseSeconds: int32(w.LeaseSeconds),
		Limit:        int32(w.Batch),
	})
	if err != nil {
		w.Logger.ErrorContext(ctx, "code review claim failed", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	w.Logger.InfoContext(ctx, "code review batch claimed", "count", len(rows))
	for _, row := range rows {
		job := rowToClaimedReview(row)
		w.processOne(ctx, job, runJob)
	}
}

// processOne runs a single claimed job, recovering from panics, and calls
// RequeueCodeReview or FailCodeReview based on the outcome and attempt count.
func (w *CodeReviewWorker) processOne(
	ctx context.Context,
	job ClaimedReview,
	runJob func(context.Context, ClaimedReview) error,
) {
	var jobErr error

	func() {
		defer func() {
			if r := recover(); r != nil {
				jobErr = fmt.Errorf("panic in runJob: %v", r)
				w.Logger.ErrorContext(ctx, "code review runJob panicked",
					"id", job.ID, "attempts", job.Attempts, "panic", r)
			}
		}()
		jobErr = runJob(ctx, job)
	}()

	if jobErr == nil {
		w.Logger.InfoContext(ctx, "code review job succeeded",
			"id", job.ID, "attempts", job.Attempts)
		return
	}

	// Failure path: requeue or fail terminally.
	if job.Attempts < w.MaxAttempts {
		w.Logger.WarnContext(ctx, "code review job failed; requeueing",
			"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
		if rerr := w.DB.RequeueCodeReview(ctx, dbgen.RequeueCodeReviewParams{
			ID:              job.ID,
			RunAfterSeconds: 30,
			LastError:       jobErr.Error(),
		}); rerr != nil {
			w.Logger.ErrorContext(ctx, "code review requeue failed", "id", job.ID, "err", rerr)
		}
		return
	}

	w.Logger.ErrorContext(ctx, "code review job exhausted max attempts; failing terminally",
		"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
	if ferr := w.DB.FailCodeReview(ctx, dbgen.FailCodeReviewParams{
		ID:        job.ID,
		LastError: jobErr.Error(),
	}); ferr != nil {
		w.Logger.ErrorContext(ctx, "code review fail-terminal write failed", "id", job.ID, "err", ferr)
	}
}

// rowToClaimedReview maps a dbgen.ClaimCodeReviewsRow (with pgtype.UUID fields)
// into a ClaimedReview (with plain uuid.UUID fields).
// A NULL principal_id or agent_id is mapped to uuid.Nil (safe: runJob will
// re-resolve and fail with a clear error rather than executing with a nil UUID).
func rowToClaimedReview(r dbgen.ClaimCodeReviewsRow) ClaimedReview {
	principalID := uuid.Nil
	if r.PrincipalID.Valid {
		principalID = r.PrincipalID.Bytes
	}
	agentID := uuid.Nil
	if r.AgentID.Valid {
		agentID = r.AgentID.Bytes
	}
	return ClaimedReview{
		ID:              r.ID,
		BusinessID:      r.BusinessID,
		PrincipalID:     principalID,
		AgentID:         agentID,
		RepoConnectorID: r.RepoConnectorID,
		PRNumber:        int(r.PrNumber),
		Attempts:        int(r.Attempts),
	}
}

// pgtype is used via dbgen.ClaimCodeReviewsRow (PrincipalID, AgentID pgtype.UUID).
// This blank identifier keeps the import from being pruned if the compiler
// inlines the struct layout; the real usage is in rowToClaimedReview above.
var _ pgtype.UUID
