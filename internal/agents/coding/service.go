package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
	ID            uuid.UUID          `json:"id"`
	Status        string             `json:"status"`
	Summary       string             `json:"summary"`
	ReviewURL     string             `json:"review_url"`
	PRNumber      int                `json:"pr_number"`
	Model         string             `json:"model"`      // model snapshot used for this review
	Findings      []connectors.Finding `json:"findings"`
	FindingsCount int                `json:"findings_count"`
	CostCents     int64              `json:"cost_cents"` // LLM cost of the run (0 until usage capture lands)
	CreatedAt     time.Time          `json:"created_at"`
	PostedAt      *time.Time         `json:"posted_at"`
	// Progress is the live progress snapshot for a running review (phase/tokens/
	// preview); null/omitted for pending and terminal reviews. Populated from the
	// code_review.progress jsonb the worker heartbeat persists.
	Progress json.RawMessage `json:"progress,omitempty"`
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
func (s *CodeReviewService) runJob(ctx context.Context, job ClaimedReview, prog *Progress) error {
	// Re-resolve connector under the owning principal (no secrets in the queue row).
	rc, err := s.Repos.Resolve(ctx, job.PrincipalID, job.BusinessID, job.RepoConnectorID)
	if err != nil {
		return err
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

	// Set up per-run directories and clone the PR head.
	runDir := filepath.Join(s.WorkRoot, crID.String())
	checkout := filepath.Join(runDir, "checkout")
	outDir := filepath.Join(runDir, "out")
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

	authHeader := github.BasicAuthHeader(rc.Credential.APIToken)
	if err := s.cloneFn()(ctx, conn.CloneURL(), authHeader, pr.HeadSHA, checkout, rc.AllowPrivateBaseURL); err != nil {
		return s.failJob(ctx, principalID, businessID, crID, prNumber, err)
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
	payload, skippedFiles, omittedFiles, filteredFiles := assembleDiffPayload(files, maxTotal)
	// No silent caps: dropped files are surfaced in the review body AND recorded on the audit
	// trail (binary/too-large → skipped; over-budget → omitted; prose/plan docs → filtered).
	if len(skippedFiles) > 0 || len(omittedFiles) > 0 || len(filteredFiles) > 0 {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.review.files_dropped",
			map[string]any{"skipped": skippedFiles, "omitted": omittedFiles, "filtered_docs": filteredFiles},
			nil, ptr("executed"),
		)
	}
	// Obtain the findings doc + token usage. Two paths by provider:
	//   - local (Ollama/vLLM): review host-side via the direct-API path — small
	//     local models can't drive opencode's agent loop, and the model is on-host
	//     so there's nothing to isolate (manyforge-62s). Cost is 0 (local).
	//   - cloud: opencode in the hardened, egress-restricted sandbox.
	var doc FindingsDoc
	var tokensIn, tokensOut int32
	var costCents int64

	prog.SetPhase("reviewing")

	if isLocalProvider(cred.Provider) {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.localreview.invoked",
			map[string]any{"head_sha": pr.HeadSHA, "model": cred.Model, "base_url": cred.BaseURL},
			nil, ptr("executed"),
		)
		d, in, out, lerr := localReview(ctx, s.localClient(), cred, payload, prog)
		tokensIn, tokensOut = clampInt32(in), clampInt32(out) // local = no cost
		if lerr != nil {
			return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber, lerr, tokensIn, tokensOut, 0)
		}
		doc = d
	} else {
		// Scope the review to the changed files by writing their paths where the
		// entrypoint reads them (/out/review_files.txt). Absent → whole-repo review.
		if len(changed) > 0 {
			_ = os.WriteFile(filepath.Join(outDir, "review_files.txt"),
				[]byte(strings.Join(changedFilePaths(changed), "\n")), 0o644)
		}
		// Hand opencode the rendered diff (annotated hunks) as the primary review
		// scope; it may still open the full files in the read-only checkout for
		// context. Absent/empty → falls back to review_files.txt (whole-file scope).
		if payload != "" {
			_ = os.WriteFile(filepath.Join(outDir, "review_diff.txt"), []byte(payload), 0o644)
		}
		// Single-source the review prompt: write the SAME instructions the local path uses
		// (reviewInstructions) where the entrypoint reads them (/out/review_instructions.txt),
		// so local and cloud share one prompt and prompt changes need no sandbox-image rebuild.
		// The image's baked INSTRUCTIONS is only a fallback when this file is absent.
		_ = os.WriteFile(filepath.Join(outDir, "review_instructions.txt"), []byte(reviewInstructions), 0o644)
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.opencode.invoked",
			map[string]any{"image": s.Image, "head_sha": pr.HeadSHA, "model": cred.Model},
			nil, ptr("executed"),
		)
		spec := sandbox.SandboxSpec{
			Image:       s.Image,
			ReadOnlyDir: checkout,
			OutputDir:   outDir,
			Cmd:         opencodeCmd(cred.Model),
			Env:         sandboxEnv(cred),
			EgressAllow: []string{cred.Host()},
			Timeout:     s.timeout(),
		}
		res, rerr := s.Sandbox.Run(ctx, spec)

		// Capture usage as SOON as the sandbox returns — the model may have burned
		// tokens even when the run ultimately fails (e.g. unparseable output). opencode
		// reports 0 cost for a custom slug, so we price it via OpenRouter's catalog.
		// Best-effort: missing usage/pricing → zero. Every exit below records this so a
		// failed-but-billed run is still accounted (manyforge-7n5).
		usage := readSandboxUsage(outDir)
		tokensIn = clampInt32(usage.Input)
		tokensOut = clampInt32(usage.Output + usage.Reasoning)
		if s.Pricing != nil {
			if c, perr := s.Pricing.CostCents(ctx, cred.Provider, cred.Model, int64(tokensIn), int64(tokensOut)); perr == nil {
				costCents = c
			}
		}
		if rerr != nil {
			return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber, rerr, tokensIn, tokensOut, costCents)
		}
		rawFindings, ferr := os.ReadFile(filepath.Join(outDir, "review.json"))
		if ferr != nil {
			return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber,
				fmt.Errorf("coding: no findings produced (exit %d): %w%s", res.ExitCode, ferr, sandboxStderrTail(outDir, cred.APIKey, rc.Credential.APIToken)),
				tokensIn, tokensOut, costCents)
		}
		d, perr := ParseFindings(rawFindings)
		if perr != nil {
			return s.failJobWithUsage(ctx, principalID, businessID, job.AgentID, crID, prNumber,
				fmt.Errorf("%w%s", perr, sandboxStderrTail(outDir, cred.APIKey, rc.Credential.APIToken)), tokensIn, tokensOut, costCents)
		}
		doc = d
	}

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
			out = append(out, CodeReview{
				ID:            r.ID,
				Status:        r.Status,
				Summary:       r.Summary,
				PRNumber:      int(r.PrNumber),
				Model:         r.Model,
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
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
}

// readSandboxUsage reads /out/usage.json. Best-effort: missing/empty/garbage → zero
// usage (a review is never failed for lack of usage data).
func readSandboxUsage(outDir string) sandboxUsage {
	b, err := os.ReadFile(filepath.Join(outDir, "usage.json"))
	if err != nil {
		return sandboxUsage{}
	}
	var rows []sandboxUsage
	if jerr := json.Unmarshal(b, &rows); jerr != nil || len(rows) == 0 {
		return sandboxUsage{}
	}
	return rows[0]
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

// sandboxStderrTail returns a short tail of the sandbox's /out/stderr.log for the
// failure last_error (the entrypoint redirects opencode stderr there). Best-effort;
// empty when absent. Server-side diagnostic only — last_error is never returned to API clients.
func sandboxStderrTail(outDir string, secrets ...string) string {
	b, err := os.ReadFile(filepath.Join(outDir, "stderr.log"))
	if err != nil {
		return ""
	}
	// opencode prints a long usage block after ANY error; keep the meaningful
	// error lines and drop the usage/spinner noise.
	var keep []string
	for _, ln := range strings.Split(string(b), "\n") {
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
	if s == "" { // fallback: head of the raw log
		s = strings.TrimSpace(string(b))
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

// ptr returns a pointer to the given string value.
func ptr(s string) *string { return &s }

// Ensure *appdb.DB satisfies serviceDB at compile time.
var _ serviceDB = (*appdb.DB)(nil)
