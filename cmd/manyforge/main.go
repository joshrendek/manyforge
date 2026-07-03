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
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/agents/coding"
	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/jira"
	"github.com/manyforge/manyforge/internal/connectors/zendesk"
	"github.com/manyforge/manyforge/internal/crm"
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
	"github.com/manyforge/manyforge/internal/platform/mcp"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
	"github.com/manyforge/manyforge/internal/platform/secrets"
	"github.com/manyforge/manyforge/internal/tenancy"
	"github.com/manyforge/manyforge/internal/ticketing"
	"github.com/manyforge/manyforge/migrations"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func main() {
	logger := observability.NewLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)
	metrics := observability.NewMetrics()

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

	// Fail fast on schema drift: refuse to serve a database that is behind the code. The
	// expected version is the highest embedded migration; a behind/dirty DB would otherwise
	// 500 opaquely at query time on a column/table a pending migration adds (run migrate).
	expectedSchema, err := migrations.LatestVersion()
	if err != nil {
		logger.Error("startup: determine expected schema version", "err", err)
		os.Exit(1)
	}
	if err := database.VerifySchemaCurrent(ctx, expectedSchema); err != nil {
		logger.Error("startup: refusing to serve (database schema drift)", "err", err, "expected_version", expectedSchema)
		os.Exit(1)
	}

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
	// Spec 005 Phase B: the ticketing service threads CRM timeline entries (status
	// change, reply, note) onto its own WithPrincipal tx — atomic with the mutation.
	crmActivity := &crm.ActivityService{DB: database}
	ticketSvc := &ticketing.Service{
		DB:              database,
		ReplyTokenKey:   cfg.InboundReplyTokenSecret,
		SystemDomain:    cfg.InboundSystemDomain,
		OutboundLimiter: outboundLimiter,
		Suppression:     notify.DBSuppression{DB: database},
		Activity:        crmActivity,
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

	// Spec 005 CRM: tenant-wide contacts + companies. Both services run inside
	// db.WithPrincipal (RLS) and push the tenant_root_id ownership predicate into SQL.
	// The handler carries db+permResolve for symmetry with ticketing; CRM has no
	// conditional handler-side gate (read/write groups are gated by crm.read/crm.write).
	crmContacts := &crm.ContactService{DB: database}
	crmCompanies := &crm.CompanyService{DB: database}
	crmH := crm.NewHandler(crmContacts, crmCompanies, crmActivity, database, permResolve)

	// US2 agent-runtime: agent definition CRUD. Each Create also mints the agent's
	// kind='agent' principal (its acting identity). Gated by agents.configure
	// (migration-0027 catalog), same RLS-bound 404-on-lacking-perm shape as other groups.
	agentSvc := &agents.AgentService{DB: database}
	agentH := agents.NewHandler(agentSvc)

	// US3 agent-runtime: the run loop. The engine acts AS the agent principal; manual
	// trigger + run status are gated by agents.run. Cost uses the AI model registry,
	// keyed on the resolved model id. The credential service resolves the agent's BYO
	// provider key into an SSRF-guarded transport at run start.
	// Agent BYO-credential sealing. Optional: with no AI master key the credential
	// HTTP surface is disabled (handler left nil, like connectors), and the run
	// engine cannot resolve BYO keys. Set MANYFORGE_AI_MASTER_KEY where credentials are used.
	var aiSealer *mfcrypto.Sealer
	if len(cfg.AIMasterKey) > 0 {
		s, serr := mfcrypto.NewSealer(cfg.AIMasterKey)
		if serr != nil {
			logger.Error("init AI credential sealer", "err", serr)
			os.Exit(1)
		}
		aiSealer = s
	} else {
		logger.Warn("MANYFORGE_AI_MASTER_KEY unset; AI provider credential sealing disabled (BYO key creation/resolution will fail)")
	}
	credSvc := &agents.CredentialService{DB: database, Sealer: aiSealer}
	// Credential HTTP surface is only mounted when the AI sealer is configured —
	// without it, Create would return a config error. Mirrors the connectors nil-guard.
	var credH *agents.CredentialHandler
	if aiSealer != nil {
		credH = agents.NewCredentialHandler(credSvc)
	}
	aiReg, err := agents.LoadModelRegistry(ctx, database)
	if err != nil {
		logger.Error("load model registry", "err", err)
		os.Exit(1)
	}
	agentRunStore := &agents.AgentRunStore{DB: database}
	accountingStore := &agents.AccountingStore{DB: database}
	accountingH := agents.NewAccountingHandler(accountingStore)
	// connGateway is declared here (interface-typed, not *connectors.AgentGateway) so it
	// is a TRUE nil interface until the connector block below assigns it. This avoids the
	// typed-nil trap: a (*connectors.AgentGateway)(nil) boxed into ConnectorGateway would
	// be non-nil at the interface level and incorrectly register connector tools.
	var connGateway agents.ConnectorGateway // nil interface = connectors disabled
	agentEngine := &agents.Engine{
		Runs:        agentRunStore,
		Tools:       agents.NewToolRegistry(ticketSvc, connGateway),
		Auditor:     agents.NewDBAuditor(database),
		Resolver:    agents.NewAuthzChecker(database),
		NewProvider: agents.NewCredentialProviderFactory(credSvc),
		Cost:        agents.NewRegistryCostFn(aiReg, logger),
		Limits: agents.RunLimits{ // config keys (MANYFORGE_AGENT_*), defaulting to the code defaults
			MaxIterations:   cfg.AgentMaxIterations,
			MaxTokensPerRun: cfg.AgentMaxTokensPerRun,
			MaxOutputTokens: cfg.AgentMaxOutputTokens,
			WallClock:       cfg.AgentWallClock,
			Temperature:     cfg.AgentTemperature,
		},
	}
	agentRunSvc := agents.NewRunService(agentSvc, agentEngine, agentRunStore)
	agentRunH := agents.NewRunHandler(agentRunSvc)

	// US4 approvals queue: the gate writes pending approval_items via this store, a human
	// with agents.approve decides them, and an approved action is dispatched (via the
	// existing outbox/bus) to the ApprovalExecutor, which re-invokes the approved tool
	// through the SAME ticketing-backed tool registry as the Engine, as the agent principal.
	approvalStore := &agents.ApprovalStore{DB: database}
	agentEngine.Approvals = approvalStore // wire the gate's approval writer
	approvalSvc := agents.NewApprovalService(approvalStore)
	approvalH := agents.NewApprovalHandler(approvalSvc)
	approvalExec := &agents.ApprovalExecutor{
		Approvals: approvalStore,
		Tools:     agents.NewToolRegistry(ticketSvc, connGateway), // reuse the same ticketing service as the Engine
		Auditor:   agents.NewDBAuditor(database),
	}
	// approvalExec subscribes to TopicAgentApproved below, once eventBus is constructed.

	// US5 triage trigger + run drainer (l29 async path). The trigger subscribes to
	// ticket.created (below) and enqueues a queued run per enabled agent — fast +
	// idempotent, it does NOT run the loop. The drainer poller (started after the worker)
	// claims queued runs and runs the loop as the agent, decoupled from the outbox worker
	// so a long run never stalls event delivery.
	triageTrigger := &agents.TriageTrigger{Runs: agentRunStore, Logger: logger}
	replyRetriageTrigger := &agents.ReplyRetriageTrigger{Runs: agentRunStore, RetriageCap: cfg.AgentRetriageCapPerHour, Logger: logger}
	runDrainer := &agents.RunDrainer{Runs: agentRunStore, Engine: agentEngine, Logger: logger}

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

	// US6 MCP server sealer: seals per-server bearer tokens at rest using a dedicated
	// master key (MANYFORGE_MCP_MASTER_KEY), mirroring EXACTLY the DKIM sealer pattern.
	// When unset, mcpSealer is nil and creating an MCP server WITH an auth token returns
	// a clean ErrValidation (the service already handles nil-sealer + token); servers
	// without auth can still be created and the binary still boots.
	var mcpSealer *mfcrypto.Sealer
	if len(cfg.MCPMasterKey) > 0 {
		s, serr := mfcrypto.NewSealer(cfg.MCPMasterKey)
		if serr != nil {
			logger.Error("init MCP sealer", "err", serr)
			os.Exit(1)
		}
		mcpSealer = s
	} else {
		logger.Warn("MANYFORGE_MCP_MASTER_KEY unset; MCP server bearer-token sealing disabled (auth-token creation will fail)")
	}
	mcpServerSvc := &agents.MCPServerService{DB: database, Sealer: mcpSealer}
	mcpPolicySvc := &agents.MCPToolPolicyService{DB: database} // manyforge-k0d: per-tool effect policy

	// US6 MCP host wiring. The guarded HTTP client honours MCPAllowLoopback so that
	// dev environments may point at localhost MCP servers while production keeps the
	// default SSRF posture (loopback blocked). The ClientFactory wraps mcp.NewClient so
	// the host and executor receive an interface value, keeping them transport-agnostic.
	// agentSvc.MCPServers is assigned AFTER agentSvc is constructed because mcpServerSvc
	// is built after agentSvc; this is the single safe wiring point for the validator.
	mcpHTTP := netsafe.NewClientWithOptions(60*time.Second, netsafe.Options{AllowLoopback: cfg.MCPAllowLoopback})
	mcpConnect := mcp.ClientFactory(func(serverURL, authHeader string) mcp.ClientLike {
		return mcp.NewClient(serverURL, authHeader, mcpHTTP)
	})
	mcpHost := &agents.MCPHost{Servers: mcpServerSvc, Policies: mcpPolicySvc, Connect: mcpConnect, Logger: logger}
	// manyforge-k0d: the policy handler discovers tools via mcpHost and CRUDs via mcpPolicySvc;
	// it nests under /mcp_servers/{serverID} (sharing the agents.configure gate).
	mcpPolicyH := agents.NewMCPToolPolicyHandler(mcpPolicySvc, mcpHost)
	mcpH := agents.NewMCPServerHandler(mcpServerSvc, mcpPolicyH)
	agentEngine.MCP = mcpHost
	approvalExec.MCP = mcpHost
	// security carry-forward (Task 7): wire the validator so allowed_mcp_servers ids are
	// validated on agent create/update in production (cross-tenant/foreign ids rejected).
	agentSvc.MCPServers = mcpServerSvc

	// Spec 004 connector stack (US3 Jira inbound). Mirrors the MCP sealer pattern:
	// unset key ⇒ connector stack disabled (warn, no fatal); the server still boots.
	// An explicitly-set-but-wrong-length key is a hard config error caught at Load().
	var connReg *connectors.Registry
	var connWebhookH *connectors.WebhookHandler
	var connManageH *connectors.Handler
	var inboundSyncSub *connectors.InboundSyncSubscriber
	var connReconciler *connectors.Reconciler
	var outboundDispatcher *connectors.OutboundDispatcher
	if len(cfg.ConnectorMasterKey) > 0 {
		connSealer, serr := mfcrypto.NewSealer(cfg.ConnectorMasterKey)
		if serr != nil {
			logger.Error("init connector sealer", "err", serr)
			os.Exit(1)
		}
		connSvc := &connectors.Service{DB: database, Vault: secrets.NewVault(connSealer)}
		connReg = connectors.NewRegistry(connSvc)
		connReg.Register("jira", jira.NewFactory(60*time.Second))
		connReg.Register("zendesk", zendesk.NewFactory(60*time.Second))
		// Live credential verifier (registry-backed): makes create, credential rotation, and
		// the connector Test action perform a real auth probe instead of "verification
		// unavailable" (manyforge — connector Test verifier).
		connSvc.Verify = connectors.NewRegistryVerifier(connReg)
		connWebhookH = connectors.NewWebhookHandler(database, connSealer, connReg, logger)
		connManageH = connectors.NewHandler(connSvc)
		inboundSyncSub = &connectors.InboundSyncSubscriber{DB: database, Sealer: connSealer, Registry: connReg, Logger: logger}
		// StaleAfter is the per-connector minimum re-poll interval; Every is just the scan
		// tick. At StaleAfter=1m a new external issue auto-syncs within ~1–2m on the polling
		// path (vs ~5–6m before). Webhooks (connWebhookH) remain the real-time path in prod;
		// polling is the backstop, so this mainly helps webhook-less environments (e.g. a
		// localhost dev where Jira Cloud can't reach the webhook endpoint). Lower ⇒ more
		// external-API calls per connector. (manyforge-7kf)
		connReconciler = &connectors.Reconciler{DB: database, Sealer: connSealer, Registry: connReg, Logger: logger, Every: time.Minute, StaleAfter: time.Minute}
		connManageH.SetSyncTrigger(connReconciler) // wire the "Sync now" endpoint to the reconciler
		outboundDispatcher = &connectors.OutboundDispatcher{DB: database, Sealer: connSealer, Registry: connReg, Logger: logger, Every: 15 * time.Second, Batch: 20, StaleAfter: 5 * time.Minute}
		// Assign the gateway and rebuild the tool registry for both the Engine and
		// the ApprovalExecutor so they pick up the connector tools. This mirrors the
		// late-wiring pattern used for MCP (agentEngine.MCP = mcpHost below).
		connGateway = connectors.NewAgentGateway(connSvc, connReg)
		agentEngine.Tools = agents.NewToolRegistry(ticketSvc, connGateway)
		approvalExec.Tools = agents.NewToolRegistry(ticketSvc, connGateway)
	} else {
		logger.Warn("MANYFORGE_CONNECTOR_MASTER_KEY unset; external connectors disabled (no Jira webhook/sync)")
	}

	// Spec 007 code-review sandbox (slice 1). RepoConnectorService uses the same
	// connector sealer vault as the connector stack when available; falls back to a
	// nil vault when MANYFORGE_CONNECTOR_MASTER_KEY is unset (repo-connector creation
	// will fail at runtime, but the server still boots for environments that don't
	// need code review). The egress infra setup is non-fatal: a missing Docker daemon
	// (CI / no-Docker dev) logs a warning and coding reviews fail at run time only.
	var codingH *coding.Handler
	var codingSvc *coding.CodeReviewService
	// Shared live OpenRouter catalog: models for the agent form's typeahead AND
	// pricing for code-review cost accounting. Fetched via the SSRF-safe netsafe
	// client; cached in-process.
	orModels := &agents.OpenRouterModels{HTTP: netsafe.NewClient(15 * time.Second)}
	{
		var connVault *secrets.Vault
		if len(cfg.ConnectorMasterKey) > 0 {
			connSealer, serr := mfcrypto.NewSealer(cfg.ConnectorMasterKey)
			if serr != nil {
				// Already validated in the connector block above; won't happen here.
				logger.Error("init connector sealer (coding)", "err", serr)
				os.Exit(1)
			}
			connVault = secrets.NewVault(connSealer)
		}
		repoSvc := &connectors.RepoConnectorService{DB: database, Vault: connVault}
		egressAllow := strings.Split(cfg.SandboxEgressAllow, ",")
		if err := sandbox.EnsureEgressInfra(ctx, cfg.EgressProxyImage, egressAllow); err != nil {
			logger.Warn("sandbox egress infra unavailable; coding reviews will fail at run time", "err", err)
		}
		codingSvc = &coding.CodeReviewService{
			DB:           database,
			Repos:        repoSvc,
			Sandbox:      sandbox.NewDockerRunner(sandbox.NetworkName, sandbox.ProxyDNSAddr),
			Creds:        &coding.AgentCredResolver{Agents: agentSvc, Credentials: credSvc},
			Image:        cfg.SandboxImage,
			WorkRoot:     cfg.SandboxWorkRoot,
			Timeout:      8 * time.Minute, // heavy uncached agentic lanes (300k+ input tokens) exceed 5m (manyforge-2s1)
			LocalTimeout: cfg.LocalReviewTimeout,
			// Same allowlist that boots the egress proxy above — Trigger validates the
			// run's provider host against it up front (manyforge-0qj).
			EgressAllow: netsafe.ParseHostAllowlist(cfg.SandboxEgressAllow),
			Pricing:     orModels,
		}
		codingH = &coding.Handler{
			RepoSvc:      repoSvc,
			ReviewSvc:    codingSvc,
			ReviewDimSvc: &coding.ReviewDimensionService{DB: database},
		}
	}

	// Late-wire the agent handler's read-only metadata endpoints (/agents/tools,
	// /agents/models). The registry is built from the same inputs as the engine's
	// (ticketSvc + the resolved connGateway); only descriptors are read, never
	// invoked. The model catalog reads model_pricing via WithTx (no RLS).
	agentH.SetMetadata(
		agents.NewToolRegistry(ticketSvc, connGateway),
		&agents.ModelCatalog{DB: database},
	)
	// Live per-provider model catalog (OpenRouter) for the agent form's typeahead.
	// Fetched through the SSRF-safe netsafe client; cached in-process.
	agentH.SetProviderModels(orModels)

	// SL-C event bus + transactional-outbox worker. Support-desk services
	// (US1/US2) register their subscribers on eventBus before the worker starts,
	// so no event is drained without a handler. The in-process SMTP receiver
	// (cfg.SMTPAddr) and the inbox/ticketing routes are wired with their adapters
	// and handlers in US1.
	eventBus := events.NewBus()
	outboxWorker := &events.Worker{DB: database, Bus: eventBus, Logger: logger, Metrics: metrics}

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
	inboxWebhookH.Metrics = metrics
	// US2 hard-bounce intake (T040): a provider-signed (separate InboundBounceSecret)
	// webhook that suppresses the bounced recipient (global email_suppression) and
	// marks the correlated outbound message failed via a DEFINER. Mounted next to the
	// inbound webhook in the same per-IP ingest-rate-limited public group; no JWT.
	bounceH := inbox.NewBounceHandler(inbox.NewDBBounceSuppressor(database), cfg.InboundBounceSecret, cfg.InboundMaxBytes, logger)
	bounceH.Metrics = metrics

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
	sendSub := notify.SendSubscriber{Sender: sender, Logger: logger, Sealer: dkimSealer, Metrics: metrics}
	eventBus.Subscribe(events.TopicTicketReplied, sendSub.Handle)

	// US5 redact: the attachment.purge worker deletes redacted attachment blobs out-of-
	// band (the redact tx enqueues one event per blob). The handler is idempotent and
	// touches no RLS tables, so the worker's principal-less tx is fine.
	purgeSub := ticketing.AttachmentPurgeSubscriber{Blob: blobStore}
	eventBus.Subscribe(events.TopicAttachmentPurge, purgeSub.Handle)

	// US4 approvals: an approved gated action is enqueued on TopicAgentApproved (in the
	// approve tx) and dispatched by the outbox worker to the ApprovalExecutor, which
	// re-invokes the approved tool as the agent principal (exactly-once via the approval
	// state claim + the draft_reply idempotency key).
	eventBus.Subscribe(agents.TopicAgentApproved, approvalExec.Handle)

	// US5: an enabled agent auto-runs on a brand-new ticket. ONLY ticket.created is
	// subscribed (never message.received) — the structural loop-guard: an agent's own
	// reply emits ticket.replied, not ticket.created, so it can never re-trigger triage.
	eventBus.Subscribe(events.TopicTicketCreated, triageTrigger.Handle)

	// US5 follow-up (manyforge-deo.1): an OPTED-IN agent re-runs when a customer replies to
	// an existing ticket. message.received also fires for a new ticket's first message, but
	// that shares the ticket.created run's dedup key, so a fresh ticket still runs once.
	// Guarded in the enqueue_reply_retriage_run DEFINER (inbound-only + per-ticket/agent cap).
	eventBus.Subscribe(events.TopicMessageReceived, replyRetriageTrigger.Handle)

	// Spec 004 inbound-sync subscriber: fetch external issue + DEFINER upsert.
	// Registered BEFORE the outbox worker starts so no connector.inbound.sync event
	// is drained without a handler. Guard mirrors the other conditional subscribers.
	if inboundSyncSub != nil {
		eventBus.Subscribe(connectors.TopicConnectorInboundSync, inboundSyncSub.Handle)
	}

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
		account:          acctH,
		tenancy:          tenH,
		authz:            authzH,
		invitations:      invH,
		ticketing:        ticketH,
		identity:         identityH,
		inboxWebhook:     inboxWebhookH,
		bounce:           bounceH,
		authLimit:        httpx.RateLimit(authLimiter, ipKey),
		ingestLimit:      httpx.RateLimit(ingestIPLimiter, ingestIPKey),
		ticketsRead:      httpx.RequirePermission(database, permResolve, authz.PermTicketsRead, businessIDFromPath),
		ticketsReply:     httpx.RequirePermission(database, permResolve, authz.PermTicketsReply, businessIDFromPath),
		ticketsWrite:     httpx.RequirePermission(database, permResolve, authz.PermTicketsWrite, businessIDFromPath),
		ticketsAssign:    httpx.RequirePermission(database, permResolve, authz.PermTicketsAssign, businessIDFromPath),
		ticketsDelete:    httpx.RequirePermission(database, permResolve, authz.PermTicketsDelete, businessIDFromPath),
		inboxManage:      httpx.RequirePermission(database, permResolve, authz.PermInboxManage, businessIDFromPath),
		agents:           agentH,
		credentials:      credH,
		agentsConfigure:  httpx.RequirePermission(database, permResolve, authz.PermAgentsConfigure, businessIDFromPath),
		agentRuns:        agentRunH,
		agentsRun:        httpx.RequirePermission(database, permResolve, authz.PermAgentsRun, businessIDFromPath),
		accounting:       accountingH,
		approvals:        approvalH,
		agentsApprove:    httpx.RequirePermission(database, permResolve, authz.PermAgentsApprove, businessIDFromPath),
		mcp:              mcpH,
		mcpConfigure:     httpx.RequirePermission(database, permResolve, authz.PermAgentsConfigure, businessIDFromPath),
		connWebhookH:     connWebhookH,
		connectors:       connManageH,
		connectorsManage: httpx.RequirePermission(database, permResolve, authz.PermConnectorsManage, businessIDFromPath),
		crm:              crmH,
		crmRead:          httpx.RequirePermission(database, permResolve, authz.PermCRMRead, businessIDFromPath),
		crmWrite:         httpx.RequirePermission(database, permResolve, authz.PermCRMWrite, businessIDFromPath),
		codingReviews:    codingH,
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

	// Spec 007 code-review worker: polls the code_review queue and runs pending jobs
	// as the owning principal (RLS re-entered inside runJob). The claim/requeue/fail
	// queries run cross-tenant WITHOUT RLS via AppDBAdapter.WithTx — the same
	// principal-less path the outbox worker uses for system operations.
	// NOTE: ClaimCodeReviews' lease-expiry reclaim IS the boot reconcile — rows
	// whose lease expired while the server was down are automatically reclaimed on
	// the next tick, so no separate startup sweep is needed.
	crWorker := &coding.CodeReviewWorker{
		DB:     &coding.AppDBAdapter{DB: database},
		Svc:    codingSvc,
		Logger: logger,
	}
	go crWorker.Run(workerCtx)

	// Spec 004 reconcile poller: periodically lists connectors past their stale window
	// and enqueues connector.inbound.sync events for externally-updated issues.
	// Started AFTER the outbox worker so the subscriber is registered before the first
	// tick could enqueue events. The guard mirrors the approval-expire sweep pattern.
	if connReconciler != nil {
		go connReconciler.Run(workerCtx)
	}

	// Spec 004 US4 outbound dispatcher: drains connector_outbound_op, posting native
	// replies as external comments + creating external issues, writing external ids back.
	if outboundDispatcher != nil {
		go outboundDispatcher.Run(workerCtx)
	}

	// US4 approvals expire sweep: every 60s, expire stale pending approval_items across
	// ALL tenants via the SECURITY DEFINER expire_stale_approvals() function (migration
	// 0032). Runs on the principal-less WithTx tx — the same tx the outbox worker uses —
	// because the sweep is system-wide and has no per-tenant principal; the definer
	// function (owner-defined, RLS-exempt) is what makes the cross-tenant UPDATE possible.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				var n int64
				err := database.WithTx(workerCtx, func(tx pgx.Tx) error {
					return tx.QueryRow(workerCtx, "SELECT expire_stale_approvals()").Scan(&n)
				})
				if err != nil {
					logger.WarnContext(workerCtx, "approval expire sweep", "err", err)
				} else if n > 0 {
					logger.InfoContext(workerCtx, "approval items expired", "count", n)
				}
			}
		}
	}()

	// US5 run drainer: every 2s, drain all queued agent_runs (claim queued→running via the
	// SKIP-LOCKED definer fn, then run the loop as the agent). Serial per tick for v1; the
	// SKIP-LOCKED claim already supports horizontal scaling if we add workers later. The
	// loop is decoupled from the outbox worker so a long run never stalls event delivery.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				for {
					ran, err := runDrainer.DrainOnce(workerCtx)
					if err != nil {
						logger.WarnContext(workerCtx, "agent run drain", "err", err)
						break
					}
					if !ran {
						break
					}
				}
			}
		}
	}()

	// Agent-run reaper (67i): a run is set 'running' then executes a loop capped at 120s; if the
	// worker goroutine dies (restart/crash) the row is stuck 'running' forever and any "agent
	// working" indicator would lie. Reap, every 2m, runs whose updated_at is older than 10m (well
	// above the 120s cap, so a live run is never reaped). Staleness-based — not a startup
	// reap-all — so it stays correct under the horizontal scaling the drainer already anticipates.
	agentReaper := &agents.Reaper{DB: database, Logger: logger, Every: 2 * time.Minute, StaleAfter: 10 * time.Minute}
	go agentReaper.Run(workerCtx)

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
	// connWebhookH is the Spec 004 public connector webhook handler. Nil when
	// MANYFORGE_CONNECTOR_MASTER_KEY is unset (connectors disabled); mountAPIRoutes
	// guards on nil so the route is not registered in that case.
	connWebhookH *connectors.WebhookHandler

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
	// ticketsDelete gates the US5 delete/redact slice (DELETE a ticket → soft-delete/
	// redact-in-place) on the tickets.delete permission, same RLS-bound 404 shape.
	ticketsDelete func(http.Handler) http.Handler
	// inboxManage gates the US4 inbox-management slice (email-domain + inbound-address
	// CRUD) on the inbox.manage permission, same RLS-bound 404-on-lacking-perm shape.
	inboxManage func(http.Handler) http.Handler

	// agents is the US2 agent-definition CRUD handler (Spec 003).
	agents *agents.Handler
	// credentials is the AI-provider credential CRUD handler (Phase 1 of the
	// agent-management UI). Nil when MANYFORGE_AI_MASTER_KEY is unset; the mount
	// block guards on nil so the route group is not registered in that case.
	credentials *agents.CredentialHandler
	// agentsConfigure gates the US2 agent-definition CRUD slice on the
	// agents.configure permission, same RLS-bound 404-on-lacking-perm shape as the
	// other groups.
	agentsConfigure func(http.Handler) http.Handler

	// connectors is the Spec 004 connector-management CRUD handler. Nil when
	// MANYFORGE_CONNECTOR_MASTER_KEY is unset (connectors disabled); mountAPIRoutes
	// guards on nil so the route group is not registered in that case.
	connectors *connectors.Handler
	// connectorsManage gates the connector-management CRUD slice on the
	// connectors.manage permission (migration-0048 catalog). Same RLS-bound
	// 404-on-lacking-perm semantics as the other groups.
	connectorsManage func(http.Handler) http.Handler

	// agentRuns is the US3 run trigger/status handler (Spec 003): POST a manual run
	// (202) and GET its status (200) under a business's agent.
	agentRuns *agents.RunHandler
	// agentsRun gates the US3 run slice on the agents.run permission, same RLS-bound
	// 404-on-lacking-perm shape as the other groups.
	agentsRun func(http.Handler) http.Handler

	// accounting is the US7 accounting summary handler (Spec 003): GET per-agent
	// token/cost rollup for a business over a window, gated by agents.run.
	accounting *agents.AccountingHandler

	// approvals is the US4 approvals-queue handler (Spec 003): list/approve/deny.
	approvals *agents.ApprovalHandler
	// agentsApprove gates the US4 approvals slice on the agents.approve permission, same
	// RLS-bound 404-on-lacking-perm shape as the other groups.
	agentsApprove func(http.Handler) http.Handler

	// mcp is the US6 MCP server CRUD handler (Spec 003): create/list/get/patch/delete
	// MCP server connection records for a business, gated by agents.configure.
	mcp *agents.MCPServerHandler
	// mcpConfigure gates the US6 MCP server slice on the agents.configure permission
	// (same gate as the agent-definition CRUD — no new permission needed).
	mcpConfigure func(http.Handler) http.Handler

	// crm is the Spec 005 CRM handler: tenant-wide contacts + companies CRUD + contact
	// merge under a business.
	crm *crm.Handler
	// crmRead / crmWrite gate the CRM read slice (crm.read) and write slice (crm.write)
	// respectively (migration catalog), same RLS-bound 404-on-lacking-perm semantics as
	// the other groups.
	crmRead  func(http.Handler) http.Handler
	crmWrite func(http.Handler) http.Handler

	// codingReviews is the Spec 007 code-review handler: repo-connector creation gated
	// by connectorsManage (connectors.manage), code-review trigger/get gated by
	// agentsRun (agents.run). Same RLS-bound 404-on-lacking-perm semantics.
	codingReviews *coding.Handler
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
			// Spec 004 connector webhook: public, per-connector HMAC-authed, per-IP
			// ingest-rate-limited. Guard on nil: when MANYFORGE_CONNECTOR_MASTER_KEY is
			// unset the handler is nil and the route is simply not registered.
			if h.connWebhookH != nil {
				h.connWebhookH.PublicRoutes(ingress)
			}
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
			// US5 delete/redact slice: DELETE a ticket → soft-delete/redact-in-place,
			// gated on tickets.delete (migration-0015 catalog). Same RLS-bound
			// 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(td chi.Router) {
				td.Use(h.ticketsDelete)
				h.ticketing.DeleteRoutes(td)
			})
			// US4 inbox-management slice: custom email-domain create/list/verify +
			// custom inbound-address create/list, all gated on inbox.manage (migration-0015
			// catalog). Same RLS-bound 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(im chi.Router) {
				im.Use(h.inboxManage)
				h.identity.Routes(im)
			})
			// US2 agent-definition slice: CRUD agents under a business, gated on
			// agents.configure (migration-0027 catalog). Same RLS-bound
			// 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(ag chi.Router) {
				ag.Use(h.agentsConfigure)
				h.agents.ProtectedRoutes(ag)
			})
			// AI-provider credential slice: create/list/delete BYO provider keys,
			// gated on agents.configure (same permission as agent-definition CRUD).
			if h.credentials != nil {
				pr.Group(func(cg chi.Router) {
					cg.Use(h.agentsConfigure)
					h.credentials.ProtectedRoutes(cg)
				})
			}
			// US3 agent-run slice: manual trigger + run status under a business's agent,
			// gated on agents.run (migration-0029 catalog). The engine runs AS the agent
			// principal; same RLS-bound 404-on-lacking-perm semantics as the other groups.
			// US7 accounting summary is co-gated by agents.run (same permission).
			pr.Group(func(ag chi.Router) {
				ag.Use(h.agentsRun)
				h.agentRuns.ProtectedRoutes(ag)
				h.accounting.ProtectedRoutes(ag)
			})
			// US4 approvals slice: a human works a business's flat approvals queue
			// (list/approve/deny), gated on agents.approve (migration-0031 catalog;
			// human-only, never granted to agent_runtime). Same RLS-bound
			// 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(ap chi.Router) {
				ap.Use(h.agentsApprove)
				h.approvals.ProtectedRoutes(ap)
			})
			// US6 MCP server slice: CRUD MCP server connection records under a business,
			// gated on agents.configure (same permission as agent-definition CRUD —
			// configuring MCP servers is part of configuring the agent runtime).
			pr.Group(func(mc chi.Router) {
				mc.Use(h.mcpConfigure)
				h.mcp.ProtectedRoutes(mc)
			})
			// Connectors management slice: CRUD external connectors under a business, gated on
			// connectors.manage (migration-0048 catalog). Same RLS-bound 404-on-lacking-perm semantics.
			// Guard on nil: when MANYFORGE_CONNECTOR_MASTER_KEY is unset the handler is nil and the
			// route group is simply not registered.
			if h.connectors != nil {
				pr.Group(func(cg chi.Router) {
					cg.Use(h.connectorsManage)
					h.connectors.ProtectedRoutes(cg)
				})
			}
			// Spec 005 CRM read slice: contacts + companies list/get, gated on crm.read.
			// Same RLS-bound 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(cr chi.Router) {
				cr.Use(h.crmRead)
				h.crm.ReadRoutes(cr)
			})
			// Spec 005 CRM write slice: contacts + companies create/update/delete +
			// contact merge, gated on crm.write. Same RLS-bound 404-on-lacking-perm semantics.
			pr.Group(func(cw chi.Router) {
				cw.Use(h.crmWrite)
				h.crm.WriteRoutes(cw)
			})
			// Spec 007 code-review slice: repo-connector create gated on connectors.manage,
			// code-review trigger/get gated on agents.run — same RLS-bound 404-on-lacking-perm
			// semantics as the other permission groups. Spec 008 adds the review-panel config
			// surface, gated on agents.configure (configuring the panel is distinct from running
			// a review). Guard on nil so a zero-value apiHandlers (as used by the OpenAPI-drift
			// contract test) does not panic.
			if h.codingReviews != nil {
				pr.Group(func(cg chi.Router) {
					cg.Use(h.connectorsManage)
					h.codingReviews.RepoConnectorRoutes(cg)
				})
				pr.Group(func(ag chi.Router) {
					ag.Use(h.agentsRun)
					h.codingReviews.CodeReviewRoutes(ag)
				})
				pr.Group(func(cf chi.Router) {
					cf.Use(h.agentsConfigure)
					h.codingReviews.ReviewConfigRoutes(cf)
				})
			}
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
