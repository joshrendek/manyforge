// Command manyforge is the single deployable for the ManyForge platform
// (Constitution Principle V: modular monolith).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/observability"
)

func main() {
	logger := observability.NewLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	// `manyforge migrate` applies migrations then exits.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := db.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
			logger.Error("migrate", "err", err)
			os.Exit(1)
		}
		logger.Info("migrations applied")
		return
	}

	// Phase 2 mounts the chi router + modules here; the stdlib mux carries the
	// operational endpoints so the binary is runnable from day one.
	mux := http.NewServeMux()
	observability.RegisterHealth(mux)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("starting server", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}
