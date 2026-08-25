package timeseries

import (
	"context"
	"errors"
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

// rollupSpecs names the SECURITY DEFINER rollup functions this worker drives and their durable
// state rows. Each function takes
// (lag interval, overlap interval) and returns the number of bucket rows written.
//
// This list is a COMPILE-TIME constant and is interpolated into SQL. It must never be fed from
// configuration or a request — a rollup name is an identifier, not a bindable parameter, so a
// caller-supplied value here would be injection.
type rollupSpec struct {
	function string
	state    string
}

var rollupSpecs = []rollupSpec{
	{function: "rollup_analytics_daily", state: "analytics_daily"},
	{function: "rollup_analytics_pageviews", state: "analytics_pageviews"},
	{function: "rollup_analytics_dimensions", state: "analytics_dimensions"},
}

// SweepOnce advances every rollup by one window and returns the total bucket rows written. A
// negative Lag is clamped to zero, which lets a test sweep right up to now().
//
// Each rollup runs in its own transaction: they take different advisory locks, and one failing
// (say, a rollup whose table a migration is mid-way through altering) must not roll back the
// others or stall the whole pipeline.
func (w *RollupWorker) SweepOnce(ctx context.Context) (int, error) {
	lag := max(w.Lag, 0)
	total := 0
	var failures []error
	for _, spec := range rollupSpecs {
		started := time.Now()
		var n int
		var watermark time.Time
		err := w.DB.WithTx(ctx, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx,
				"SELECT "+spec.function+"(make_interval(secs => $1::int), make_interval(secs => $2::int))",
				int(lag.Seconds()), int(w.overlap().Seconds())).Scan(&n); err != nil {
				return err
			}
			return tx.QueryRow(ctx,
				`SELECT watermark_ingested_at FROM rollup_state WHERE rollup_name = $1`,
				spec.state).Scan(&watermark)
		})
		metricBase := "rollup." + spec.state
		w.Metrics.Set(metricBase+".duration_ms", max(time.Since(started).Milliseconds(), int64(1)))
		if err != nil {
			w.Metrics.Inc(metricBase + ".failures")
			failures = append(failures, fmt.Errorf("%s: %w", spec.function, err))
			continue
		}
		total += n
		now := time.Now().UTC()
		w.Metrics.Set(metricBase+".buckets_written", int64(n))
		w.Metrics.Set(metricBase+".last_success_unix", now.Unix())
		w.Metrics.Set(metricBase+".watermark_lag_seconds", max(int64(now.Sub(watermark).Seconds()), int64(0)))
	}
	return total, errors.Join(failures...)
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
			if n > 0 {
				w.Metrics.Add(observability.MetricRollupBucketsWritten, int64(n))
			}
			if err != nil {
				// Each failed rollup leaves only its own watermark unadvanced; successful siblings
				// committed independently and the next tick safely retries the failed windows.
				w.Logger.ErrorContext(ctx, "rollup sweep", "err", err)
				w.Metrics.Inc(observability.MetricRollupSweepFailed)
				continue
			}
		}
	}
}
