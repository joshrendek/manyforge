// Package config loads runtime configuration from the environment (12-factor).
// Secrets are never hard-coded; see .env.example for the full set.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config holds runtime configuration.
type Config struct {
	Addr             string        // HTTP listen address
	DatabaseURL      string        // PostgreSQL DSN for the app role (non-superuser, non-BYPASSRLS)
	AccessTokenTTL   time.Duration // EdDSA access-token lifetime
	TrustedProxyCIDR string        // CIDR allowed to set the client IP via X-Forwarded-For
	JWTIssuer        string        // expected/stamped token issuer
	JWTAudience      string        // expected/stamped token audience
	JWTActiveKID     string        // kid of the persistent signing key (unset ⇒ ephemeral dev ring)
	JWTSigningKeyPEM string        // PKCS8 Ed25519 private key PEM for persistent JWT signing; inline or via *_PATH
	JWTVerifyKeys    string        // additional verify-only keys for rotation: "kid=<pkix pubkey pem>,..."
	RateLimitRPS     float64       // per-IP token refill rate for abuse-sensitive routes (FR-029)
	RateLimitBurst   float64       // per-IP burst allowance for abuse-sensitive routes

	// Support desk (spec 002).
	SMTPAddr                string  // built-in inbound SMTP receiver listen address; empty disables it
	InboundWebhookSecret    string  // HMAC-SHA256 secret for the provider webhook signature (constant-time verified)
	InboundBounceSecret     string  // HMAC-SHA256 secret for the hard-bounce webhook signature; PURPOSE-SEPARATED from InboundWebhookSecret (a leak of one cannot forge the other)
	InboundReplyTokenSecret []byte  // HMAC key minting/verifying the threading reply token; purpose-separated from the webhook + JWT secrets
	InboundSystemAddrSecret []byte  // HMAC key deriving the unguessable system inbound-address localpart (FR-001); purpose-separated from the reply-token/webhook/JWT secrets
	BlobURL                 string  // attachment object-storage backend (file:///… or s3://…); empty disables attachments
	InboundSystemDomain     string  // platform-hosted domain that auto-provisioned system inbound addresses live on
	DKIMKeyPath             string  // path to the default DKIM private key for verified custom sending identities
	InboundMaxBytes         int64   // max inbound message size (SMTP MaxMessageBytes + webhook body cap), FR-007/FR-020
	AttachmentMaxBytes      int64   // per-attachment size cap, FR-007
	IngestRateRPS           float64 // per-recipient inbound ingestion refill rate (loop/abuse bound, FR-018/FR-020)
	IngestRateBurst         float64 // per-recipient inbound ingestion burst allowance
	OutboundRateRPS         float64 // per-business outbound send refill rate (FR-020)
	OutboundRateBurst       float64 // per-business outbound send burst allowance

	// Outbound SMTP relay (US2/T039). SMTPHost empty ⇒ the notify worker uses the
	// dev LogSender (logs the threaded reply, honors suppression) instead of a real
	// MTA, so reply flows are completable without an outbound relay configured.
	SMTPHost string // outbound relay host; empty ⇒ LogSender
	SMTPPort int    // outbound relay port (default 587)
	SMTPUser string // SMTP AUTH username; empty ⇒ no auth
	SMTPPass string // SMTP AUTH password

	// Optional system-domain DKIM signing (FR-013 deliverability). All THREE must be
	// set to sign; otherwise outbound is sent unsigned (the locked default — DKIM is
	// not required for the system domain in dev / un-provisioned envs).
	SystemDKIMDomain        string // d= domain, e.g. the InboundSystemDomain
	SystemDKIMSelector      string // s= selector
	SystemDKIMPrivateKeyPEM string // PEM-encoded private key (ed25519 PKCS#8 or RSA); inline or via *_PATH

	// At-rest master key for sealing per-domain DKIM private keys (US4 custom
	// sending identities). Supplied via MANYFORGE_DKIM_MASTER_KEY as base64 or
	// hex; the decoded value MUST be 32 bytes (AES-256). Nil/empty when unset —
	// custom-domain signing then degrades to system-only and the server still
	// boots; only an explicitly-set-but-invalid key is a hard config error.
	DKIMMasterKey []byte

	// Agent runtime (US6 MCP host).
	// MCPMasterKey is the at-rest master key for sealing MCP server bearer tokens.
	// Supplied via MANYFORGE_MCP_MASTER_KEY as base64 or hex; the decoded value MUST
	// be 32 bytes (AES-256). Nil/empty when unset — auth-token creation then degrades
	// gracefully (the service returns ErrValidation on attempt); the server still boots
	// and MCP servers without auth can still be created. An explicitly-set-but-wrong-
	// length key is a hard config error caught here.
	MCPMasterKey []byte
	// AIMasterKey is the at-rest master key for sealing agent BYO provider credentials
	// (API keys). Supplied via MANYFORGE_AI_MASTER_KEY as base64 or hex; the decoded
	// value MUST be 32 bytes (AES-256). Nil/empty when unset — the credential HTTP
	// surface is disabled and the run engine cannot resolve BYO keys; the server still
	// boots. An explicitly-set-but-wrong-length key is a hard config error caught here.
	AIMasterKey []byte
	// MCPAllowLoopback permits the outbound MCP HTTP client to dial loopback
	// addresses. Default false (locked-secure); set true only for local dev MCP servers.
	MCPAllowLoopback bool

	// ConnectorMasterKey is the sealed-at-rest master key for connector credentials
	// (Jira, Zendesk, etc.). Supplied via MANYFORGE_CONNECTOR_MASTER_KEY as base64 or
	// hex; the decoded value MUST be 32 bytes (AES-256). Nil/empty when unset — the
	// connector stack (webhook handler, inbound-sync subscriber, reconciler) is disabled
	// and the server still boots. An explicitly-set-but-wrong-length key is a hard config
	// error caught here.
	ConnectorMasterKey []byte

	// GitHubAppMasterKey seals the instance GitHub App private key + client/webhook
	// secrets. MANYFORGE_GITHUB_APP_MASTER_KEY (base64/hex, 32 bytes). Nil when unset —
	// GitHub App integration disabled, server still boots. Set-but-wrong-length is fatal.
	GitHubAppMasterKey []byte

	// InstanceOperatorPrincipal gates instance setup routes (GitHub App manifest
	// creation, etc.). MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL (UUID). uuid.Nil when
	// unset — setup routes reject everyone (404, no oracle). The operator finds
	// their principal id from GET /api/v1/me.
	InstanceOperatorPrincipal uuid.UUID
	// PublicBaseURL is the externally-reachable base (https://hub.example.com) used
	// to build the GitHub App manifest's redirect/callback/webhook URLs.
	// MANYFORGE_PUBLIC_BASE_URL. No existing base-URL config was reusable — the only
	// other *BaseURL/*URL settings in this file are per-connector (Jira/Zendesk) or
	// per-feature (BlobURL), not an instance-wide externally-reachable base.
	PublicBaseURL string

	// Agent run loop bounds (Spec 003 §8, manyforge-ji7). Defaults below mirror the code
	// defaults in agents.RunLimits (withDefaults backstops any zero). Tunable per-deployment
	// via env so the loop budget isn't a recompile.
	AgentMaxIterations      int           // MANYFORGE_AGENT_MAX_ITERATIONS (default 8)
	AgentMaxTokensPerRun    int           // MANYFORGE_AGENT_MAX_TOKENS_PER_RUN (default 100000)
	AgentMaxOutputTokens    int           // MANYFORGE_AGENT_MAX_OUTPUT_TOKENS (default 4096)
	AgentWallClock          time.Duration // MANYFORGE_AGENT_WALL_CLOCK (default 120s)
	AgentTemperature        float64       // MANYFORGE_AGENT_TEMPERATURE (default 0.0; deterministic)
	AgentRetriageCapPerHour int           // MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR (default 5; per-ticket/agent reply re-triage cap)

	// Spec 007 code-review sandbox (slice 1).
	// SandboxImage is the opencode review sandbox Docker image.
	// Default: manyforge/opencode-sandbox:dev.
	SandboxImage string
	// EgressProxyImage is the allowlisting egress proxy Docker image.
	// Default: manyforge/egress-proxy:dev.
	EgressProxyImage string
	// SandboxEgressAllow is a comma-separated list of provider hostnames the
	// sandbox is allowed to reach through the egress proxy.
	// Default: api.anthropic.com,openrouter.ai,api.openai.com,router.huggingface.co.
	SandboxEgressAllow string
	// SandboxWorkRoot is the host-side directory used for per-run checkouts.
	// MUST be a path visible inside the Docker VM (on Colima/Mac that means
	// under $HOME, NOT /tmp). Default: $HOME/.cache/manyforge/sandbox.
	SandboxWorkRoot string
	// SandboxReviewTimeout is the wall-clock cap for a single sandbox review run
	// (CodeReviewService.Timeout), covering every provider including local ones
	// (Ollama/vLLM/LM Studio) now that all reviews route through the sandbox
	// (manyforge-9er) — there is no longer a separate host-side local-provider path.
	// Default 8m matches the prior hardcoded cloud-sandbox cap; operators running a
	// slow local model can raise it. Env: MANYFORGE_SANDBOX_REVIEW_TIMEOUT.
	SandboxReviewTimeout time.Duration

	// SandboxMode selects which sandbox runner backs the code-review sandbox:
	// "off" (disabled), "docker" (DockerRunner), or "kube" (KubeRunner, Task 4.5).
	// Env: MANYFORGE_SANDBOX_MODE. Defaults to "kube" when KUBERNETES_SERVICE_HOST
	// is set (running in-cluster), else "docker". Any other value is a hard config
	// error caught here.
	SandboxMode string
	// SandboxNamespace is the namespace the KubeRunner creates per-review Jobs/
	// Secrets/ConfigMaps in, and the namespace the egress-proxy Service lives in
	// (ProxyAddr is derived from it in main.go). This is deliberately NOT
	// kube.Namespace() — that helper reads the RUNNING POD's own namespace (the
	// app's release namespace), which is a different namespace than the
	// dedicated sandbox namespace the Role/RoleBinding in charts/manyforge/
	// templates/rbac.yaml grant access to. Env: MANYFORGE_SANDBOX_NAMESPACE.
	// Defaults to "manyforge-sandbox", matching the chart's
	// .Values.sandbox.namespace default — keep the two in sync.
	SandboxNamespace string
	// SandboxPullSecret is the name of the image-pull Secret the KubeRunner
	// attaches to every per-review Job's PodSpec (imagePullSecrets), so a
	// private sandbox image (opencode-sandbox, egress-proxy) can be pulled in
	// the dedicated sandbox namespace. Env: MANYFORGE_SANDBOX_PULL_SECRET.
	// Default: "ghcr-auth", matching the chart's imagePullSecrets default —
	// keep the two in sync (see charts/manyforge/values.yaml sandbox.pullSecret).
	SandboxPullSecret string
}

// Load reads configuration from the environment, applying safe local-dev
// defaults. It errors only on malformed values.
func Load() (Config, error) {
	cfg := Config{
		Addr:             env("MANYFORGE_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("MANYFORGE_DATABASE_URL"),
		TrustedProxyCIDR: os.Getenv("MANYFORGE_TRUSTED_PROXY_CIDR"),
		JWTIssuer:        env("MANYFORGE_JWT_ISSUER", "manyforge"),
		JWTAudience:      env("MANYFORGE_JWT_AUDIENCE", "manyforge-api"),
		JWTActiveKID:     env("MANYFORGE_JWT_ACTIVE_KID", ""),
		JWTVerifyKeys:    env("MANYFORGE_JWT_VERIFY_KEYS", ""),
	}

	ttl, err := envDuration("MANYFORGE_ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_ACCESS_TOKEN_TTL: %w", err)
	}
	cfg.AccessTokenTTL = ttl

	if cfg.RateLimitRPS, err = envFloat("MANYFORGE_RATELIMIT_RPS", 5); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_RATELIMIT_RPS: %w", err)
	}
	if cfg.RateLimitBurst, err = envFloat("MANYFORGE_RATELIMIT_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_RATELIMIT_BURST: %w", err)
	}

	// Persistent JWT signing key (Task 1.1): supplied inline (…_PEM) or via a
	// file path (…_PEM_PATH), mirroring the DKIM key pattern below. Unset ⇒ the
	// server falls back to the ephemeral dev key ring (see main.go); a
	// configured-but-unreadable path is a hard config error.
	cfg.JWTSigningKeyPEM = os.Getenv("MANYFORGE_JWT_SIGNING_KEY_PEM")
	if cfg.JWTSigningKeyPEM == "" {
		if p := os.Getenv("MANYFORGE_JWT_SIGNING_KEY_PEM_PATH"); p != "" {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return Config{}, fmt.Errorf("MANYFORGE_JWT_SIGNING_KEY_PEM_PATH: %w", rerr)
			}
			cfg.JWTSigningKeyPEM = string(b)
		}
	}

	// Support desk (spec 002).
	cfg.SMTPAddr = os.Getenv("MANYFORGE_SMTP_ADDR")
	cfg.InboundWebhookSecret = os.Getenv("MANYFORGE_INBOUND_WEBHOOK_SECRET")
	// Hard-bounce webhook HMAC secret. Purpose-separated from the inbound-webhook
	// secret so a leak of one cannot forge the other; empty in dev fail-closes the
	// bounce verify (every signature is rejected, and the no-oracle ack still 202s).
	cfg.InboundBounceSecret = os.Getenv("MANYFORGE_INBOUND_BOUNCE_SECRET")
	// Reply-token HMAC key: purpose-separated from the webhook + JWT secrets so a
	// leak of one cannot forge the others. Decoded as raw bytes (the string is the
	// key material); empty in dev is tolerated (threading falls back to headers).
	cfg.InboundReplyTokenSecret = []byte(os.Getenv("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET"))
	// System inbound-address HMAC key (FR-001): derives the unguessable localpart of
	// the auto-provisioned b-…@<domain> address. Purpose-separated from the reply-token
	// key; empty in dev is tolerated (the address is still deterministic+idempotent,
	// just not key-protected against enumeration in dev).
	cfg.InboundSystemAddrSecret = []byte(os.Getenv("MANYFORGE_INBOUND_SYSTEM_ADDRESS_SECRET"))
	cfg.BlobURL = os.Getenv("MANYFORGE_BLOB_URL")
	cfg.InboundSystemDomain = env("MANYFORGE_INBOUND_SYSTEM_DOMAIN", "inbound.localhost")
	cfg.DKIMKeyPath = os.Getenv("MANYFORGE_DKIM_KEY_PATH")

	if cfg.InboundMaxBytes, err = envInt64("MANYFORGE_INBOUND_MAX_BYTES", 30<<20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INBOUND_MAX_BYTES: %w", err)
	}
	if cfg.AttachmentMaxBytes, err = envInt64("MANYFORGE_ATTACHMENT_MAX_BYTES", 25<<20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_ATTACHMENT_MAX_BYTES: %w", err)
	}
	if cfg.IngestRateRPS, err = envFloat("MANYFORGE_INGEST_RATE_RPS", 2); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INGEST_RATE_RPS: %w", err)
	}
	if cfg.IngestRateBurst, err = envFloat("MANYFORGE_INGEST_RATE_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_INGEST_RATE_BURST: %w", err)
	}
	if cfg.OutboundRateRPS, err = envFloat("MANYFORGE_OUTBOUND_RATE_RPS", 2); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_OUTBOUND_RATE_RPS: %w", err)
	}
	if cfg.OutboundRateBurst, err = envFloat("MANYFORGE_OUTBOUND_RATE_BURST", 20); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_OUTBOUND_RATE_BURST: %w", err)
	}

	// Outbound SMTP relay (US2). Host empty ⇒ LogSender; port defaults to submission.
	cfg.SMTPHost = os.Getenv("MANYFORGE_SMTP_HOST")
	if cfg.SMTPPort, err = envInt("MANYFORGE_SMTP_PORT", 587); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_SMTP_PORT: %w", err)
	}
	cfg.SMTPUser = os.Getenv("MANYFORGE_SMTP_USER")
	cfg.SMTPPass = os.Getenv("MANYFORGE_SMTP_PASS")

	// Optional system DKIM. The private key can be supplied inline (…_PEM) or via a
	// file path (…_PEM_PATH); a malformed path is a hard config error so a configured
	// key never silently degrades to unsigned (the no-DKIM default is the empty case).
	cfg.SystemDKIMDomain = os.Getenv("MANYFORGE_SYSTEM_DKIM_DOMAIN")
	cfg.SystemDKIMSelector = os.Getenv("MANYFORGE_SYSTEM_DKIM_SELECTOR")
	cfg.SystemDKIMPrivateKeyPEM = os.Getenv("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM")
	if cfg.SystemDKIMPrivateKeyPEM == "" {
		if p := os.Getenv("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM_PATH"); p != "" {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return Config{}, fmt.Errorf("MANYFORGE_SYSTEM_DKIM_PRIVATE_KEY_PEM_PATH: %w", rerr)
			}
			cfg.SystemDKIMPrivateKeyPEM = string(b)
		}
	}

	// DKIM master key (US4): unset ⇒ nil (no error, signing degrades to system);
	// set-but-not-32-bytes-after-decode ⇒ hard error so a misconfigured key never
	// silently disables custom-domain signing.
	if cfg.DKIMMasterKey, err = envKey32("MANYFORGE_DKIM_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_DKIM_MASTER_KEY: %w", err)
	}

	// MCP master key (US6): unset ⇒ nil (no error, auth-token sealing degrades);
	// set-but-not-32-bytes-after-decode ⇒ hard error so a misconfigured key never
	// silently disables MCP bearer-token protection.
	if cfg.MCPMasterKey, err = envKey32("MANYFORGE_MCP_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_MCP_MASTER_KEY: %w", err)
	}

	// AI master key (agent BYO credentials): unset ⇒ nil (no error, credential surface
	// disabled); set-but-not-32-bytes-after-decode ⇒ hard error so a misconfigured key
	// never silently disables BYO provider-key sealing.
	if cfg.AIMasterKey, err = envKey32("MANYFORGE_AI_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AI_MASTER_KEY: %w", err)
	}

	// Agent runtime (US6): permit loopback MCP servers in dev; default false (locked-secure).
	if cfg.MCPAllowLoopback, err = envBool("MANYFORGE_MCP_ALLOW_LOOPBACK", false); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_MCP_ALLOW_LOOPBACK: %w", err)
	}

	// Connector master key (US3 Spec 004): unset ⇒ nil (no error, connector stack
	// disabled); set-but-not-32-bytes-after-decode ⇒ hard error so a misconfigured
	// key never silently disables the webhook handler/inbound-sync/reconciler.
	if cfg.ConnectorMasterKey, err = envKey32("MANYFORGE_CONNECTOR_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_CONNECTOR_MASTER_KEY: %w", err)
	}

	// GitHub App master key: unset ⇒ nil (no error, GitHub App integration disabled);
	// set-but-not-32-bytes-after-decode ⇒ hard error so a misconfigured key never
	// silently disables sealing of the instance GitHub App credentials.
	if cfg.GitHubAppMasterKey, err = envKey32("MANYFORGE_GITHUB_APP_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_GITHUB_APP_MASTER_KEY: %w", err)
	}

	// Instance operator principal (GitHub App setup gate): unset ⇒ uuid.Nil, so the
	// operator-only routes 404 for everyone until explicitly configured.
	if v := os.Getenv("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL"); v != "" {
		if cfg.InstanceOperatorPrincipal, err = uuid.Parse(v); err != nil {
			return Config{}, fmt.Errorf("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL: %w", err)
		}
	}
	cfg.PublicBaseURL = strings.TrimSuffix(os.Getenv("MANYFORGE_PUBLIC_BASE_URL"), "/")

	// Agent run loop bounds (Spec 003 §8). Defaults mirror agents.RunLimits; a malformed value
	// is a hard config error rather than a silent fallback (the loop budget is a safety bound).
	if cfg.AgentMaxIterations, err = envInt("MANYFORGE_AGENT_MAX_ITERATIONS", 8); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_MAX_ITERATIONS: %w", err)
	}
	if cfg.AgentMaxTokensPerRun, err = envInt("MANYFORGE_AGENT_MAX_TOKENS_PER_RUN", 100_000); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_MAX_TOKENS_PER_RUN: %w", err)
	}
	if cfg.AgentMaxOutputTokens, err = envInt("MANYFORGE_AGENT_MAX_OUTPUT_TOKENS", 4096); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_MAX_OUTPUT_TOKENS: %w", err)
	}
	if cfg.AgentWallClock, err = envDuration("MANYFORGE_AGENT_WALL_CLOCK", 120*time.Second); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_WALL_CLOCK: %w", err)
	}
	if cfg.AgentTemperature, err = envFloat("MANYFORGE_AGENT_TEMPERATURE", 0.0); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_TEMPERATURE: %w", err)
	}
	if cfg.AgentRetriageCapPerHour, err = envInt("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR", 5); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR: %w", err)
	}

	// Spec 007 sandbox defaults. SandboxWorkRoot must be bind-mountable into the
	// Docker VM: on Colima/Mac only paths under $HOME are mirrored into the VM,
	// so we default to $HOME/.cache/manyforge/sandbox (never os.TempDir()/tmp).
	cfg.SandboxImage = env("MANYFORGE_SANDBOX_IMAGE", "manyforge/opencode-sandbox:dev")
	cfg.EgressProxyImage = env("MANYFORGE_EGRESS_PROXY_IMAGE", "manyforge/egress-proxy:dev")
	cfg.SandboxEgressAllow = env("MANYFORGE_SANDBOX_EGRESS_ALLOW", "api.anthropic.com,openrouter.ai,api.openai.com,router.huggingface.co")
	sandboxWorkRootDefault := "/tmp/mf-sandbox" // fallback only; overridden below when $HOME is available
	if home, herr := os.UserHomeDir(); herr == nil {
		sandboxWorkRootDefault = home + "/.cache/manyforge/sandbox"
	}
	cfg.SandboxWorkRoot = env("MANYFORGE_SANDBOX_WORK_ROOT", sandboxWorkRootDefault)

	if cfg.SandboxReviewTimeout, err = envDuration("MANYFORGE_SANDBOX_REVIEW_TIMEOUT", 8*time.Minute); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_SANDBOX_REVIEW_TIMEOUT: %w", err)
	}

	// Sandbox mode (Task 4.1, Phase 4 k8s-native sandbox). Defaults to "kube" when
	// KUBERNETES_SERVICE_HOST is set (the standard in-cluster env var Kubernetes
	// injects into every pod), else "docker". Only the env var is consulted here —
	// this package must NOT import client-go — so the default is a cheap, offline
	// signal rather than a real in-cluster API probe.
	cfg.SandboxMode = env("MANYFORGE_SANDBOX_MODE", defaultSandboxMode())
	switch cfg.SandboxMode {
	case "off", "docker", "kube":
	default:
		return Config{}, fmt.Errorf("MANYFORGE_SANDBOX_MODE: must be off|docker|kube, got %q", cfg.SandboxMode)
	}

	// Single source of truth for the sandbox namespace (Task 4.5 fix): the
	// KubeRunner's Namespace and the egress-proxy ProxyAddr in main.go both
	// derive from this, not from kube.Namespace() (which reads the app pod's
	// OWN namespace — the wrong value here).
	cfg.SandboxNamespace = env("MANYFORGE_SANDBOX_NAMESPACE", "manyforge-sandbox")
	cfg.SandboxPullSecret = env("MANYFORGE_SANDBOX_PULL_SECRET", "ghcr-auth")

	return cfg, nil
}

// defaultSandboxMode picks "kube" when KUBERNETES_SERVICE_HOST is present (the pod
// is running in-cluster), else "docker".
func defaultSandboxMode() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return "kube"
	}
	return "docker"
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return time.ParseDuration(v)
}

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseFloat(v, 64)
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseBool(v)
}

func envInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// envKey32 decodes a 32-byte key from base64 (std then url) or hex. An unset/empty
// var returns (nil, nil): the feature degrades rather than blocking boot. A value
// that fails to decode, or decodes to a non-32-byte length, is a hard error.
//
// Each encoding is tried in turn, preferring the first that yields exactly 32
// bytes. A 64-char hex key is also valid base64 (decoding to 48 bytes), so we
// must not commit to the first successful decode regardless of length.
// hexKey32Re anchors the bare-form hex auto-detect: exactly 64 hex chars (a 32-byte
// key). Anything else in bare form is treated as base64. This makes the hex-vs-base64
// choice deterministic instead of "first decoder that happens to yield 32 bytes".
var hexKey32Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// envKey32 loads a 32-byte secret from the named env var (manyforge-no9). The value
// is parsed unambiguously:
//   - "hex:<v>"    — v is hex (authoritative; no auto-detect)
//   - "base64:<v>" — v is base64, std or URL alphabet, padded or raw (authoritative)
//   - bare <v>     — auto-detected: a 64-char [0-9a-fA-F] string is hex; anything
//     else is base64. Back-compatible with the two canonical encodings of a 32-byte
//     key (44-char base64, 64-char hex); the explicit prefixes remove all doubt.
//
// Empty/unset ⇒ (nil, nil). A value that decodes but isn't 32 bytes, or doesn't
// decode at all, is a config error.
func envKey32(key string) ([]byte, error) {
	v := os.Getenv(key)
	if v == "" {
		return nil, nil
	}

	var (
		b   []byte
		err error
	)
	switch {
	case strings.HasPrefix(v, "hex:"):
		b, err = hex.DecodeString(strings.TrimPrefix(v, "hex:"))
	case strings.HasPrefix(v, "base64:"):
		b, err = decodeBase64(strings.TrimPrefix(v, "base64:"))
	case hexKey32Re.MatchString(v):
		b, err = hex.DecodeString(v)
	default:
		b, err = decodeBase64(v)
	}
	if err != nil {
		return nil, fmt.Errorf("not valid base64 or hex")
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("decoded key must be 32 bytes, got %d", len(b))
	}
	return b, nil
}

// decodeBase64 accepts a base64 string in either the standard or URL-safe alphabet,
// padded or raw (unpadded) — the four forms a 32-byte key can take.
func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, derr := enc.DecodeString(s); derr == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("invalid base64")
}
