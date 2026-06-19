package agents

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// reaperDB is the minimal DB surface the reaper needs (satisfied by *db.DB). The reaper's only
// write goes through the reap_stale_agent_runs SECURITY DEFINER, so a plain WithTx (app role) is
// enough — the DEFINER bypasses RLS for the principal-less sweep.
type reaperDB interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

const (
	defaultReapEvery      = 2 * time.Minute
	defaultReapStaleAfter = 10 * time.Minute
)

// Reaper marks orphaned 'running' agent_run rows as failed. The runner sets a run 'running' then
// executes a loop capped at defaultWallClock (120s); if the worker goroutine dies (backend
// restart or crash) the row is stuck 'running' forever, which would make any "agent working"
// indicator lie. Run() reaps, every Every, runs whose updated_at is older than StaleAfter — a
// window chosen well above the 120s cap so a genuinely-live run is never reaped. Mirrors
// connectors.OutboundDispatcher (ticker loop + staleness window).
type Reaper struct {
	DB         reaperDB
	Logger     *slog.Logger
	Every      time.Duration // tick interval (default 2m)
	StaleAfter time.Duration // a 'running' run older than this is orphaned (default 10m)
}

func (r *Reaper) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Reaper) staleAfter() time.Duration {
	// A zero/negative struct field means "use the default window", NOT "reap every running run":
	// a zero-valued Reaper must never wipe live runs. The reap-all (0s) semantics live only in the
	// DEFINER for a potential single-instance startup sweep; the periodic worker always uses a real
	// window.
	if r.StaleAfter <= 0 {
		return defaultReapStaleAfter
	}
	return r.StaleAfter
}

// ReapOnce marks every 'running' run older than StaleAfter as failed and returns how many were
// reaped (0 is normal).
func (r *Reaper) ReapOnce(ctx context.Context) (int64, error) {
	secs := r.staleAfter().Seconds()
	var n int64
	err := r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT reap_stale_agent_runs($1::double precision)", secs).Scan(&n)
	})
	return n, err
}

// Run reaps on a ticker until ctx is cancelled. A failed pass is logged and retried next tick
// (never fatal — the reaper is a janitor, not on the request path).
func (r *Reaper) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = defaultReapEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.ReapOnce(ctx)
			if err != nil {
				r.logger().WarnContext(ctx, "agents/reaper: pass failed", "err", err)
			} else if n > 0 {
				r.logger().InfoContext(ctx, "agents/reaper: reaped orphaned runs", "count", n)
			}
		}
	}
}
