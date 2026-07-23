package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// verifySettings is the review_config subset that drives the optional verify pass (manyforge-8qs.1).
// A blank Provider means "run the verifier on the review's default credential, with Model overriding".
type verifySettings struct {
	Enabled  bool
	Provider string
	Model    string
}

// reviewRuntimeConfig is the review_config subset the worker needs at run time: the verify-pass
// settings (8qs.1) and the cite-rules flag (8qs.2). Loaded once per job.
type reviewRuntimeConfig struct {
	Verify    verifySettings
	CiteRules bool
}

// resolveReviewRuntimeConfig loads the business's run-time review settings from review_config,
// degrading to the ZERO value (verify disabled, cite-rules off) on a missing row or any error — a
// config-read hiccup must never block a review, never silently enable dropping, and never silently
// enable rule seeding. Mirrors resolveReviewChain's read-and-degrade pattern; the row is small and
// read once per job.
func (s *CodeReviewService) resolveReviewRuntimeConfig(ctx context.Context, principalID, businessID uuid.UUID) reviewRuntimeConfig {
	var rc reviewRuntimeConfig
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetReviewConfig(ctx, businessID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no config row ⇒ zero value (verify off, cite-rules off)
		}
		if err != nil {
			return err
		}
		rc.Verify.Enabled = row.VerifyEnabled
		rc.Verify.Model = row.VerifyModel
		if row.VerifyProvider.Valid {
			rc.Verify.Provider = string(row.VerifyProvider.AiProvider)
		}
		rc.CiteRules = row.CiteRules
		return nil
	}); err != nil {
		slog.Default().WarnContext(ctx, "coding: resolve review runtime config failed, using defaults",
			"err", err, "business_id", businessID)
		return reviewRuntimeConfig{}
	}
	return rc
}

// laneEnv augments a lane's sandbox env with the cite-rules flag when enabled, so the entrypoint
// seeds the reviewed repo's own rule docs into the prompt (manyforge-8qs.2). Mutates and returns
// the same map for call-site brevity.
func laneEnv(env map[string]string, citeRules bool) map[string]string {
	if citeRules {
		env["CITE_RULES"] = "1"
	}
	return env
}

// verifyDimensionKey is the lane Key used for the verify pass — surfaced in dimension_runs
// accounting and the opencode.invoked audit so the verifier's cost/status is visible.
const verifyDimensionKey = "verify"

// verifyInstructions frames the verify lane as a false-positive filter. The sandbox entrypoint
// appends the findings-schema directive, so the verifier's output is a normal findings doc — we
// treat it as the CONFIRMED subset and drop every original candidate it did not echo back.
const verifyInstructions = `You are a meticulous senior engineer performing a VERIFICATION pass over the candidate findings of an automated code review. You are given the same changed code (as unified-diff hunks) plus a numbered list of CANDIDATE FINDINGS produced by an earlier pass.

For each candidate, decide whether it is a TRUE POSITIVE — a real correctness, security, or robustness issue supported by the diff — or a FALSE POSITIVE (wrong, already handled, not supported by the code, or pure style/speculation).

Return ONLY the candidates you CONFIRM as true positives. For each confirmed candidate, echo its file, line, severity, and title EXACTLY as given in the candidate list — do not reword, renumber, merge, or re-severity them. Omit every false positive. Do NOT invent new findings; only ever return a subset of the candidates below. If every candidate is a false positive, return an empty findings array.`

// buildVerifyPrompt renders the verify instructions plus the numbered candidate findings the
// verifier must adjudicate. It is embedded as the lane's review_instructions.txt; the diff hunks
// are supplied separately (review_diff.txt) by the shared lane machinery.
func buildVerifyPrompt(findings []connectors.Finding) string {
	var b strings.Builder
	b.WriteString(verifyInstructions)
	fmt.Fprintf(&b, "\n\nCANDIDATE FINDINGS TO VERIFY (%d):\n", len(findings))
	for i, f := range findings {
		line := "null"
		if f.Line != nil {
			line = strconv.Itoa(*f.Line)
		}
		fmt.Fprintf(&b, "[%d] file=%s line=%s severity=%s title=%q\n    detail: %s\n",
			i+1, f.File, line, f.Severity, f.Title, singleLine(f.Detail))
	}
	return b.String()
}

// singleLine collapses newlines so one candidate finding stays on its own lines in the prompt.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// findingMatchKey mirrors dedupeFindings' identity key (file + line + lowercased title) so a
// verifier's echoed finding matches the original it confirms. Line-inclusive because two findings
// on the same file/title but different lines are distinct.
func findingMatchKey(f connectors.Finding) string {
	line := -1
	if f.Line != nil {
		line = *f.Line
	}
	return strings.TrimSpace(f.File) + "\x00" + strconv.Itoa(line) + "\x00" +
		strings.ToLower(strings.TrimSpace(f.Title))
}

// applyVerifyDrops partitions the original findings into kept (a matching key appears in the
// verifier's confirmed set) and dropped (no match). It only ever RETAINS original finding objects
// — the verifier can remove a finding but never alter or add one — so a reworded echo can at worst
// drop a real finding (guarded by fail-open at the call site), never corrupt or fabricate data.
func applyVerifyDrops(original, confirmed []connectors.Finding) (kept, dropped []connectors.Finding) {
	confirmedKeys := make(map[string]struct{}, len(confirmed))
	for _, c := range confirmed {
		confirmedKeys[findingMatchKey(c)] = struct{}{}
	}
	for _, f := range original {
		if _, ok := confirmedKeys[findingMatchKey(f)]; ok {
			kept = append(kept, f)
		} else {
			dropped = append(dropped, f)
		}
	}
	return kept, dropped
}

// verifyOutcome resolves the verify lane's result into the findings to keep. FAIL OPEN: if the
// verify lane errored (sandbox failure, timeout, unparseable output), keep ALL original findings —
// a broken verifier must never silently swallow real findings. A successful (even empty) result is
// trusted: an empty confirmed set means the verifier judged every candidate a false positive.
func verifyOutcome(original []connectors.Finding, vres laneResult) (kept, dropped []connectors.Finding, failedOpen bool) {
	if vres.Err != nil {
		return original, nil, true
	}
	kept, dropped = applyVerifyDrops(original, vres.Doc.Findings)
	return kept, dropped, false
}
