package coding

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
)

// systemDB is the minimal cross-tenant DB surface required by CodeReviewWorker.
// Claim/requeue/fail run principal-less (system path): the worker is a system
// process with no manyforge.principal_id GUC. Because code_review has RLS ENABLEd
// (migrations/0071) and the app connects as manyforge_app (NOSUPERUSER NOBYPASSRLS),
// a principal-less UPDATE would be RLS-blocked. So these methods route through the
// SECURITY DEFINER functions claim_code_reviews / requeue_code_review /
// fail_code_review (migrations/0073), whose owner bypasses RLS — exactly the
// pattern the outbox drain uses (claim_outbox_batch, migrations/0016).
//
// On the real path, satisfy this with *AppDBAdapter (raw pgx inside DB.WithTx).
// In unit tests, inject a struct that returns a scripted claim batch and records
// requeue/fail calls.
type systemDB interface {
	// ClaimCodeReviews atomically leases up to limit runnable rows across all
	// tenants via the claim_code_reviews SECURITY DEFINER function.
	ClaimCodeReviews(ctx context.Context, leaseSeconds, limit int) ([]ClaimedReview, error)
	// RequeueCodeReview resets a row to pending after a retriable failure.
	RequeueCodeReview(ctx context.Context, id uuid.UUID, delaySeconds int, lastError string) error
	// FailCodeReview marks a row terminally failed (max attempts exhausted).
	FailCodeReview(ctx context.Context, id uuid.UUID, lastError string) error
	// RenewLease renews a running row's lease AND persists its progress snapshot via
	// the renew_code_review_lease SECURITY DEFINER function (migrations/0076). The
	// status='running' guard makes a renew after terminal a harmless no-op; a nil
	// progress leaves the column unchanged-as-NULL.
	RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error
}

// CodeReviewWorker polls the code_review queue and runs pending jobs.
// It is a system-level process: claim/requeue/fail run principal-less through the
// SECURITY DEFINER queue functions (cross-tenant, RLS-bypassing by owner); each
// job's heavy pipeline runs under the owning principal via
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
	// HeartbeatInterval is how often a running job's lease is renewed and its progress
	// persisted (default 5s). Must be well under LeaseSeconds so a live job is never
	// re-claimed.
	HeartbeatInterval time.Duration

	// runJobSeam is an injectable function seam used by unit tests to override
	// the real w.Svc.runJob without requiring a live service or DB. On the real
	// path it is nil and defaults to w.Svc.runJob at first use.
	runJobSeam func(ctx context.Context, job ClaimedReview, prog *Progress) error
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
	if w.HeartbeatInterval <= 0 {
		w.HeartbeatInterval = 5 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// effectiveRunJob returns the runJobSeam if set, otherwise w.Svc.runJob.
func (w *CodeReviewWorker) effectiveRunJob() func(ctx context.Context, job ClaimedReview, prog *Progress) error {
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
func (w *CodeReviewWorker) tick(ctx context.Context, runJob func(context.Context, ClaimedReview, *Progress) error) {
	jobs, err := w.DB.ClaimCodeReviews(ctx, w.LeaseSeconds, w.Batch)
	if err != nil {
		w.Logger.ErrorContext(ctx, "code review claim failed", "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	w.Logger.InfoContext(ctx, "code review batch claimed", "count", len(jobs))
	for _, job := range jobs {
		w.processOne(ctx, job, runJob)
	}
}

// processOne runs a single claimed job, recovering from panics, and calls
// RequeueCodeReview or FailCodeReview based on the outcome and attempt count.
func (w *CodeReviewWorker) processOne(
	ctx context.Context,
	job ClaimedReview,
	runJob func(context.Context, ClaimedReview, *Progress) error,
) {
	var jobErr error

	// Heartbeat: renew the lease + persist progress every HeartbeatInterval while
	// runJob is in flight, so a job exceeding the base lease is never re-claimed.
	prog := &Progress{}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(w.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if rerr := w.DB.RenewLease(ctx, job.ID, w.LeaseSeconds, prog.Snapshot()); rerr != nil {
					w.Logger.WarnContext(ctx, "code review lease renew failed", "id", job.ID, "err", rerr)
				}
			}
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				jobErr = fmt.Errorf("panic in runJob: %v", r)
				w.Logger.ErrorContext(ctx, "code review runJob panicked",
					"id", job.ID, "attempts", job.Attempts, "panic", r)
			}
		}()
		jobErr = runJob(ctx, job, prog)
	}()
	close(stop)

	if jobErr == nil {
		w.Logger.InfoContext(ctx, "code review job succeeded",
			"id", job.ID, "attempts", job.Attempts)
		return
	}

	// Failure path: requeue or fail terminally.
	if job.Attempts < w.MaxAttempts {
		w.Logger.WarnContext(ctx, "code review job failed; requeueing",
			"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
		if rerr := w.DB.RequeueCodeReview(ctx, job.ID, 30, jobErr.Error()); rerr != nil {
			w.Logger.ErrorContext(ctx, "code review requeue failed", "id", job.ID, "err", rerr)
		}
		return
	}

	w.Logger.ErrorContext(ctx, "code review job exhausted max attempts; failing terminally",
		"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
	if ferr := w.DB.FailCodeReview(ctx, job.ID, jobErr.Error()); ferr != nil {
		w.Logger.ErrorContext(ctx, "code review fail-terminal write failed", "id", job.ID, "err", ferr)
	}
}

// AppDBAdapter adapts *appdb.DB to the systemDB interface required by
// CodeReviewWorker. All three methods run WITHOUT an RLS principal context
// (WithTx, not WithPrincipal) and call the code_review queue's SECURITY DEFINER
// functions (migrations/0073), whose owner bypasses RLS — exactly the same pattern
// the outbox worker uses for its principal-less drain.
//
// Usage in main.go:
//
//	crWorker := &coding.CodeReviewWorker{DB: &coding.AppDBAdapter{DB: database}, ...}
type AppDBAdapter struct {
	DB *appdb.DB
}

// ClaimCodeReviews runs the cross-tenant claim via the claim_code_reviews
// SECURITY DEFINER function (RLS bypassed by the function owner).
func (a *AppDBAdapter) ClaimCodeReviews(ctx context.Context, leaseSeconds, limit int) ([]ClaimedReview, error) {
	var out []ClaimedReview
	if err := a.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, business_id, principal_id, agent_id, repo_connector_id, pr_number, attempts
			   FROM claim_code_reviews($1, $2)`,
			leaseSeconds, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id, businessID, repoConnectorID uuid.UUID
				principalID, agentID            pgtype.UUID
				prNumber, attempts              int32
			)
			if err := rows.Scan(&id, &businessID, &principalID, &agentID,
				&repoConnectorID, &prNumber, &attempts); err != nil {
				return err
			}
			out = append(out, claimedReviewFromRow(
				id, businessID, principalID, agentID, repoConnectorID, prNumber, attempts))
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// RequeueCodeReview resets a row to pending after a retriable failure via the
// requeue_code_review SECURITY DEFINER function.
func (a *AppDBAdapter) RequeueCodeReview(ctx context.Context, id uuid.UUID, delaySeconds int, lastError string) error {
	return a.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT requeue_code_review($1, $2, $3)", id, delaySeconds, lastError)
		return err
	})
}

// FailCodeReview marks a row terminally failed via the fail_code_review SECURITY
// DEFINER function (max attempts exhausted).
func (a *AppDBAdapter) FailCodeReview(ctx context.Context, id uuid.UUID, lastError string) error {
	return a.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT fail_code_review($1, $2)", id, lastError)
		return err
	})
}

// RenewLease renews a running row's lease and persists its progress snapshot via the
// renew_code_review_lease SECURITY DEFINER function (RLS bypassed by the function
// owner). A nil progress is encoded as SQL NULL (jsonb), leaving the column unchanged.
func (a *AppDBAdapter) RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error {
	return a.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT renew_code_review_lease($1, $2, $3)", id, leaseSeconds, progress)
		return err
	})
}

// claimedReviewFromRow maps a claim_code_reviews result row (with pgtype.UUID for
// the nullable principal_id/agent_id columns) into a ClaimedReview. A NULL
// principal_id or agent_id maps to uuid.Nil (safe: runJob re-resolves and fails
// with a clear error rather than executing with a nil UUID).
func claimedReviewFromRow(
	id, businessID uuid.UUID,
	principalID, agentID pgtype.UUID,
	repoConnectorID uuid.UUID,
	prNumber, attempts int32,
) ClaimedReview {
	pid := uuid.Nil
	if principalID.Valid {
		pid = principalID.Bytes
	}
	aid := uuid.Nil
	if agentID.Valid {
		aid = agentID.Bytes
	}
	return ClaimedReview{
		ID:              id,
		BusinessID:      businessID,
		PrincipalID:     pid,
		AgentID:         aid,
		RepoConnectorID: repoConnectorID,
		PRNumber:        int(prNumber),
		Attempts:        int(attempts),
	}
}
