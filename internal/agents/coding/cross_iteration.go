package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// crossIterationFingerprint is a LINE-INDEPENDENT identity for a finding across review iterations
// (manyforge-e54.1): file + (rule_id, or the title when no rule is cited). It deliberately omits the
// line number — a finding survives a diff that shifts its line between commits. It prefers the
// stable rule_id when present; otherwise it falls back to the (lowercased) title, so a reworded
// title breaks carryover matching — an inherent limitation of title-based identity. Persisted as a
// stable text column, so it is hashed (sha256 hex) rather than kept as the raw NUL-delimited key
// used by the in-memory dedupe/verify match keys.
func crossIterationFingerprint(f connectors.Finding) string {
	key := strings.TrimSpace(f.RuleID)
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(f.Title))
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(f.File)) + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// findingDelta is the NEW/CARRYOVER/RESOLVED classification of a review's findings against the PR's
// prior iterations (manyforge-e54.1).
type findingDelta struct {
	New       int // findings not seen in a prior iteration
	Carryover int // findings that were already flagged and are still present
	Resolved  int // fingerprints previously open, absent from this review
}

// summaryLine renders the one-line delta prepended to the review summary.
func (d findingDelta) summaryLine() string {
	return fmt.Sprintf("_Since the last review of this PR: %d new · %d carried over · %d resolved._", d.New, d.Carryover, d.Resolved)
}

// classifyCrossIteration compares the current review's findings to the PR's prior fingerprint set
// (fingerprint → status). It returns the delta and the DISTINCT current fingerprints (order
// preserved) to persist. Each distinct fingerprint is counted once — two findings that share a
// line-free fingerprint (same file+title, different lines) are one tracked issue.
func classifyCrossIteration(prior map[string]string, current []connectors.Finding) (findingDelta, []string) {
	var delta findingDelta
	seen := make(map[string]bool, len(current))
	currentFps := make([]string, 0, len(current))
	for _, f := range current {
		fp := crossIterationFingerprint(f)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		currentFps = append(currentFps, fp)
		if _, was := prior[fp]; was {
			delta.Carryover++
		} else {
			delta.New++
		}
	}
	for fp, status := range prior {
		if status == "open" && !seen[fp] {
			delta.Resolved++
		}
	}
	return delta, currentFps
}

// priorFindingFingerprints reads the fingerprints already seen for a PR (fingerprint → status),
// RLS-scoped. A read; safe to run in its own tx before the post.
func (s *CodeReviewService) priorFindingFingerprints(ctx context.Context, principalID, businessID uuid.UUID, repo string, prNumber int) (map[string]string, error) {
	prior := map[string]string{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListFindingSeen(ctx, dbgen.ListFindingSeenParams{
			BusinessID: businessID,
			Repo:       repo,
			PrNumber:   int32(prNumber),
		})
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			prior[r.Fingerprint] = r.Status
		}
		return nil
	})
	return prior, err
}

// persistFindingSeen upserts the current review's fingerprints (as open) and resolves the ones no
// longer present. Runs on the finalize tx's Queries so it commits atomically with the review row.
func persistFindingSeen(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID, repo string, prNumber int, headSHA string, currentFps []string) error {
	for _, fp := range currentFps {
		if err := q.UpsertFindingSeen(ctx, dbgen.UpsertFindingSeenParams{
			ID:          uuid.New(),
			BusinessID:  businessID,
			Repo:        repo,
			PrNumber:    int32(prNumber),
			Fingerprint: fp,
			HeadSha:     headSHA,
		}); err != nil {
			return err
		}
	}
	if _, err := q.MarkFindingsResolved(ctx, dbgen.MarkFindingsResolvedParams{
		BusinessID:          businessID,
		Repo:                repo,
		PrNumber:            int32(prNumber),
		CurrentFingerprints: currentFps,
	}); err != nil {
		return err
	}
	return nil
}
