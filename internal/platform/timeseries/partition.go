// Package timeseries owns the shared time-series storage machinery for manyforge-p20: partition
// lifecycle management for any table registered in partitioned_table, and idempotent rollup
// sweeps. Consumers (analytics, crash reporting) declare a table and a retention policy; this
// package does the rest.
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

// MaintenanceWorker keeps partitions ahead of ingest and drops them once they age past their
// table's retain_for. Both halves run through SECURITY DEFINER functions because manyforge_app
// holds no CREATE privilege — the same principal-less pattern as the outbox drain.
//
// Safe to run on every replica: the underlying DDL is guarded by CREATE TABLE IF NOT EXISTS
// semantics (to_regclass check) and drops are idempotent, so a concurrent sweep is a no-op
// rather than an error.
type MaintenanceWorker struct {
	DB      *db.DB
	Logger  *slog.Logger
	Metrics *observability.Metrics
	// Every defaults to 1h. Partitions are pre-created several periods ahead, so the cadence is
	// not latency-critical — it only has to beat precreate_ahead × granularity.
	Every time.Duration
}

func (w *MaintenanceWorker) withDefaults() {
	if w.Every <= 0 {
		w.Every = time.Hour
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// SweepOnce creates any due partitions and drops any expired ones, in a single transaction so a
// failure part-way leaves neither half applied.
func (w *MaintenanceWorker) SweepOnce(ctx context.Context) (created, dropped int, err error) {
	err = w.DB.WithTx(ctx, func(tx pgx.Tx) error {
		if qerr := tx.QueryRow(ctx, "SELECT create_due_partitions()").Scan(&created); qerr != nil {
			return fmt.Errorf("create due partitions: %w", qerr)
		}
		if qerr := tx.QueryRow(ctx, "SELECT drop_expired_partitions()").Scan(&dropped); qerr != nil {
			return fmt.Errorf("drop expired partitions: %w", qerr)
		}
		// Drop analytics salts past the raw-event retention window. This is what makes an aged-out
		// visitor_hash permanently un-derivable rather than merely inconvenient to reverse, so it
		// belongs with retention rather than on its own schedule.
		var purged int
		if qerr := tx.QueryRow(ctx, "SELECT purge_expired_analytics_salts()").Scan(&purged); qerr != nil {
			return fmt.Errorf("purge expired analytics salts: %w", qerr)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return created, dropped, nil
}

// Run sweeps immediately, then on every tick until ctx is cancelled. The immediate sweep matters
// on a cold start: a fresh deploy must not wait a full interval for today's partition to exist.
//
// A failed sweep is loud but non-fatal — precreate_ahead gives several periods of slack before
// ingest could actually fail, which is the entire point of creating partitions ahead of time.
func (w *MaintenanceWorker) Run(ctx context.Context) {
	w.withDefaults()
	w.Logger.InfoContext(ctx, "partition maintenance worker started", "every", w.Every)
	w.sweepAndLog(ctx)
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweepAndLog(ctx)
		}
	}
}

func (w *MaintenanceWorker) sweepAndLog(ctx context.Context) {
	created, dropped, err := w.SweepOnce(ctx)
	if err != nil {
		w.Logger.ErrorContext(ctx, "partition maintenance sweep", "err", err)
		w.Metrics.Inc(observability.MetricPartitionSweepFailed)
		return
	}
	if created > 0 {
		w.Logger.InfoContext(ctx, "partitions created", "count", created)
		w.Metrics.Add(observability.MetricPartitionCreated, int64(created))
	}
	if dropped > 0 {
		w.Logger.InfoContext(ctx, "partitions dropped", "count", dropped)
		w.Metrics.Add(observability.MetricPartitionDropped, int64(dropped))
	}
}
