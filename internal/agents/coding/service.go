package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/platform/audit"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// CodeReview is the service-layer view of a code_review row.
type CodeReview struct {
	ID            uuid.UUID            `json:"id"`
	Status        string               `json:"status"`
	Summary       string               `json:"summary"`
	ReviewURL     string               `json:"review_url"`
	PRNumber      int                  `json:"pr_number"`
	Model         string               `json:"model"` // model snapshot used for this review
	Repo          string               `json:"repo,omitempty"` // "owner/name" from the review's connector (list rows, via the join); normally always set (repo_connector_id is a NOT NULL FK)
	Findings      []connectors.Finding `json:"findings"`
	FindingsCount int                  `json:"findings_count"`
	CostCents     int64                `json:"cost_cents"` // LLM cost of the run (0 until usage capture lands)
	CreatedAt     time.Time            `json:"created_at"`
	PostedAt      *time.Time           `json:"posted_at"`
	// Progress is the live progress snapshot for a running review (phase/tokens/
	// preview); null/omitted for pending and terminal reviews. Populated from the
	// code_review.progress jsonb the worker heartbeat persists.
	Progress json.RawMessage `json:"progress,omitempty"`
	// DimensionRuns is the per-lane accounting for a multi-dimension review (spec 008):
	// a JSON array of {dimension, model, provider, tokens_in, tokens_out, cost_cents,
	// status, skipped_reason, finding_count}. The detail UI groups findings by dimension
	// and surfaces skipped lanes from it. Empty/omitted for legacy single-lane reviews.
	DimensionRuns json.RawMessage `json:"dimension_runs,omitempty"`
}

// ClaimedReview is the typed representation of a claim_code_reviews result row,
// with UUIDs unwrapped from pgtype. The background worker maps the claimed row
// into this struct and passes it to runJob so no secrets travel in the queue row.
type ClaimedReview struct {
	ID              uuid.UUID
	BusinessID      uuid.UUID
	PrincipalID     uuid.UUID
	AgentID         uuid.UUID
	RepoConnectorID uuid.UUID
	PRNumber        int
	Attempts        int
}

// serviceDB is the minimal DB surface required by CodeReviewService.
// Satisfied by *appdb.DB.
type serviceDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// repoResolver is the minimal RepoConnectorService surface needed.
type repoResolver interface {
	Resolve(ctx context.Context, principalID, businessID, id uuid.UUID) (connectors.ResolvedRepoConnector, error)
}

// installationTokenSource mints a fresh per-repo GitHub App installation token
// (spec 011 Slice 2). Satisfied by *githubapp.InstallationTokenSource. runJob
// calls it for github_app connectors — which carry no stored credential — to
// obtain a short-lived ghs_ token that is a drop-in for a PAT on the clone/
// fetch/post paths. It is an interface (not a concrete type) so the coding
// package never imports githubapp and stays test-fakeable.
type installationTokenSource interface {
	Token(ctx context.Context, installationID int64, repo string) (string, error)
}

// CostEstimator prices a run from its token counts. Implemented by
// *agents.OpenRouterModels (live OpenRouter pricing). Returns 0 (no error) for
// providers/models it can't price, so usage capture never fails a review.
type CostEstimator interface {
	CostCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error)
}

// CodeReviewService orchestrates the code-review agent lifecycle.
// Enqueue performs cheap validation and inserts a pending row.
// runJob (called by the background worker) runs the heavy pipeline:
// FetchPR → clone → sandbox → parse findings → post review → finalize.
type CodeReviewService struct {
	DB       serviceDB
	Repos    repoResolver
	Sandbox  sandbox.SandboxRunner
	Creds    AICredentialResolver
	Image    string        // opencode sandbox image tag
	WorkRoot string        // host temp root for per-run checkouts; must be writable
	Timeout  time.Duration // sandbox wall-clock cap (0 = 10 min default)
	// LocalTimeout is the HTTP timeout for the host-side local-provider review path
	// (Ollama/vLLM); 0 ⇒ 30 min default. Separate from Timeout (the sandbox cap) so a
	// long local review (e.g. a 35b model) isn't killed at the 10-min sandbox limit.
	LocalTimeout time.Duration

	// EgressAllow is the boot-static set of provider hosts the sandbox egress
	// proxy permits (from MANYFORGE_SANDBOX_EGRESS_ALLOW). The proxy is shared and
	// long-lived, so Enqueue validates the run's provider host against this set
	// up front and fails with ErrValidation rather than launching a sandbox the
	// proxy will silently egress-block (manyforge-0qj). Same matcher the proxy uses.
	EgressAllow netsafe.HostAllowlist

	// Pricing estimates the LLM cost of a run from its token counts (opencode
	// reports 0 cost for a custom OpenRouter slug, so the host prices it). Optional —
	// when nil, reviews record 0 cost. Satisfied by *agents.OpenRouterModels.
	Pricing CostEstimator

	// Clone is the injectable seam for cloning a repo at a specific SHA.
	// Defaults to coding.CloneAtSHA when nil (set at call time). Tests inject
	// a fake that just creates the directory without needing a real git server.
	// The allowPrivate parameter mirrors rc.AllowPrivateBaseURL from the connector.
	Clone func(ctx context.Context, cloneURL, authHeader, sha, destDir string, allowPrivate bool) error

	// ClonesInSandbox is true when the configured sandbox.SandboxRunner clones the
	// reviewed repo itself (KubeRunner: an in-cluster init container clones
	// CloneURL/CloneSHA from the SandboxSpec) rather than depending on a host-side
	// checkout. Set from cfg.SandboxMode == "kube" in cmd/manyforge/main.go.
	//
	// This matters because the app runs as gcr.io/distroless/static:nonroot in kube
	// mode — no git, no shell, read-only root filesystem — so runJob's host-side
	// MkdirAll/git-clone (below) would fail outright. When true, runJob skips ALL
	// host filesystem work (WorkRoot/checkout/outDir) and never calls s.cloneFn();
	// the sandbox.SandboxSpec still carries CloneURL/CloneAuthHeader/CloneSHA/
	// CloneAllowPrivate so the runner can clone on its own, and findings come back
	// via SandboxResult.Outputs, never a shared host directory.
	//
	// The SSRF guard (checkCloneURL) always runs regardless of this flag — see
	// runJob — so a blocked clone host still fails the job before any runner is
	// invoked (MF-KUBE-SANDBOX-23).
	ClonesInSandbox bool

	// Tokens mints a fresh per-repo installation token for github_app connectors
	// (spec 011 Slice 2). Nil unless the GitHub App integration is configured
	// (main.go late-wires it inside the App-master-key block). A github_app
	// connector reaching runJob with a nil source fails the job — there is no
	// stored credential to fall back on. Manual ('github') reviews never touch it.
	Tokens installationTokenSource
}

// cloneFn returns the effective clone function (injectable seam or real default).
func (s *CodeReviewService) cloneFn() func(ctx context.Context, cloneURL, authHeader, sha, destDir string, allowPrivate bool) error {
	if s.Clone != nil {
		return s.Clone
	}
	return CloneAtSHA
}

// timeout returns the effective sandbox timeout.
func (s *CodeReviewService) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 10 * time.Minute
}

// localTimeout returns the effective host-side local-provider review timeout (30 min
// default). Distinct from timeout(): local models can run far longer than the cloud
// sandbox wall-clock cap.
func (s *CodeReviewService) localTimeout() time.Duration {
	if s.LocalTimeout > 0 {
		return s.LocalTimeout
	}
	return 30 * time.Minute
}

// localClient is the HTTP client for the host-side local-provider review path. A
// plain client (allows loopback, which the SSRF-safe netsafe client refuses);
// localReview enforces a loopback-only base URL so this stays local-only.
func (s *CodeReviewService) localClient() *http.Client {
	return &http.Client{Timeout: s.localTimeout()}
}

// Enqueue validates the request cheaply (resolve connector, resolve cred, egress
// pre-flight) and inserts a pending code_review row. It does NOT build the GitHub
// client, fetch the PR, clone, or touch the sandbox — those run in runJob when the
// background worker picks up the row.
// Returns CodeReview{ID, Status:"pending", PRNumber} on success.
func (s *CodeReviewService) Enqueue(
	ctx context.Context,
	principalID, businessID, agentID, repoConnectorID uuid.UUID,
	prNumber int,
) (CodeReview, error) {

	// 1. Resolve repo connector (RLS-scoped) to confirm ownership and extract type.
	// No GitHub client is built here — just an ownership/existence check.
	_, err := s.Repos.Resolve(ctx, principalID, businessID, repoConnectorID)
	if err != nil {
		return CodeReview{}, err
	}

	// 2. Resolve AI credential (RLS-scoped). Must have a non-empty host.
	cred, err := s.Creds.Resolve(ctx, principalID, businessID, agentID)
	if err != nil {
		return CodeReview{}, err
	}
	if cred.Host() == "" {
		return CodeReview{}, fmt.Errorf("coding: agent has no usable AI credential: %w", errs.ErrValidation)
	}
	// The sandbox egress proxy is shared and boot-static; a provider host outside
	// its allowlist would be silently CONNECT-blocked, so reject it up front with a
	// clear, actionable error instead of launching a doomed sandbox (manyforge-0qj).
	// Local providers (Ollama/vLLM) review host-side via the direct-API path — no
	// sandbox, no egress proxy — so the allowlist check does not apply to them.
	if !isLocalProvider(cred.Provider) && !s.EgressAllow.Allows(cred.Host()) {
		return CodeReview{}, fmt.Errorf(
			"coding: provider host %q is not in the sandbox egress allowlist (add it to MANYFORGE_SANDBOX_EGRESS_ALLOW): %w",
			cred.Host(), errs.ErrValidation)
	}

	// 3. Insert pending code_review + audit "agent.coding.review.requested" in one tx.
	crID := uuid.New()
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if _, ierr := dbgen.New(tx).InsertCodeReview(ctx, dbgen.InsertCodeReviewParams{
			ID:              crID,
			AgentRunID:      pgtype.UUID{}, // NULL — no agent_run tracking per arch decision
			RepoConnectorID: repoConnectorID,
			PrNumber:        int32(prNumber),
			BusinessID:      businessID,
			PrincipalID:     pgtype.UUID{Bytes: principalID, Valid: true},
			AgentID:         pgtype.UUID{Bytes: agentID, Valid: true},
			// Snapshot the resolved model so the history shows which model produced
			// each review even after the agent's model changes later.
			Model: cred.Model,
		}); ierr != nil {
			return ierr
		}
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID,
			"agent.coding.review.requested",
			map[string]any{"pr": prNumber, "repo_connector_id": repoConnectorID},
			nil,
			ptr("requested"),
		))
	}); err != nil {
		return CodeReview{}, err
	}

	return CodeReview{
		ID:       crID,
		Status:   "pending",
		PRNumber: prNumber,
	}, nil
}

// runJob executes the heavy code-review pipeline for a claimed row.
// It re-resolves the connector and credential under job.PrincipalID/BusinessID —
// NO secrets come from the queue row itself. Called by the background worker
// (Task 5) after claiming a pending row via the claim_code_reviews function.
// reviewCheckRunName is the GitHub Check Run name shown in the PR's Checks tab
// while an app-backed review runs (manyforge-nh6).
const reviewCheckRunName = "manyforge review"

// maxConcurrentLanes bounds how many dimension-review lanes run at once
// (manyforge-w54). Each lane is its own sandbox container (a Job/pod in kube
// mode), so this caps the simultaneous pod fan-out for a many-dimension panel.
const maxConcurrentLanes = 4

// reviewModelPanel is the code_review.model sentinel for a multi-dimension review
// (manyforge-vv6): there is no single model, so the UI renders it as a multi-model
// panel and reads the real per-lane models from dimension_runs.
const reviewModelPanel = "panel"

// reviewModelLabel is the value stamped on code_review.model. A single-lane review
// records the resolved model; a multi-dimension panel records the panel sentinel
// instead of the review agent's default model (which no lane necessarily ran, and
// which previously misled the review list into showing one model for the whole
// panel — manyforge-vv6).
func reviewModelLabel(active []Dimension, resolved string) string {
	switch {
	case len(active) > 1:
		return reviewModelPanel
	case len(active) == 1 && strings.TrimSpace(active[0].Model) != "":
		// A single configured dimension ran on its OWN model (PR #23 review) — record
		// that, not the review agent's default which no lane used.
		return active[0].Model
	default:
		return resolved
	}
}

// runJob uses a named return (retErr) so a deferred Check Run update can resolve
// the "review in progress" signal to success/failure from whatever exit path fires.
func (s *CodeReviewService) runJob(ctx context.Context, job ClaimedReview, prog *Progress) (retErr error) {
	// Re-resolve connector under the owning principal (no secrets in the queue row).
	rc, err := s.Repos.Resolve(ctx, job.PrincipalID, job.BusinessID, job.RepoConnectorID)
	if err != nil {
		return err
	}

	// App-backed connector: mint a fresh per-repo installation token (outside any DB
	// tx) and set it as the connector credential — runJob's clone/fetch/post paths are
	// otherwise unchanged (the ghs_ token is a drop-in for a PAT). NewFactory requires a
	// non-empty token, so this MUST precede it. A mint failure (incl. a suspended/
	// deleted install) is a plain error → failJob → bounded worker retry (no terminal
	// sentinel; a transient GitHub 5xx should get another attempt).
	if rc.Type == "github_app" {
		if s.Tokens == nil {
			return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
				fmt.Errorf("coding: github_app connector but no installation-token source configured: %w", errs.ErrValidation))
		}
		instID, ok := installationIDFromConfig(rc.Config)
		if !ok {
			return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
				fmt.Errorf("coding: github_app connector missing installation_id: %w", errs.ErrValidation))
		}
		tok, terr := s.Tokens.Token(ctx, instID, rc.Repo)
		if terr != nil {
			return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
				fmt.Errorf("coding: mint installation token: %w", terr))
		}
		rc.Credential.APIToken = tok
	}

	// Build GitHub connector client.
	conn, err := github.NewFactory(60 * time.Second)(rc)
	if err != nil {
		return fmt.Errorf("coding: build connector client: %w", err)
	}

	// Re-resolve AI credential under the owning principal (no secrets in the queue row).
	cred, err := s.Creds.Resolve(ctx, job.PrincipalID, job.BusinessID, job.AgentID)
	if err != nil {
		return err
	}
	if cred.Host() == "" {
		return fmt.Errorf("coding: agent has no usable AI credential: %w", errs.ErrValidation)
	}
	// Egress pre-flight (fable M5): fail fast on a provider host the boot-static
	// sandbox proxy will CONNECT-block, instead of launching a doomed sandbox. Same
	// expression as Enqueue — but this ALSO covers the app-triggered path, which is
	// enqueued by the webhook DEFINER and never went through Enqueue's check. Local
	// providers review host-side (no proxy), so the allowlist does not apply to them.
	if !isLocalProvider(cred.Provider) && !s.EgressAllow.Allows(cred.Host()) {
		return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
			fmt.Errorf("coding: provider host %q not in sandbox egress allowlist: %w", cred.Host(), errs.ErrValidation))
	}

	// From here on, any error must call s.failJob to mark the row failed.
	crID := job.ID
	prNumber := job.PRNumber
	principalID := job.PrincipalID
	businessID := job.BusinessID

	// Graceful degradation (manyforge-206): on a retry, switch to a faster
	// provider-compatible fallback model so a slow model that 504'd (OpenRouter
	// upstream idle timeout) doesn't just fail again on every attempt.
	if m := effectiveReviewModel(cred.Provider, cred.Model, job.Attempts); m != cred.Model {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.review.fallback_model",
			map[string]any{"configured": cred.Model, "fallback": m, "attempt": job.Attempts},
			nil, ptr("executed"),
		)
		cred.Model = m
	}

	// Live progress: scrub the resolved secrets from any streamed preview, and mark
	// the first phase. prog is nil for direct (non-worker) callers — all methods no-op.
	prog.SetSecrets(cred.APIKey, rc.Credential.APIToken)
	prog.SetPhase("preparing")

	// Fetch PR metadata (host-side, uses the credential).
	pr, err := conn.FetchPR(ctx, prNumber)
	if err != nil {
		return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
	}
	if pr.State != "open" {
		return s.failJob(ctx, principalID, businessID, crID, prNumber,
			fmt.Errorf("coding: pull request not open: %w", errs.ErrValidation))
	}

	// Claim-time same-head re-check (fable M4): a rapid-push sibling may have already
	// reviewed this EXACT head between enqueue and claim. Skip (finalize succeeded, no
	// post) to avoid a duplicate review on the PR. A query error is treated as "not yet
	// reviewed" (proceed) — we never skip a real review on a transient DB hiccup.
	if already, cerr := s.reviewedHead(ctx, job.PrincipalID, job.BusinessID, job.RepoConnectorID, job.PRNumber, pr.HeadSHA, job.ID); cerr == nil && already {
		return s.finalizeSkipped(ctx, job, pr.HeadSHA, cred.Model)
	}

	// Best-effort "review in progress" Check Run on the PR (manyforge-nh6). Placed
	// after the skip check so a skipped/deduped review never opens one. Only
	// app-backed connectors carry the checks:write permission GitHub requires (a PAT
	// would 403), so we gate on that and swallow any error — a progress signal must
	// never fail or block the review. The deferred update resolves the run from
	// retErr, covering every exit below (success, failJob*, or a raw finalize error);
	// checkFindings/checkReviewURL are stamped by the success path for the summary.
	var checkFindings int
	var checkReviewURL string
	if rc.Type == "github_app" {
		if crp, ok := conn.(connectors.CheckRunPoster); ok {
			if id, cerr := crp.CreateCheckRun(ctx, reviewCheckRunName, pr.HeadSHA); cerr != nil {
				slog.Default().WarnContext(ctx, "code review: create check run (non-fatal)", "err", cerr, "review", crID)
			} else {
				defer func() {
					// Recover so a panic on runJob's stack (retErr stays nil during
					// unwinding) resolves the run as failure, not a misleading success —
					// then re-raise so the worker still observes the crash.
					rec := recover()
					conclusion, title := "success", "manyforge review complete"
					sha := pr.HeadSHA
					if len(sha) > 7 {
						sha = sha[:7]
					}
					summary := fmt.Sprintf("Reviewed %s — %d finding(s).", sha, checkFindings)
					// checkReviewURL is GitHub's own review html_url; still, only embed it
					// as a link when it's a clean https URL so a surprising value can't
					// break out of the Markdown link syntax (PR #20 review).
					if strings.HasPrefix(checkReviewURL, "https://") && !strings.ContainsAny(checkReviewURL, "() ") {
						summary += " [View review](" + checkReviewURL + ")"
					}
					if retErr != nil || rec != nil {
						conclusion, title = "failure", "manyforge review failed"
						summary = "The review did not complete. See manyforge logs for details."
					}
					// Resolve on a cancel-immune context: when the job failed BECAUSE its
					// own ctx was canceled (timeout/shutdown), reusing it here would fail
					// too and leave the run stuck "in progress" forever (PR #20 review).
					uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					if uerr := crp.UpdateCheckRun(uctx, id, conclusion, title, summary); uerr != nil {
						slog.Default().WarnContext(ctx, "code review: update check run (non-fatal)", "err", uerr, "review", crID)
					}
					if rec != nil {
						panic(rec)
					}
				}()
			}
		}
	}

	// Set up per-run directories and clone the PR head — HOST-SIDE ONLY when the
	// configured runner does not clone in the sandbox itself (docker/off/tests).
	// In kube mode (s.ClonesInSandbox) the app pod is distroless (no git, no
	// shell, read-only root FS) — the KubeRunner's own init container clones the
	// repo instead, so none of this host filesystem work happens; checkout/outDir
	// below stay as inert, never-created paths that KubeRunner ignores.
	runDir := filepath.Join(s.WorkRoot, crID.String())
	checkout := filepath.Join(runDir, "checkout")
	outDir := filepath.Join(runDir, "out")
	if !s.ClonesInSandbox {
		// Defense in depth: shield the per-run dirs from other local users by making
		// WorkRoot 0700 owned by the server user. The leaf /work and /out below are
		// world-accessible so the capless container can reach them, but a 0700 ancestor
		// means no other local user can traverse in. (The docker daemon resolves the
		// bind-mount source as root, so the 0700 ancestor never blocks the container.)
		if err := os.MkdirAll(s.WorkRoot, 0o700); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		if err := os.Chmod(s.WorkRoot, 0o700); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		// The sandbox runs with --cap-drop ALL, which strips CAP_DAC_OVERRIDE: even the
		// container's root must obey filesystem permission bits. The per-run dirs are
		// owned by the host server user, but the container process runs as a different
		// uid — so the read-only /work mount must be world-readable/traversable (0755)
		// and the /out mount world-writable (0777), or opencode can neither read the
		// checkout nor write findings. Chmod explicitly to defeat the process umask.
		// (Colima remaps bind-mount ownership and hides this; native Linux preserves it
		// — see TestSandboxIsolation, which pins both halves.) The 0700 WorkRoot above
		// keeps these world-perms unreachable by other local users. A future hardening
		// is `--user <host-uid>` so 0700 leaves suffice and no world-perms are needed.
		if err := os.MkdirAll(checkout, 0o755); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		if err := os.Chmod(checkout, 0o755); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		if err := os.MkdirAll(outDir, 0o777); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		if err := os.Chmod(outDir, 0o777); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
		defer func() { _ = os.RemoveAll(runDir) }() // always clean up regardless of outcome
	}

	authHeader := github.BasicAuthHeader(rc.Credential.APIToken)

	// SSRF guard: ALWAYS validate the clone URL host, regardless of which runner
	// ends up performing the actual clone. checkCloneURL is pure (url.Parse +
	// net.LookupIP + netsafe.IsBlocked) — no git/exec needed — so it runs safely
	// even in the distroless kube-mode app pod. In docker/off mode s.cloneFn()
	// (CloneAtSHA) re-checks this internally before shelling out to git; in kube
	// mode there is no host-side git at all, so THIS is the only host-side SSRF
	// guard the run gets before the KubeRunner's in-cluster init container clones
	// the repo. Pinned by MF-KUBE-SANDBOX-23.
	if err := checkCloneURL(conn.CloneURL(), rc.AllowPrivateBaseURL); err != nil {
		return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
	}

	if !s.ClonesInSandbox {
		if err := s.cloneFn()(ctx, conn.CloneURL(), authHeader, pr.HeadSHA, checkout, rc.AllowPrivateBaseURL); err != nil {
			return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
		}
	}

	// Fetch the PR's changed files (patch text + commentable lines) once. Used to
	// (a) render the diff-based review payload, (b) post findings as inline diff
	// comments, and (c) scope the sandbox. Best-effort: on failure `files` is nil and
	// the review degrades (cloud → whole-repo; local → "no reviewable changes").
	files, cerr := conn.ChangedFiles(ctx, prNumber)
	if cerr != nil {
		files = nil
	}
	changed := commentableMap(files)
	// On-host local providers (Ollama/vLLM) get a tighter diff budget: small models can't
	// prompt-eval a large diff in reasonable time. Cloud/opencode (capable models) uses the
	// larger default. Prose/planning docs are filtered out in either case (see assembleDiffPayload).
	maxTotal := reviewMaxTotalBytes
	if isLocalProvider(cred.Provider) {
		maxTotal = localProviderMaxTotalBytes
	}
	// The whole-PR dropped-file sets (skipped/omitted/filtered) are computed once here for the
	// audit trail and the review body's "not reviewed" note; each dimension lane assembles its
	// own scope-filtered payload below (an unscoped lane reproduces this exact payload).
	_, skippedFiles, omittedFiles, filteredFiles := assembleDiffPayload(files, maxTotal)
	// No silent caps: dropped files are surfaced in the review body AND recorded on the audit
	// trail (binary/too-large → skipped; over-budget → omitted; prose/plan docs → filtered).
	if len(skippedFiles) > 0 || len(omittedFiles) > 0 || len(filteredFiles) > 0 {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.review.files_dropped",
			map[string]any{"skipped": skippedFiles, "omitted": omittedFiles, "filtered_docs": filteredFiles},
			nil, ptr("executed"),
		)
	}
	// Fan out the review across the active dimension lanes (spec 008). Zero-config resolves to a
	// single "general" lane (defaultPanel), so a default review is byte-for-byte the legacy
	// single-agent review; configured panels (DB-resolved) arrive in a later slice. Each lane
	// reviews its scope-filtered diff with its own prompt + model; findings are floored, tagged,
	// and de-duplicated, and usage is summed. Partial success (FR-013): one lane failing does
	// not fail the whole review — only every lane failing does.
	prog.SetPhase("reviewing")

	panel := s.resolvePanel(ctx, principalID, businessID)
	changedPaths := changedFilePaths(changed)
	active, skippedDims := activeDimensions(panel, changedPaths)
	// Drop lanes requesting an unsupported per-dimension provider (manyforge-ubk) — skip with a
	// reason rather than silently misroute them to the review's default provider.
	active, provSkipped := partitionByProvider(active, cred.Provider)
	skippedDims = append(skippedDims, provSkipped...)
	// Every dimension scoped out or skipped ⇒ nothing was reviewed. Fail honestly rather than
	// post an empty "No issues found" review that falsely implies the PR was checked
	// (aggregateReview would otherwise return an empty doc + nil error).
	if len(active) == 0 {
		return s.failJob(ctx, principalID, businessID, crID, prNumber,
			fmt.Errorf("coding: no reviewable dimensions matched this change: %w", errs.ErrValidation))
	}
	if len(skippedDims) > 0 {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.review.dimensions_skipped",
			map[string]any{"skipped": skippedDims}, nil, ptr("executed"),
		)
	}

	// reviewLane runs ONE dimension end-to-end and returns its outcome. Local providers
	// (Ollama/vLLM) review host-side via the direct-API path — small models can't drive
	// opencode's agent loop, and the on-host model needs no isolation (manyforge-62s; cost 0).
	// Cloud providers run opencode in the hardened, egress-restricted sandbox, writing to a
	// per-lane output dir so configured lanes never clobber each other's review.json. An empty
	// dim.Model uses the review's resolved credential; the dimension's prompt drives both paths.
	reviewLane := func(dim Dimension, laneOutDir string) laneResult {
		laneCred := cred
		if strings.TrimSpace(dim.Model) != "" {
			laneCred.Model = dim.Model
		}
		scoped := filterFilesByScope(files, dim.ScopeGlobs)
		lanePayload, _, _, _ := assembleDiffPayload(scoped, maxTotal)

		if isLocalProvider(laneCred.Provider) {
			_ = s.auditStep(ctx, principalID, businessID, crID,
				"agent.coding.localreview.invoked",
				map[string]any{"head_sha": pr.HeadSHA, "model": laneCred.Model, "base_url": laneCred.BaseURL, "dimension": dim.Key},
				nil, ptr("executed"),
			)
			d, in, out, lerr := localReview(ctx, s.localClient(), laneCred, lanePayload, dim.Prompt, prog)
			return laneResult{Dim: dim, Doc: d, Model: laneCred.Model, Provider: laneCred.Provider, TokensIn: clampInt32(in), TokensOut: clampInt32(out), Err: lerr} // local = no cost
		}

		// Cloud path: hand opencode the scoped diff + changed-file scope hint + the dimension's
		// prompt in-band on the spec (Inputs) — the runner materializes them where the
		// entrypoint reads them (/out/review_*.txt). review_diff is the primary scope;
		// review_files.txt is the whole-file fallback. Single-sourcing the prompt this way
		// means local and cloud share one prompt and prompt changes need no sandbox-image
		// rebuild — the image's baked INSTRUCTIONS is only a fallback when review_instructions.txt
		// is absent.
		inputs := map[string][]byte{
			"review_instructions.txt": []byte(dim.Prompt),
		}
		if len(scoped) > 0 {
			inputs["review_files.txt"] = []byte(strings.Join(changedFilePaths(commentableMap(scoped)), "\n"))
		}
		if lanePayload != "" {
			inputs["review_diff.txt"] = []byte(lanePayload)
		}
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.opencode.invoked",
			map[string]any{"image": s.Image, "head_sha": pr.HeadSHA, "model": laneCred.Model, "dimension": dim.Key},
			nil, ptr("executed"),
		)
		spec := sandbox.SandboxSpec{
			Image:       s.Image,
			ReadOnlyDir: checkout,
			OutputDir:   laneOutDir,
			Cmd:         opencodeCmd(laneCred.Model),
			Env:         sandboxEnv(laneCred),
			EgressAllow: []string{laneCred.Host()},
			Timeout:     s.timeout(),
			// Stream opencode's live tool-call narration into the review heartbeat so a cloud
			// review shows progress like the local path (secrets are scrubbed by prog.Snapshot
			// via SetSecrets). The worker persists prog every heartbeat while this lane runs.
			StreamStderr: &progressStreamWriter{prog: prog, dim: dim.Key},
			Inputs:       inputs,
			// Clone info, in-band, for a future host-FS-independent runner (KubeRunner, Task
			// 4.3) to clone the repo itself. DockerRunner ignores these fields — the host
			// clone into ReadOnlyDir above (runJob) is what it actually uses.
			CloneURL:          conn.CloneURL(),
			CloneAuthHeader:   authHeader,
			CloneSHA:          pr.HeadSHA,
			CloneAllowPrivate: rc.AllowPrivateBaseURL,
		}
		// runLaneOnce executes the sandbox once, captures usage, and parses the findings.
		// Usage is read as SOON as the sandbox returns — the model may have burned tokens even
		// when the run ultimately fails (unparseable output). A timed-out/killed container never
		// writes usage.json, so a killed lane genuinely has no usage to recover (manyforge-2s1).
		// Returns (usage, doc, failReason, err); failReason is the client-safe category for err.
		runLaneOnce := func() (sandboxUsage, FindingsDoc, string, error) {
			res, rerr := s.Sandbox.Run(ctx, spec)
			usage := parseSandboxUsage(res.Outputs["usage.json"])
			if rerr != nil {
				reason := "sandbox error"
				if res.TimedOut {
					reason = "timed out"
				}
				return usage, FindingsDoc{}, reason, rerr
			}
			raw, ok := res.Outputs["review.json"]
			if !ok {
				return usage, FindingsDoc{}, "no output produced",
					fmt.Errorf("coding: no findings produced (exit %d): %w%s", res.ExitCode, errNoReviewOutput, sandboxStderrTail(res.Stderr, laneCred.APIKey, rc.Credential.APIToken))
			}
			d, perr := ParseFindings(raw)
			if perr != nil {
				return usage, FindingsDoc{}, "unparseable model output",
					fmt.Errorf("%w%s", perr, sandboxStderrTail(res.Stderr, laneCred.APIKey, rc.Credential.APIToken))
			}
			return usage, d, "", nil
		}

		// Run the lane, retrying ONCE on a clean-exit parse/empty failure — mirrors the local
		// path's empty-under-json_schema retry (manyforge-6h1). A verbose model whose final JSON
		// was truncated/garbled often succeeds on a second pass. Only clean-exit failures retry:
		// a timeout or docker error would just recur and cost another full lane run. Usage is
		// summed across attempts so a failed-then-succeeded lane bills for both.
		var totalUsage sandboxUsage
		var doc FindingsDoc
		var laneErr error
		var reason string
		for attempt := 0; attempt < codeReviewLaneMaxAttempts; attempt++ {
			if attempt > 0 {
				slog.Default().InfoContext(ctx, "code review cloud lane: retrying after unparseable/empty output",
					"dimension", dim.Key, "model", laneCred.Model)
			}
			u, d, rsn, err := runLaneOnce()
			totalUsage = addUsage(totalUsage, u)
			if err == nil {
				doc, laneErr, reason = d, nil, ""
				break
			}
			doc, laneErr, reason = FindingsDoc{}, err, rsn
			if rsn != "unparseable model output" && rsn != "no output produced" {
				break // timeout / sandbox error — do not burn another full run
			}
		}

		// TokensIn counts the FULL billed prompt volume — fresh input plus cached reads,
		// which dominate the agentic loop (opencode re-reads the cached context every turn).
		lr := laneResult{Dim: dim, Model: laneCred.Model, Provider: laneCred.Provider,
			TokensIn: clampInt32(totalUsage.Input + totalUsage.CacheRead), TokensOut: clampInt32(totalUsage.Output + totalUsage.Reasoning)}
		// Prefer opencode's own cost (it prices cache-read tokens correctly); fall back to
		// catalog pricing only when opencode couldn't price the model (custom slug ⇒ cost 0).
		if c, priced := costCentsFromUsage(totalUsage); priced {
			lr.CostCents = c
		} else if s.Pricing != nil {
			if c, perr := s.Pricing.CostCents(ctx, laneCred.Provider, laneCred.Model, int64(lr.TokensIn), int64(lr.TokensOut)); perr == nil {
				lr.CostCents = c
			}
		}
		if laneErr != nil {
			lr.Err = laneErr
			lr.FailReason = reason
			// Log the FULL error server-side (it can carry model-output snippets / sandbox
			// internals) — the client only ever sees lr.FailReason. This closes the gap where a
			// partial-success lane's failure was silently dropped, undiagnosable (manyforge-2s1/6h1).
			slog.Default().ErrorContext(ctx, "code review cloud lane failed",
				"dimension", dim.Key, "model", laneCred.Model, "reason", reason, "err", lr.Err)
			return lr
		}
		lr.Doc = doc
		return lr
	}

	// Parallel fan-out (manyforge-w54): run each dimension lane concurrently — in kube
	// mode each lane is its own Job/pod — bounded by maxConcurrentLanes so a
	// many-dimension panel can't burst the cluster. Results are written BY INDEX so
	// aggregation stays order-deterministic (aggregateReview / dedupeFindings /
	// buildDimensionRuns depend on active's Order) regardless of completion order. A
	// single lane still writes directly to outDir (byte-for-byte the legacy path);
	// multiple lanes each get an isolated sub-dir so their review.json / usage.json
	// never collide. Per-lane dirs are created BEFORE the fan-out so a mkdir failure
	// still funnels through failJob cleanly rather than racing inside a goroutine.
	laneResults := make([]laneResult, len(active))
	laneOutDirs := make([]string, len(active))
	for i, dim := range active {
		laneOutDirs[i] = outDir
		if len(active) > 1 {
			laneOutDir := filepath.Join(outDir, "lane-"+dim.Key)
			// Host materialization only applies when the docker/off runner shares a
			// host output directory; KubeRunner ignores OutputDir entirely and the
			// distroless app pod couldn't create this anyway (s.ClonesInSandbox).
			if !s.ClonesInSandbox {
				if err := os.MkdirAll(laneOutDir, 0o777); err != nil {
					return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
				}
				if err := os.Chmod(laneOutDir, 0o777); err != nil {
					return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
				}
			}
			laneOutDirs[i] = laneOutDir
		}
	}
	var g errgroup.Group
	g.SetLimit(maxConcurrentLanes)
	for i, dim := range active {
		g.Go(func() error {
			// A lane now runs in its own goroutine, so a panic here would crash the
			// whole worker instead of failing a single review. Recover it and record
			// the lane as failed; aggregateReview then treats it like any other lane
			// failure (this review fails cleanly, sibling lanes still produce output).
			defer func() {
				if r := recover(); r != nil {
					laneResults[i] = laneResult{Dim: dim, Err: fmt.Errorf("coding: review lane %q panicked: %v", dim.Key, r)}
				}
			}()
			// reviewLane captures its own (non-panic) failures in laneResult.Err; the
			// goroutine never returns an error, so aggregateReview always sees every
			// lane's outcome (a failed lane is a failed dimension, not a dropped one).
			laneResults[i] = reviewLane(dim, laneOutDirs[i])
			return nil
		})
	}
	_ = g.Wait()

	doc, tokensIn, tokensOut, costCents, aggErr := aggregateReview(laneResults)
	if aggErr != nil {
		return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber, aggErr, tokensIn, tokensOut, costCents)
	}
	// Per-lane accounting persisted on the review row (spec 008): each ran lane's model/usage/
	// status/finding-count, plus any skipped dimensions with their reason. Empty for a default
	// single-lane review only in the sense that it holds one "general" entry.
	dimRunsJSON, _ := json.Marshal(buildDimensionRuns(laneResults, skippedDims))

	// Post the review to the PR (intentionally ungated — advisory only). Findings on
	// changed lines become inline diff comments; the rest land in the summary body.
	// DedupKey makes the post idempotent: a worker retry (e.g. a transient sandbox
	// error, or a finalize failure) re-runs the whole job, but PostReview reuses the
	// already-posted review instead of duplicating it (manyforge-303).
	// Strip any secret the sandbox/model echoed before it is stored on the review row
	// or posted to the PR (manyforge-fqo #2).
	redactDoc(&doc, cred.APIKey, rc.Credential.APIToken)
	review := buildReview(doc, changed, pr.HeadSHA, skippedFiles, omittedFiles)
	review.DedupKey = crID.String()
	prog.SetPhase("posting")
	ref, err := conn.PostReview(ctx, prNumber, review)
	if err != nil {
		return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber, err, tokensIn, tokensOut, costCents)
	}
	// manyforge-nh6: the review posted — stamp the detail the deferred check-run
	// update renders as its success summary (no-op for non-app connectors).
	checkFindings = len(doc.Findings)
	checkReviewURL = ref.URL

	// Finalize in one tx: record the run as an agent_run (so ReviewBot shows in
	// accounting), stamp tokens/cost + the run link on the review, and audit.
	findingsJSON, _ := json.Marshal(doc.Findings)
	postedAt := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}

	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		runID, rerr := q.CreateCodeReviewAgentRun(ctx, dbgen.CreateCodeReviewAgentRunParams{
			ID:            uuid.New(),
			AgentID:       job.AgentID,
			BusinessID:    businessID,
			TargetID:      crID,
			Status:        "succeeded",
			TokensIn:      tokensIn,
			TokensOut:     tokensOut,
			CostCents:     costCents,
			CorrelationID: crID.String(),
		})
		if rerr != nil {
			return rerr
		}
		_, uerr := q.UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID:                crID,
			Status:            "succeeded",
			HeadSha:           pr.HeadSHA,
			Summary:           doc.Summary,
			Findings:          findingsJSON,
			ExternalReviewRef: ref.ExternalID,
			PostedAt:          postedAt,
			TokensIn:          tokensIn,
			TokensOut:         tokensOut,
			CostCents:         costCents,
			DimensionRuns:     dimRunsJSON,
			Model:             reviewModelLabel(active, cred.Model), // fable M2 + vv6: single-lane stamps the resolved model; a multi-dim panel stamps the "panel" sentinel
			AgentRunID:        pgtype.UUID{Bytes: runID, Valid: true},
		})
		if uerr != nil {
			return uerr
		}
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID,
			"agent.coding.review.posted",
			nil,
			map[string]any{"review_url": ref.URL, "findings": len(doc.Findings),
				"tokens_in": tokensIn, "tokens_out": tokensOut, "cost_cents": costCents},
			ptr("posted"),
		))
	}); err != nil {
		return err
	}
	return nil
}

// List returns the business's code reviews newest-first (up to 200).
// ReviewURL is intentionally left empty in list rows — the UI list links
// to the detail page (Get), which resolves the connector repo and populates it.
// Skipping per-row connector resolves keeps List O(1) queries instead of O(n).
func (s *CodeReviewService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]CodeReview, error) {
	var out []CodeReview
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListCodeReviews(ctx, businessID)
		if err != nil {
			return err
		}
		out = make([]CodeReview, 0, len(rows))
		for _, r := range rows {
			var findings []connectors.Finding
			if len(r.Findings) > 0 {
				_ = json.Unmarshal(r.Findings, &findings)
			}
			var postedAt *time.Time
			if r.PostedAt.Valid {
				t := r.PostedAt.Time
				postedAt = &t
			}
			repo := ""
			if r.Repo != nil { // LEFT JOIN: nil when the connector was deleted
				repo = *r.Repo
			}
			out = append(out, CodeReview{
				ID:            r.ID,
				Status:        r.Status,
				Summary:       r.Summary,
				PRNumber:      int(r.PrNumber),
				Model:         r.Model,
				Repo:          repo,
				Findings:      findings,
				FindingsCount: len(findings),
				CostCents:     r.CostCents,
				CreatedAt:     r.CreatedAt,
				PostedAt:      postedAt,
				Progress:      json.RawMessage(r.Progress),
				// ReviewURL intentionally empty in List — populated in Get only.
				// The UI history list links via the detail page to avoid N connector
				// resolves per list row.
			})
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("coding: list code reviews: %w", err)
	}
	return out, nil
}

// Get loads a code_review by id, scoped to the business for RLS defense-in-depth.
// Cross-tenant or unknown id → ErrNotFound.
// ReviewURL is populated here (detail path) by resolving the connector's repo via
// s.Repos.Resolve and calling reviewURL(repo, pr, externalRef). It is intentionally
// left empty in List rows to avoid N connector resolves per list call — the UI
// history list links to the detail page instead.
func (s *CodeReviewService) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (CodeReview, error) {
	// dbgen row is captured in a closure-local to carry repoConnectorID + externalRef
	// out of the WithPrincipal scope for the post-tx connector resolve below.
	type rawRow struct {
		cr              CodeReview
		repoConnectorID uuid.UUID
		externalRef     string
	}
	var raw rawRow

	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetCodeReview(ctx, dbgen.GetCodeReviewParams{
			ID:         id,
			BusinessID: businessID,
		})
		if err != nil {
			return err
		}

		var findings []connectors.Finding
		if len(row.Findings) > 0 {
			_ = json.Unmarshal(row.Findings, &findings)
		}
		var postedAt *time.Time
		if row.PostedAt.Valid {
			t := row.PostedAt.Time
			postedAt = &t
		}

		raw.cr = CodeReview{
			ID:            row.ID,
			Status:        row.Status,
			Summary:       row.Summary,
			PRNumber:      int(row.PrNumber),
			Model:         row.Model,
			Findings:      findings,
			FindingsCount: len(findings),
			CostCents:     row.CostCents,
			CreatedAt:     row.CreatedAt,
			PostedAt:      postedAt,
			Progress:      json.RawMessage(row.Progress),
			DimensionRuns: json.RawMessage(row.DimensionRuns),
		}
		raw.repoConnectorID = row.RepoConnectorID
		raw.externalRef = row.ExternalReviewRef
		return nil
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CodeReview{}, fmt.Errorf("coding: code review not found: %w", errs.ErrNotFound)
		}
		return CodeReview{}, fmt.Errorf("coding: get code review: %w", err)
	}

	// Populate ReviewURL in the detail path by resolving the connector's repo.
	// Done outside the DB transaction: Repos.Resolve opens its own RLS-scoped tx.
	// Best-effort: a missing/inaccessible connector leaves ReviewURL empty without
	// failing the entire Get. Only attempted when externalRef is non-empty (i.e.
	// review was actually posted to GitHub).
	if raw.externalRef != "" {
		if rc, err := s.Repos.Resolve(ctx, principalID, businessID, raw.repoConnectorID); err == nil {
			raw.cr.ReviewURL = reviewURL(rc.Repo, raw.cr.PRNumber, raw.externalRef)
		}
	}

	return raw.cr, nil
}

// fail marks the code_review as failed in the DB (best-effort), audits the failure,
// and returns an empty CodeReview with the original cause (no provider/schema detail leaked).
func (s *CodeReviewService) fail(
	ctx context.Context,
	principalID, businessID, crID uuid.UUID,
	prNumber int,
	cause error,
) (CodeReview, error) {
	_ = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, _ = dbgen.New(tx).UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID:                crID,
			Status:            "failed",
			HeadSha:           "",
			Summary:           "",
			Findings:          []byte("[]"),
			ExternalReviewRef: "",
			PostedAt:          pgtype.Timestamptz{}, // NULL
			// fable C1: dimension_runs is jsonb NOT NULL — a nil []byte encodes to SQL
			// NULL → 23502 → this UPDATE silently aborts (the error is swallowed by the
			// `_, _ =`), the row stays 'running', and the aborted tx also drops the
			// failure audit below. Passing "[]" lets the row actually reach 'failed'
			// (findings is already non-nil). Model "" preserves any prior stamp via the
			// query's COALESCE(NULLIF(...)).
			DimensionRuns: []byte("[]"),
			Model:         "",
		})
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID,
			"agent.coding.review.failed",
			map[string]any{"pr": prNumber},
			map[string]any{"error": cause.Error()},
			ptr("failed"),
		))
	})
	return CodeReview{}, cause
}

// failJob is the runJob-specific variant of fail: marks failed and returns the error
// (no CodeReview value, since runJob returns only error).
func (s *CodeReviewService) failJob(
	ctx context.Context,
	principalID, businessID, crID uuid.UUID,
	prNumber int,
	cause error,
) error {
	_, _ = s.fail(ctx, principalID, businessID, crID, prNumber, cause)
	return cause
}

// failJobWithUsage is failJob for post-sandbox failures: it ALSO records the
// sandbox's token usage — as a failed agent_run (so accounting captures the spend)
// and on the review row — so a run that burned tokens before failing (e.g.
// unparseable model output) is still accounted for (manyforge-7n5). Each retry that
// reaches the sandbox records its own agent_run, so the total across attempts
// reflects real spend. Recording is best-effort and must never mask the original
// failure. SetCodeReviewUsage touches only tokens/cost — the worker's requeue/fail
// owns status/last_error/attempts.
func (s *CodeReviewService) failJobWithUsage(
	ctx context.Context,
	principalID, businessID, agentID, crID uuid.UUID,
	prNumber int,
	cause error,
	tokensIn, tokensOut int32, costCents int64,
) error {
	// Mark failed FIRST — fail()'s UpdateCodeReviewResult rewrites the row (and zeros
	// tokens/cost) — THEN record usage so it survives. The worker's requeue/fail (run
	// after runJob returns) only touches status/last_error/attempts, so these persist.
	err := s.failJob(ctx, principalID, businessID, crID, prNumber, cause)
	_ = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, rerr := q.CreateCodeReviewAgentRun(ctx, dbgen.CreateCodeReviewAgentRunParams{
			ID:            uuid.New(),
			AgentID:       agentID,
			BusinessID:    businessID,
			TargetID:      crID,
			Status:        "failed",
			TokensIn:      tokensIn,
			TokensOut:     tokensOut,
			CostCents:     costCents,
			CorrelationID: crID.String(),
		}); rerr != nil {
			return rerr
		}
		return q.SetCodeReviewUsage(ctx, dbgen.SetCodeReviewUsageParams{
			ID: crID, TokensIn: tokensIn, TokensOut: tokensOut, CostCents: costCents,
		})
	})
	return err
}

// installationIDFromConfig extracts the GitHub App installation id from a github_app
// connector's Config (populated from repo_connector.config → jsonb_build_object(
// 'installation_id', <bigint>) by github_pr_review_ingest). The value round-trips
// through json.Unmarshal(map[string]any), so it normally arrives as float64 —
// json.Number and the integer types are handled too for robustness. A missing, zero,
// or non-numeric value returns ok=false so the caller fails the job with a clear error.
func installationIDFromConfig(cfg map[string]any) (int64, bool) {
	v, present := cfg["installation_id"]
	if !present {
		return 0, false
	}
	var id int64
	switch n := v.(type) {
	case float64:
		id = int64(n)
	case json.Number:
		p, err := n.Int64()
		if err != nil {
			return 0, false
		}
		id = p
	case int64:
		id = n
	case int:
		id = int64(n)
	default:
		return 0, false
	}
	if id <= 0 {
		return 0, false
	}
	return id, true
}

// reviewedHead reports whether ANOTHER succeeded code_review already exists for this
// exact (repo_connector, pr, head_sha) — i.e. a rapid-push sibling already reviewed
// this head. selfID is excluded so the row's own eventual success never matches. Runs
// under the owning principal (RLS); business_id is pinned as defense in depth. A query
// error returns (false, err) so the caller treats "unknown" as "not yet reviewed" and
// proceeds — a real review is never skipped on a transient DB error.
func (s *CodeReviewService) reviewedHead(ctx context.Context, principalID, businessID, connID uuid.UUID, prNumber int, headSHA string, selfID uuid.UUID) (bool, error) {
	var exists bool
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM code_review
			   WHERE repo_connector_id = $1 AND pr_number = $2 AND head_sha = $3
			     AND status = 'succeeded' AND id <> $4 AND business_id = $5
			 )`,
			connID, prNumber, headSHA, selfID, businessID).Scan(&exists)
	})
	return exists, err
}

// finalizeSkipped marks a review 'succeeded' WITHOUT posting — used when the claim-time
// re-check finds a sibling already reviewed this exact head (fable C2). It writes a
// terminal row (empty summary, empty findings, nothing posted) so the worker treats the
// job as done and never requeues it.
//
// findings AND dimension_runs MUST be non-nil ("[]", not nil): both columns are jsonb
// NOT NULL, so a nil []byte encodes to SQL NULL → 23502 → the UPDATE aborts. If that
// error were swallowed the row would stay 'running', its lease would expire, and
// claim_code_reviews (which re-claims expired-running rows with NO attempts cap) would
// re-claim it forever — the worker never fails a job that returned nil. So the error is
// PROPAGATED (return err): a genuine DB failure then surfaces as a job error and the
// worker requeues it under the normal bounded-retry policy instead of looping.
func (s *CodeReviewService) finalizeSkipped(ctx context.Context, job ClaimedReview, headSHA, model string) error {
	return s.DB.WithPrincipal(ctx, job.PrincipalID, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID:                job.ID,
			Status:            "succeeded",
			HeadSha:           headSHA,
			Summary:           "",
			Findings:          []byte("[]"),
			ExternalReviewRef: "",
			PostedAt:          pgtype.Timestamptz{}, // NULL — nothing was posted
			TokensIn:          0,
			TokensOut:         0,
			CostCents:         0,
			DimensionRuns:     []byte("[]"),
			Model:             model,
			AgentRunID:        pgtype.UUID{}, // NULL — no agent_run for a skipped review
		}); err != nil {
			return err
		}
		return audit.Write(ctx, tx, codingAudit(job.BusinessID, job.PrincipalID, job.ID,
			"agent.coding.review.skipped_superseded",
			map[string]any{"pr": job.PRNumber, "head_sha": headSHA},
			nil, ptr("skipped"),
		))
	})
}

// auditStep opens a short transaction to write a standalone audit entry for steps
// that are not co-located with a DB mutation (e.g. "sandbox invoked").
func (s *CodeReviewService) auditStep(
	ctx context.Context,
	principalID, businessID, crID uuid.UUID,
	action string,
	inputs, outputs any,
	decision *string,
) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID, action, inputs, outputs, decision))
	})
}

// codingAudit builds an audit.Entry for a code_review lifecycle event.
func codingAudit(
	businessID, principalID, crID uuid.UUID,
	action string,
	inputs, outputs any,
	decision *string,
) audit.Entry {
	tt := "code_review"
	return audit.Entry{
		BusinessID:       &businessID,
		ActorPrincipalID: &principalID,
		Action:           action,
		TargetType:       &tt,
		TargetID:         &crID,
		Inputs:           inputs,
		Outputs:          outputs,
		Decision:         decision,
	}
}

// sandboxEnv builds the env the opencode entrypoint consumes. LLM_PROVIDER selects
// the opencode built-in provider (model prefix + auth.json key); LLM_BASE_URL is
// used only to derive the egress-allowlist host.
func sandboxEnv(cred AICredential) map[string]string {
	return map[string]string{
		"LLM_API_KEY":  cred.APIKey,
		"LLM_BASE_URL": cred.BaseURL,
		"LLM_MODEL":    cred.Model,
		"LLM_PROVIDER": cred.Provider,
	}
}

// opencodeCmd returns the sandbox command argv for the opencode runner.
// The real image ENTRYPOINT (/usr/local/bin/review) drives the full opencode
// invocation and writes /out/review.json, so no additional Cmd args are
// required. The provider (LLM_PROVIDER), model (LLM_MODEL), and key (LLM_API_KEY)
// are injected via env; the entrypoint maps them onto sst/opencode's
// `-m <provider>/<model>` flag and its provider-keyed auth.json respectively.
func opencodeCmd(_ string) []string {
	return []string{} // ENTRYPOINT runs the review; no extra Cmd needed
}

// reviewURL constructs a GitHub pull request review deep-link.
// Returns "" when repo or externalRef is empty (review not yet posted).
// Format: https://github.com/{repo}/pull/{pr}#pullrequestreview-{externalRef}
func reviewURL(repo string, pr int, externalRef string) string {
	if repo == "" || externalRef == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-%s", repo, pr, externalRef)
}

// sandboxUsage is the token usage the entrypoint extracts from opencode's session
// DB into /out/usage.json (sqlite3 -json output: a one-element array).
type sandboxUsage struct {
	Cost       float64 `json:"cost"` // opencode's own computed cost in USD (0 if it couldn't price the model)
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning"`
	CacheRead  int64   `json:"cache_read"`  // cached-prompt reads — the dominant token category for the agentic loop
	CacheWrite int64   `json:"cache_write"` // cache writes (first-turn prompt caching)
}

// codeReviewLaneMaxAttempts caps how many times a cloud lane runs: one initial attempt plus
// one retry, taken only on a clean-exit parse/empty failure (manyforge-6h1).
const codeReviewLaneMaxAttempts = 2

// addUsage sums two sandbox usage records — used to accumulate cost/tokens across a lane's
// retry attempts so a failed-then-succeeded lane bills for every attempt (manyforge-6h1).
func addUsage(a, b sandboxUsage) sandboxUsage {
	return sandboxUsage{
		Cost:       a.Cost + b.Cost,
		Input:      a.Input + b.Input,
		Output:     a.Output + b.Output,
		Reasoning:  a.Reasoning + b.Reasoning,
		CacheRead:  a.CacheRead + b.CacheRead,
		CacheWrite: a.CacheWrite + b.CacheWrite,
	}
}

// costCentsFromUsage returns opencode's own computed cost in cents, preferring it over
// catalog pricing because opencode prices cache-read tokens correctly (the host's token
// catalog does not). A zero cost means opencode couldn't price the model (e.g. a custom
// slug) → priced=false so the caller falls back to catalog pricing.
func costCentsFromUsage(u sandboxUsage) (cents int64, priced bool) {
	if u.Cost > 0 {
		return int64(math.Round(u.Cost * 100)), true
	}
	return 0, false
}

// errNoReviewOutput indicates the sandbox run's Outputs carried no review.json — e.g. a
// run that exited without the entrypoint ever writing findings.
var errNoReviewOutput = errors.New("coding: review.json not found in sandbox outputs")

// parseSandboxUsage parses usage.json content — the entrypoint's sqlite3 -json output: a
// one-element array of {cost, input, output, reasoning, cache_read, cache_write}.
// Best-effort: missing/empty/garbage → zero usage (a review is never failed for lack of
// usage data).
func parseSandboxUsage(b []byte) sandboxUsage {
	if len(b) == 0 {
		return sandboxUsage{}
	}
	var rows []sandboxUsage
	if jerr := json.Unmarshal(b, &rows); jerr != nil || len(rows) == 0 {
		return sandboxUsage{}
	}
	return rows[0]
}

// readSandboxUsage reads /out/usage.json from a host output directory. Thin wrapper over
// parseSandboxUsage for callers still working off a host directory rather than
// SandboxResult.Outputs (e.g. direct unit tests of the on-disk contract).
func readSandboxUsage(outDir string) sandboxUsage {
	b, err := os.ReadFile(filepath.Join(outDir, "usage.json"))
	if err != nil {
		return sandboxUsage{}
	}
	return parseSandboxUsage(b)
}

// clampInt32 bounds a token count to a non-negative int32 (the agent_run/code_review
// token columns are int4) — defends against a garbage usage value.
func clampInt32(n int64) int32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(n)
	}
}

// sandboxStderrTail returns a short tail of the container's stderr (opencode's output, now
// streamed to the container stderr and captured in SandboxResult.Stderr) for the failure
// last_error. Best-effort; empty when absent. Server-side diagnostic only — last_error is never
// returned to API clients.
func sandboxStderrTail(stderr []byte, secrets ...string) string {
	if len(stderr) == 0 {
		return ""
	}
	// opencode prints a long usage block after ANY error; keep the meaningful
	// error lines and drop the usage/spinner noise.
	var keep []string
	for _, ln := range strings.Split(string(stderr), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "Error") || strings.Contains(t, "Unauthorized") ||
			strings.Contains(t, "401") || strings.Contains(t, "failed") {
			keep = append(keep, t)
		}
	}
	s := strings.Join(keep, " ")
	if s == "" { // fallback: head of the raw stderr
		s = strings.TrimSpace(string(stderr))
	}
	const max = 600
	if len(s) > max {
		s = s[:max] + "…"
	}
	if s == "" {
		return ""
	}
	return " | sandbox stderr: " + redactSecrets(s, secrets...)
}

// progressStreamWriter forwards the sandbox's live stderr (opencode's tool-call narration) into
// the review Progress heartbeat, so a cloud review streams progress like the local path. It is
// driven by io.MultiWriter from exec's single stderr-copy goroutine (no concurrent writes here),
// and Progress.UpdateStream is itself synchronized. Only a tail is retained — the heartbeat
// preview is tail-capped anyway — so a long run does not hold the whole (multi-MB) stderr twice.
// Secrets are scrubbed downstream by Progress.Snapshot (via SetSecrets), so nothing sensitive is
// persisted even if the model echoes a key. dim prefixes the preview so the UI shows the live lane.
type progressStreamWriter struct {
	prog  *Progress
	dim   string
	buf   bytes.Buffer
	lines int
}

func (w *progressStreamWriter) Write(p []byte) (int, error) {
	w.lines += bytes.Count(p, []byte{'\n'})
	w.buf.Write(p)
	const keepTail = 8 << 10
	if w.buf.Len() > keepTail {
		tail := append([]byte(nil), w.buf.Bytes()[w.buf.Len()-keepTail:]...)
		w.buf.Reset()
		w.buf.Write(tail)
	}
	w.prog.UpdateStream(w.lines, w.dim+":\n"+w.buf.String())
	return len(p), nil
}

// ptr returns a pointer to the given string value.
func ptr(s string) *string { return &s }

// Ensure *appdb.DB satisfies serviceDB at compile time.
var _ serviceDB = (*appdb.DB)(nil)
