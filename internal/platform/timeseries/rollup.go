package timeseries

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/observability"
)

// RollupWorker advances the pre-aggregated rollups that survive raw-event retention.
//
// The sweep is deliberately NOT outbox-driven. At the design target of ~1M events/day, one outbox
// row per event would drown the shared outbox that also carries ticket, notification, and
// connector events. Instead the worker walks a watermark over ingested_at and recomputes the
// occurred_at buckets that moved.
//
// Correctness rests on rollup_analytics_daily recomputing each touched bucket rather than
// incrementing it: execution is at-least-once, so an increment would double-count silently on any
// retry. Because recomputation is idempotent, a failed sweep can simply leave the watermark where
// it was and let the next tick redo the same window.
type RollupWorker struct {
	DB      *db.DB
	Logger  *slog.Logger
	Metrics *observability.Metrics
	// Every defaults to 1m.
	Every time.Duration
	// Lag defaults to 30s. It holds the sweep back from the very edge of now() so rows from
	// transactions that committed slightly out of clock order are not skipped past by the
	// watermark. Raising it trades rollup freshness for a wider safety margin.
	Lag time.Duration
	// Overlap defaults to 5m and re-scans a trailing slice of already-swept time on every pass.
	// It closes the straggler race: a write that STARTED before the previous cutoff but COMMITTED
	// after that sweep's snapshot carries an ingested_at at or below the old watermark, so a
	// strictly forward-only window would skip it permanently and silently. Re-scanning costs
	// nothing because the rollup recomputes buckets rather than incrementing them.
	Overlap time.Duration
}

// overlap returns the configured trailing re-scan window, defaulting to 5m. Zero is not treated
// as "no overlap" — a caller that wants none must set a negative value, so forgetting to set the
// field cannot silently reintroduce the straggler race.
func (w *RollupWorker) overlap() time.Duration {
	if w.Overlap == 0 {
		return 5 * time.Minute
	}
	return max(w.Overlap, 0)
}

func (w *RollupWorker) withDefaults() {
	if w.Every <= 0 {
		w.Every = time.Minute
	}
	if w.Lag <= 0 {
		w.Lag = 30 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// SweepOnce advances the analytics daily rollup by one window and returns the number of bucket
// rows written. A negative Lag is clamped to zero, which lets a test sweep right up to now().
func (w *RollupWorker) SweepOnce(ctx context.Context) (int, error) {
	lag := max(w.Lag, 0)
	var n int
	err := w.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT rollup_analytics_daily(make_interval(secs => $1::int), make_interval(secs => $2::int))",
			int(lag.Seconds()), int(w.overlap().Seconds())).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("rollup analytics daily: %w", err)
	}
	return n, nil
}

// Run sweeps on every tick until ctx is cancelled.
func (w *RollupWorker) Run(ctx context.Context) {
	w.withDefaults()
	w.Logger.InfoContext(ctx, "rollup worker started", "every", w.Every, "lag", w.Lag)
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.SweepOnce(ctx)
			if err != nil {
				// Watermark is unadvanced; the next tick retries the same window. Safe only
				// because the rollup recomputes rather than increments.
				w.Logger.ErrorContext(ctx, "rollup sweep", "err", err)
				w.Metrics.Inc(observability.MetricRollupSweepFailed)
				continue
			}
			if n > 0 {
				w.Metrics.Add(observability.MetricRollupBucketsWritten, int64(n))
			}
		}
	}
}
