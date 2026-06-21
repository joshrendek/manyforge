package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
)

// CodeReview is the service-layer view of a code_review row.
type CodeReview struct {
	ID        uuid.UUID
	Status    string
	Summary   string
	ReviewURL string
	PRNumber  int
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

// CodeReviewService orchestrates the code-review agent lifecycle:
// resolve connector → resolve AI credential → insert pending row → FetchPR
// → clone → run sandbox → parse findings → post review → finalize.
type CodeReviewService struct {
	DB       serviceDB
	Repos    repoResolver
	Sandbox  sandbox.SandboxRunner
	Creds    AICredentialResolver
	Image    string        // opencode sandbox image tag
	WorkRoot string        // host temp root for per-run checkouts; must be writable
	Timeout  time.Duration // sandbox wall-clock cap (0 = 10 min default)

	// Clone is the injectable seam for cloning a repo at a specific SHA.
	// Defaults to coding.CloneAtSHA when nil (set at call time). Tests inject
	// a fake that just creates the directory without needing a real git server.
	Clone func(ctx context.Context, cloneURL, authHeader, sha, destDir string) error
}

// cloneFn returns the effective clone function (injectable seam or real default).
func (s *CodeReviewService) cloneFn() func(ctx context.Context, cloneURL, authHeader, sha, destDir string) error {
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

// Trigger runs the full code-review pipeline for the given pull request and
// returns the persisted CodeReview on success. Any failure after the initial
// insert is recorded as status="failed" in the DB before the error is returned.
func (s *CodeReviewService) Trigger(
	ctx context.Context,
	principalID, businessID, agentID, repoConnectorID uuid.UUID,
	prNumber int,
) (CodeReview, error) {

	// 1. Resolve repo connector (RLS-scoped).
	rc, err := s.Repos.Resolve(ctx, principalID, businessID, repoConnectorID)
	if err != nil {
		return CodeReview{}, err
	}

	// 2. Build GitHub connector client.
	conn, err := github.NewFactory(60 * time.Second)(rc)
	if err != nil {
		return CodeReview{}, fmt.Errorf("coding: build connector client: %w", err)
	}

	// 3. Resolve AI credential (RLS-scoped). Must have a non-empty host.
	cred, err := s.Creds.Resolve(ctx, principalID, businessID, agentID)
	if err != nil {
		return CodeReview{}, err
	}
	if cred.Host() == "" {
		return CodeReview{}, fmt.Errorf("coding: agent has no usable AI credential: %w", errs.ErrValidation)
	}

	// 4. Insert pending code_review + audit "agent.coding.review.requested" in one tx.
	crID := uuid.New()
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if _, ierr := dbgen.New(tx).InsertCodeReview(ctx, dbgen.InsertCodeReviewParams{
			ID:              crID,
			AgentRunID:      pgtype.UUID{}, // NULL — no agent_run tracking per arch decision
			RepoConnectorID: repoConnectorID,
			PrNumber:        int32(prNumber),
			BusinessID:      businessID,
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

	// From here on, any error must call s.fail to mark the row failed.

	// 5. Fetch PR metadata (host-side, uses the credential).
	pr, err := conn.FetchPR(ctx, prNumber)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 6. Set up per-run directories and clone the PR head.
	runDir := filepath.Join(s.WorkRoot, crID.String())
	checkout := filepath.Join(runDir, "checkout")
	outDir := filepath.Join(runDir, "out")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}
	defer func() { _ = os.RemoveAll(runDir) }() // always clean up regardless of outcome

	authHeader := github.BasicAuthHeader(rc.Credential.APIToken)
	if err := s.cloneFn()(ctx, conn.CloneURL(), authHeader, pr.HeadSHA, checkout); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 7. Audit sandbox invocation, then run opencode in the isolated sandbox.
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
		Env: map[string]string{
			"LLM_API_KEY":  cred.APIKey,
			"LLM_BASE_URL": cred.BaseURL,
			"LLM_MODEL":    cred.Model,
		},
		EgressAllow: []string{cred.Host()},
		Timeout:     s.timeout(),
	}
	res, err := s.Sandbox.Run(ctx, spec)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 8. Read and parse findings from /out/review.json.
	rawFindings, err := os.ReadFile(filepath.Join(outDir, "review.json"))
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber,
			fmt.Errorf("coding: no findings produced (exit %d): %w", res.ExitCode, err))
	}
	doc, err := ParseFindings(rawFindings)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 9. Post the review to the PR (intentionally ungated — advisory only).
	body := RenderMarkdown(doc)
	ref, err := conn.PostReview(ctx, prNumber, connectors.Review{
		Summary:  doc.Summary,
		Findings: doc.Findings,
		Body:     body,
	})
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 10. Finalize: UpdateCodeReviewResult (succeeded) + audit "agent.coding.review.posted" in one tx.
	findingsJSON, _ := json.Marshal(doc.Findings)
	postedAt := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}

	var out CodeReview
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, uerr := dbgen.New(tx).UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID:                crID,
			Status:            "succeeded",
			HeadSha:           pr.HeadSHA,
			Summary:           doc.Summary,
			Findings:          findingsJSON,
			ExternalReviewRef: ref.ExternalID,
			PostedAt:          postedAt,
		})
		if uerr != nil {
			return uerr
		}
		out = CodeReview{
			ID:        row.ID,
			Status:    row.Status,
			Summary:   row.Summary,
			ReviewURL: ref.URL,
			PRNumber:  prNumber,
		}
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID,
			"agent.coding.review.posted",
			nil,
			map[string]any{"review_url": ref.URL, "findings": len(doc.Findings)},
			ptr("posted"),
		))
	}); err != nil {
		return CodeReview{}, err
	}
	return out, nil
}

// Get loads a code_review by id, scoped to the business for RLS defense-in-depth.
// Cross-tenant or unknown id → ErrNotFound.
func (s *CodeReviewService) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (CodeReview, error) {
	var out CodeReview
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetCodeReview(ctx, dbgen.GetCodeReviewParams{
			ID:         id,
			BusinessID: businessID,
		})
		if err != nil {
			return err
		}
		reviewURL := ""
		if row.ExternalReviewRef != "" {
			// ExternalReviewRef is the numeric review ID; reviewURL is populated
			// from the connector post response which we don't re-fetch here.
			// Callers that need the URL should use the Trigger return value or
			// read ExternalReviewRef and construct the URL from the connector config.
			reviewURL = ""
		}
		out = CodeReview{
			ID:        row.ID,
			Status:    row.Status,
			Summary:   row.Summary,
			ReviewURL: reviewURL,
			PRNumber:  int(row.PrNumber),
		}
		return nil
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CodeReview{}, fmt.Errorf("coding: code review not found: %w", errs.ErrNotFound)
		}
		return CodeReview{}, fmt.Errorf("coding: get code review: %w", err)
	}
	return out, nil
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

// opencodeCmd returns the sandbox command argv for the opencode runner.
// The real image ENTRYPOINT (/usr/local/bin/review) drives the full
// opencode invocation and writes /out/review.json, so no additional Cmd
// args are required.  model is injected via the LLM_MODEL env var; the
// entrypoint's opencode.json uses {env:LLM_MODEL} substitution.
func opencodeCmd(_ string) []string {
	return []string{} // ENTRYPOINT runs the review; no extra Cmd needed
}

// ptr returns a pointer to the given string value.
func ptr(s string) *string { return &s }

// Ensure *appdb.DB satisfies serviceDB at compile time.
var _ serviceDB = (*appdb.DB)(nil)
