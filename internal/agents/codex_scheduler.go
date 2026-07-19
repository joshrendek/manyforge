package agents

import (
	"context"
	"log/slog"
	"time"
)

// codexRefresher is the worker's seam (satisfied by *CodexTokenService).
type codexRefresher interface {
	RefreshDue(ctx context.Context) (int, error)
}

// defaultCodexRefreshEvery mirrors the MANYFORGE_CODEX_REFRESH_INTERVAL config default (see
// internal/platform/config/config.go) and is used whenever Every is non-positive.
const defaultCodexRefreshEvery = 30 * time.Minute

// CodexRefreshWorker periodically refreshes near-expiry openai_codex tokens across all tenants so
// idle credentials stay warm (connection-health) without a review run. RefreshDue's SKIP LOCKED
// makes it multi-replica safe with no leader election. Cancel ctx (the shared workerCtx) to stop.
type CodexRefreshWorker struct {
	Svc    codexRefresher
	Logger *slog.Logger
	Every  time.Duration
}

func (w *CodexRefreshWorker) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = defaultCodexRefreshEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.Svc.RefreshDue(ctx)
			if err != nil {
				w.Logger.WarnContext(ctx, "codex refresh sweep", "err", err)
			} else if n > 0 {
				w.Logger.InfoContext(ctx, "codex tokens refreshed", "count", n)
			}
		}
	}
}
