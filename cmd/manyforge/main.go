// Command manyforge is the single deployable for the ManyForge platform
// (Constitution Principle V: modular monolith).
package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
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

	smtp "github.com/emersion/go-smtp"
	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/config"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
	"github.com/manyforge/manyforge/internal/tenancy"
	"github.com/manyforge/manyforge/internal/ticketing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	// Outbound send rate limiter (FR-020): per-business AND per-recipient token
	// buckets built from the SAME outbound knobs (MANYFORGE_OUTBOUND_RATE_*),
	// mirroring how the ingest limiter is built from the ingest knobs. The
	// ticketing.Service spends a token per Reply; a 429 carries no existence oracle.
	outboundLimiter := ratelimit.NewTokenBucket(cfg.OutboundRateRPS, cfg.OutboundRateBurst)
	ticketSvc := &ticketing.Service{
		DB:              database,
		ReplyTokenKey:   cfg.InboundReplyTokenSecret,
		SystemDomain:    cfg.InboundSystemDomain,
		OutboundLimiter: outboundLimiter,
		Suppression:     notify.DBSuppression{DB: database},
	}
	acctH := account.NewHandler(acctSvc)
	tenH := tenancy.NewHandler(tenSvc)
	authzH := authz.NewHandler(authzSvc)
	invH := invitations.NewHandler(invSvc)

	// PermissionResolver adapter over authz.Resolve. RequirePermission is the first
	// consumer (US1 ticketing): it resolves the caller's effective perms at the
	// target business INSIDE the caller's RLS principal context and renders an
	// IDENTICAL 404 (never 403) for a missing principal, an invisible/foreign
	// business, OR a lacking permission — matching the 002 no-oracle contract.
	permResolve := func(ctx context.Context, tx pgx.Tx, pid, bid uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, pid, bid)
	}
	// The ticketing handler also takes the resolver + db for the CONDITIONAL
	// tickets.assign gate on triage (the route is tickets.write; an assignee change
	// additionally requires tickets.assign per the OpenAPI), resolved no-oracle (404).
	ticketH := ticketing.NewHandler(ticketSvc, database, permResolve)
	// businessIDFromPath reads the {id} path param. A malformed id is treated as a
	// 404 by RequirePermission (no oracle), consistent with the read handlers.
	businessIDFromPath := func(r *http.Request) (uuid.UUID, error) {
		return uuid.Parse(chi.URLParam(r, "id"))
	}

	// US4 inbox-management identity surface (custom email domains + verification +
	// custom inbound addresses). The DKIM master key seals per-domain Ed25519 private
	// keys at rest; when MANYFORGE_DKIM_MASTER_KEY is unset the sealer is nil and
	// CreateEmailDomain refuses (custom sending degrades to system-only) — the server
	// still boots. An explicitly-set-but-wrong-length key is a hard config error
	// caught at Load(), so NewSealer here only fails on an internal cipher error.
	var dkimSealer ticketing.KeySealer
	if len(cfg.DKIMMasterKey) > 0 {
		sealer, serr := mfcrypto.NewSealer(cfg.DKIMMasterKey)
		if serr != nil {
			logger.Error("init DKIM sealer", "err", serr)
			os.Exit(1)
		}
		dkimSealer = sealer
	} else {
		logger.Warn("MANYFORGE_DKIM_MASTER_KEY unset; custom email-domain signing disabled (system identity only)")
	}
	identitySvc := &ticketing.IdentityService{
		DB:             database,
		Resolver:       ticketing.NetTXTResolver{},
		Sealer:         dkimSealer,
		SystemMailHost: cfg.InboundSystemDomain,
	}
	identityH := ticketing.NewIdentityHandler(identitySvc)

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
	// US2 hard-bounce intake (T040): a provider-signed (separate InboundBounceSecret)
	// webhook that suppresses the bounced recipient (global email_suppression) and
	// marks the correlated outbound message failed via a DEFINER. Mounted next to the
	// inbound webhook in the same per-IP ingest-rate-limited public group; no JWT.
	bounceH := inbox.NewBounceHandler(inbox.NewDBBounceSuppressor(database), cfg.InboundBounceSecret, cfg.InboundMaxBytes, logger)

	// FR-001 zero-config inbound: every business gets a system inbound address on
	// creation. tenancy emits business.created (in the create tx, via the outbox); the
	// inbox Provisioner — subscribed here, BEFORE the worker starts — provisions the
	// address inside the worker's tx. This avoids a tenancy→inbox import cycle. The
	// handler is idempotent (deterministic keyed localpart + savepoint-guarded INSERT),
	// safe under at-least-once delivery.
	inboxProvisioner := inbox.NewProvisioner(database, inbox.ProvisionConfig{
		SystemDomain: cfg.InboundSystemDomain,
		SystemKey:    cfg.InboundSystemAddrSecret,
	}, logger)
	eventBus.Subscribe(events.TopicBusinessCreated, inboxProvisioner.Handle)

	// US2 outbound send worker (T039): drains ticket.replied → builds the threaded
	// Mail (From/Reply-To on the business's system inbound address) → dispatches via
	// the Sender → records delivery_state. Registered BEFORE the worker starts so no
	// reply event is drained without a handler. Sender selection: a configured SMTP
	// relay (MANYFORGE_SMTP_HOST) uses the real SMTPSender (optionally DKIM-signed);
	// otherwise the dev LogSender logs the threaded reply. Both honor the suppression
	// list. The handler is idempotent (skips a message already 'sent').
	var sender notify.Sender
	if cfg.SMTPHost != "" {
		dkimCfg, derr := dkimConfigFromCfg(cfg)
		if derr != nil {
			// A configured-but-unparseable DKIM key fails startup loudly rather than
			// silently sending unsigned mail (deliverability/spoofing risk).
			logger.Error("parse system DKIM key", "err", derr)
			os.Exit(1)
		}
		sender = notify.NewSMTPSender(notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPass,
			DKIM: dkimCfg, // nil ⇒ unsigned (the locked default when no DKIM key is configured)
		}, notify.DBSuppression{DB: database})
		logger.Info("outbound mail via SMTP relay", "host", cfg.SMTPHost, "dkim", dkimCfg != nil)
	} else {
		sender = notify.LogSender{Logger: logger, Suppression: notify.DBSuppression{DB: database}}
		logger.Warn("MANYFORGE_SMTP_HOST unset; outbound replies are logged, not sent (dev LogSender)")
	}
	// US4 outbound identity selection (T059/FR-013): the send subscriber shares the
	// SAME DKIM sealer the IdentityService uses, so it can unseal a verified custom
	// domain's private key and sign the reply as that domain. When the sealer is nil
	// (no MANYFORGE_DKIM_MASTER_KEY), the send path simply never selects a custom
	// identity and every reply goes out from the system address — the correct degrade.
	sendSub := notify.SendSubscriber{Sender: sender, Logger: logger, Sealer: dkimSealer}
	eventBus.Subscribe(events.TopicTicketReplied, sendSub.Handle)

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

	// Inbound ingestion rate limiting (FR-020), the abuse/loop bound on the public
	// ingress. TWO independent token-bucket layers built from the SAME ingest knobs,
	// shared across the webhook AND SMTP paths so a given source/recipient cannot
	// evade one transport by hopping to the other:
	//   - ingestIPLimiter: per-IP. Wraps the webhook group via httpx.RateLimit and
	//     gates inbound SMTP DATA from the connection remote IP. BOTH transports key
	//     on inbox.IPRateLimitKey(<bare client IP>) — the webhook IP comes from the
	//     trusted-proxy-aware ratelimit.ClientIP (spoofed X-Forwarded-For can't evade
	//     it), the SMTP IP from the connection peer. Same IP ⇒ same key ⇒ same bucket,
	//     so an IP at budget on one transport is also throttled on the other (no
	//     transport-hopping evasion). The shared key shape lives in one function so it
	//     cannot silently drift between the two call sites.
	//   - ingestRecipientLimiter: per-DECODED-recipient on the webhook path. Enforced
	//     inside the handler BEFORE recipient resolution so a known and an unknown
	//     recipient throttle identically (no existence oracle).
	ingestIPLimiter := ratelimit.NewTokenBucket(cfg.IngestRateRPS, cfg.IngestRateBurst)
	ingestRecipientLimiter := ratelimit.NewTokenBucket(cfg.IngestRateRPS, cfg.IngestRateBurst)
	inboxWebhookH.SetRecipientLimiter(ingestRecipientLimiter)
	// ingestIPKey unifies the webhook per-IP key with the SMTP per-IP key: both run
	// the bare client IP through inbox.IPRateLimitKey so the two share one bucket.
	ingestIPKey := func(r *http.Request) string { return inbox.IPRateLimitKey(ratelimit.ClientIP(r, trusted)) }

	mountAPIRoutes(mux, apiHandlers{
		account:       acctH,
		tenancy:       tenH,
		authz:         authzH,
		invitations:   invH,
		ticketing:     ticketH,
		identity:      identityH,
		inboxWebhook:  inboxWebhookH,
		bounce:        bounceH,
		authLimit:     httpx.RateLimit(authLimiter, ipKey),
		ingestLimit:   httpx.RateLimit(ingestIPLimiter, ingestIPKey),
		ticketsRead:   httpx.RequirePermission(database, permResolve, "tickets.read", businessIDFromPath),
		ticketsReply:  httpx.RequirePermission(database, permResolve, "tickets.reply", businessIDFromPath),
		ticketsWrite:  httpx.RequirePermission(database, permResolve, "tickets.write", businessIDFromPath),
		ticketsAssign: httpx.RequirePermission(database, permResolve, "tickets.assign", businessIDFromPath),
		inboxManage:   httpx.RequirePermission(database, permResolve, "inbox.manage", businessIDFromPath),
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

	// In-process inbound SMTP receiver (US1). Started ONLY when MANYFORGE_SMTP_ADDR
	// is set; in dev it is empty and the receiver is disabled. STARTTLS is
	// opportunistic — no cert is configured here, so the listener runs plaintext
	// (inbound MX is best-effort TLS). It reuses the same inbox.Service the webhook
	// path does, so SMTP and webhook deliveries produce identical ticket shapes.
	var smtpAdapter *inbox.SMTPAdapter
	if cfg.SMTPAddr != "" {
		smtpAdapter = inbox.NewSMTPAdapter(cfg.SMTPAddr, inboxSvc, inboxSvc, cfg.InboundMaxBytes, ingestIPLimiter, nil, logger)
		go func() {
			logger.Info("starting inbound SMTP receiver", "addr", cfg.SMTPAddr)
			if err := smtpAdapter.ListenAndServe(); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
				// Do not crash the process on an SMTP bind/serve failure: the HTTP and
				// webhook ingestion paths stay up. Log and continue.
				logger.Error("inbound SMTP receiver stopped", "err", err)
			}
		}()
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

	workerCancel() // stop draining the outbox before closing the DB pool
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if smtpAdapter != nil {
		if err := smtpAdapter.Shutdown(shutdownCtx); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			logger.Error("inbound SMTP graceful shutdown failed", "err", err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}

// apiHandlers carries the feature handlers and the group-level middleware that
// mountAPIRoutes wires onto the /api/v1 router. Extracting the mount logic into a
// single function (called by main and by the OpenAPI-drift contract test) keeps
// production route registration and the test's view of it from drifting apart.
type apiHandlers struct {
	account      *account.Handler
	tenancy      *tenancy.Handler
	authz        *authz.Handler
	invitations  *invitations.Handler
	ticketing    *ticketing.Handler
	identity     *ticketing.IdentityHandler
	inboxWebhook *inbox.Handler
	bounce       *inbox.BounceHandler

	// Group-level middleware. Each gates a route group exactly as main wires it:
	// authLimit (per-IP auth abuse cap), ingestLimit (per-IP inbound ingest cap),
	// ticketsRead (tickets.read permission gate for the US1 ticketing read slice),
	// ticketsReply (tickets.reply gate for the US2 reply + note write slice),
	// ticketsWrite (tickets.write gate for the US3 triage PATCH slice),
	// ticketsAssign (tickets.assign gate for the assignee-picker list endpoint).
	authLimit     func(http.Handler) http.Handler
	ingestLimit   func(http.Handler) http.Handler
	ticketsRead   func(http.Handler) http.Handler
	ticketsReply  func(http.Handler) http.Handler
	ticketsWrite  func(http.Handler) http.Handler
	ticketsAssign func(http.Handler) http.Handler
	// inboxManage gates the US4 inbox-management slice (email-domain + inbound-address
	// CRUD) on the inbox.manage permission, same RLS-bound 404-on-lacking-perm shape.
	inboxManage func(http.Handler) http.Handler
}

// mountAPIRoutes registers every /api/v1 route onto mux. It is the single source of
// truth for the route table, shared by main (runtime) and the OpenAPI-drift test
// (which passes zero-value handlers + no-op middleware to enumerate the routes).
func mountAPIRoutes(mux chi.Router, h apiHandlers) {
	mux.Route("/api/v1", func(r chi.Router) {
		r.Group(func(pub chi.Router) {
			pub.Use(h.authLimit)
			h.account.PublicRoutes(pub)
		})
		// Inbound provider webhook (US1): public, authenticated by the per-provider
		// HMAC signature, NOT by JWT and NOT the per-IP auth limiter. Its own public
		// group carries the per-IP INGEST limiter (trusted-proxy-aware client IP, so a
		// spoofed X-Forwarded-For can't evade it); the per-recipient cap is enforced
		// inside the handler before resolution (no existence oracle). T032/FR-020.
		r.Group(func(ingress chi.Router) {
			ingress.Use(h.ingestLimit)
			h.inboxWebhook.PublicRoutes(ingress)
			// Hard-bounce intake (T040): same public, HMAC-authed, per-IP ingest-rate-
			// limited group. Its own purpose-separated secret + uniform no-oracle 202.
			h.bounce.PublicRoutes(ingress)
		})
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			h.account.ProtectedRoutes(pr)
			h.tenancy.ProtectedRoutes(pr)
			h.authz.ProtectedRoutes(pr)
			h.invitations.ProtectedRoutes(pr)
			// US1 ticketing read slice: every endpoint gated on tickets.read at the
			// {id} business. RequirePermission resolves under the caller's RLS
			// principal and 404s (never 403) on a lacking perm — so an outsider sees
			// the same not-found as for a business that does not exist.
			pr.Group(func(tk chi.Router) {
				tk.Use(h.ticketsRead)
				h.ticketing.ProtectedRoutes(tk)
			})
			// US2 ticketing write slice: reply + note, both gated on tickets.reply
			// (the migration-0015 catalog: "Send replies AND internal notes on a
			// ticket"). Same RLS-bound 404-on-lacking-perm semantics as the read group.
			pr.Group(func(tw chi.Router) {
				tw.Use(h.ticketsReply)
				h.ticketing.WriteRoutes(tw)
			})
			// US3 ticketing triage slice: PATCH status/priority/tags/assignee, gated on
			// tickets.write (migration-0015 catalog). Same RLS-bound 404-on-lacking-perm
			// semantics as the read/write groups.
			pr.Group(func(tw2 chi.Router) {
				tw2.Use(h.ticketsWrite)
				h.ticketing.TriageRoutes(tw2)
			})
			// Assignee-picker slice: list a business's assignable members, gated on
			// tickets.assign (the same permission the triage assignee write checks).
			// Same RLS-bound 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(ta chi.Router) {
				ta.Use(h.ticketsAssign)
				h.ticketing.AssignableRoutes(ta)
			})
			// US4 inbox-management slice: custom email-domain create/list/verify +
			// custom inbound-address create/list, all gated on inbox.manage (migration-0015
			// catalog). Same RLS-bound 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(im chi.Router) {
				im.Use(h.inboxManage)
				h.identity.Routes(im)
			})
		})
	})
}

// dkimConfigFromCfg builds the optional system-domain DKIM signer. It returns
// (nil, nil) UNLESS all three of domain, selector, and a private key are configured
// — the locked default is unsigned mail, which must always work. When a key IS
// configured but cannot be parsed it returns an error so startup fails loudly rather
// than silently sending unsigned (a deliverability/spoofing hazard). The PEM is
// parsed as PKCS#8 (ed25519) first, then PKCS#1 (RSA) as a fallback.
func dkimConfigFromCfg(cfg config.Config) (*notify.DKIMConfig, error) {
	if cfg.SystemDKIMDomain == "" || cfg.SystemDKIMSelector == "" || cfg.SystemDKIMPrivateKeyPEM == "" {
		return nil, nil
	}
	block, _ := pem.Decode([]byte(cfg.SystemDKIMPrivateKeyPEM))
	if block == nil {
		return nil, errors.New("DKIM private key: no PEM block found")
	}
	var signer crypto.Signer
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		s, ok := key.(crypto.Signer)
		if !ok {
			return nil, errors.New("DKIM private key: PKCS#8 key is not a crypto.Signer")
		}
		signer = s
	} else if rsaKey, rerr := x509.ParsePKCS1PrivateKey(block.Bytes); rerr == nil {
		signer = rsaKey
	} else {
		return nil, errors.New("DKIM private key: unsupported PEM (want PKCS#8 ed25519 or PKCS#1 RSA)")
	}
	return &notify.DKIMConfig{
		Domain:     cfg.SystemDKIMDomain,
		Selector:   cfg.SystemDKIMSelector,
		PrivateKey: signer,
	}, nil
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
