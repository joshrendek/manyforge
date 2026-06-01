// Command manyforge is the single deployable for the ManyForge platform
// (Constitution Principle V: modular monolith).
package main

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
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
	invSvc := &invitations.Service{DB: database, Mailer: mailer.LogMailer{Logger: logger}}
	acctH := account.NewHandler(acctSvc)
	tenH := tenancy.NewHandler(tenSvc)
	authzH := authz.NewHandler(authzSvc)
	invH := invitations.NewHandler(invSvc)

	// SL-C event bus + transactional-outbox worker. Support-desk services
	// (US1/US2) register their subscribers on eventBus before the worker starts,
	// so no event is drained without a handler. The in-process SMTP receiver
	// (cfg.SMTPAddr) and the inbox/ticketing routes are wired with their adapters
	// and handlers in US1.
	eventBus := events.NewBus()
	outboxWorker := &events.Worker{DB: database, Bus: eventBus, Logger: logger}

	// Attachment object store (SL-E). A configured MANYFORGE_BLOB_URL opens the
	// file://|s3:// bucket; empty (dev) leaves the store nil — NewService tolerates
	// a nil store by emitting no attachment rows, so nothing references a blob that
	// is never written.
	var blobStore blob.Store
	if cfg.BlobURL != "" {
		b, err := blob.Open(ctx, cfg.BlobURL)
		if err != nil {
			logger.Error("open blob store", "err", err)
			os.Exit(1)
		}
		defer func() { _ = b.Close() }()
		blobStore = b
	} else {
		logger.Warn("MANYFORGE_BLOB_URL unset; inbound attachments disabled")
	}

	// US1 inbound ingestion. The reply-token key degrades gracefully when unset in
	// dev (nil key ⇒ threading falls back to RFC822 headers; the webhook path does
	// not verify reply tokens, so this never panics).
	inboxSvc := inbox.NewService(database, blobStore, inbox.Config{
		ReplyTokenKey:       cfg.InboundReplyTokenSecret,
		AttachmentMaxBytes:  cfg.AttachmentMaxBytes,
		InboundSystemDomain: cfg.InboundSystemDomain,
	}, logger)
	inboxWebhookH := inbox.NewWebhookHandler(inboxSvc, cfg.InboundWebhookSecret, cfg.InboundMaxBytes, inbox.Config{
		InboundSystemDomain: cfg.InboundSystemDomain,
	}, logger)

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

	// Per-IP rate limiting for the unauthenticated abuse surface — signup, login,
	// email verification (FR-029). The key is the trusted-proxy-aware client IP so
	// a spoofed X-Forwarded-For cannot evade it.
	trusted := parseTrustedCIDRs(cfg.TrustedProxyCIDR, logger)
	authLimiter := ratelimit.NewTokenBucket(cfg.RateLimitRPS, cfg.RateLimitBurst)
	ipKey := func(r *http.Request) string { return ratelimit.ClientIP(r, trusted) }

	mux.Route("/api/v1", func(r chi.Router) {
		r.Group(func(pub chi.Router) {
			pub.Use(httpx.RateLimit(authLimiter, ipKey))
			acctH.PublicRoutes(pub)
		})
		// Inbound provider webhook (US1): public, authenticated by the per-provider
		// HMAC signature, NOT by JWT and NOT the per-IP auth limiter. Mounted in its
		// own public group so a per-recipient ingest rate limiter (T032) can wrap it.
		r.Group(func(ingress chi.Router) {
			inboxWebhookH.PublicRoutes(ingress)
		})
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			acctH.ProtectedRoutes(pr)
			tenH.ProtectedRoutes(pr)
			authzH.ProtectedRoutes(pr)
			invH.ProtectedRoutes(pr)
		})
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Drain the transactional outbox in the background until shutdown.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go outboxWorker.Run(workerCtx)

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

	workerCancel() // stop draining the outbox before closing the DB pool
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}

// parseTrustedCIDRs parses a comma-separated CIDR list (MANYFORGE_TRUSTED_PROXY_CIDR)
// into networks whose X-Forwarded-For headers are honored for client-IP resolution.
// Malformed entries are logged and skipped; an empty list means no proxy is trusted
// (the direct peer is authoritative).
func parseTrustedCIDRs(s string, logger *slog.Logger) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			logger.Warn("ignoring malformed trusted proxy CIDR", "cidr", c, "err", err)
			continue
		}
		out = append(out, n)
	}
	return out
}
