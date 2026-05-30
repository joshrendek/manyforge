// Command manyforge is the single deployable for the ManyForge platform
// (Constitution Principle V: modular monolith).
package main

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/tenancy"
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

	ctx := context.Background()
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("connect database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// Dev key ring: ephemeral EdDSA keys. Tokens do not survive a restart;
	// configure persistent keys for production (see research R4).
	ring, err := auth.NewDevKeyRing(cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		logger.Error("build key ring", "err", err)
		os.Exit(1)
	}
	logger.Warn("using ephemeral dev JWT keys; access tokens are invalid across restarts")

	acctSvc := &account.Service{
		DB: database, Ring: ring, Mailer: mailer.LogMailer{Logger: logger},
		AccessTTL: cfg.AccessTokenTTL, RefreshTTL: 30 * 24 * time.Hour, TokenTTL: 24 * time.Hour,
	}
	tenSvc := &tenancy.Service{DB: database}
	authzSvc := &authz.Service{DB: database}
	acctH := account.NewHandler(acctSvc)
	tenH := tenancy.NewHandler(tenSvc)
	authzH := authz.NewHandler(authzSvc)

	mux := httpx.NewRouter(ring)
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := database.Pool().Ping(r.Context()); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Code: "NOT_READY", Message: "database unavailable"})
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/metrics", expvar.Handler())

	mux.Route("/api/v1", func(r chi.Router) {
		acctH.PublicRoutes(r)
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			acctH.ProtectedRoutes(pr)
			tenH.ProtectedRoutes(pr)
			authzH.ProtectedRoutes(pr)
		})
	})

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

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}
