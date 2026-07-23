package coding

import (
	"context"
	"encoding/json"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// estimateReviewWindow is how many recent SUCCEEDED reviews seed the per-lane cost heuristic.
const estimateReviewWindow = 30

// fallbackPerLaneMicroCents is the per-lane cost assumed when the business has NO review history to
// average yet — a deliberately rough placeholder (2¢) so the Review Setup page can still show an
// order-of-magnitude number, flagged by based_on_reviews == 0 so the UI can mark it approximate.
const fallbackPerLaneMicroCents int64 = 2_000_000

// ReviewCostEstimate is the pre-PR per-review cost projection shown on the Review Setup page
// (manyforge-8qs.3). It is an ESTIMATE: current enabled-lane count × the business's own observed
// average per-lane cost (or a fallback when there is no history yet). Latency isn't promised — the
// lane count + parallelism is the intuition the UI conveys.
type ReviewCostEstimate struct {
	LaneCount             int   `json:"lane_count"`               // enabled dimensions (+1 for the verify lane)
	VerifyEnabled         bool  `json:"verify_enabled"`           // whether a verify lane is included in LaneCount
	BasedOnReviews        int   `json:"based_on_reviews"`         // recent reviews averaged; 0 ⇒ fallback constant used
	PerLaneCostMicroCents int64 `json:"per_lane_cost_microcents"` // avg observed (or fallback) cost per lane
	EstCostMicroCents     int64 `json:"est_cost_microcents"`      // LaneCount × per-lane, sub-cent precise
	EstCostCents          int64 `json:"est_cost_cents"`           // EstCostMicroCents rounded to whole cents (display)
}

// avgPerLaneMicroCents averages the observed per-lane cost (cost_microcents) across the RAN lanes of
// the supplied reviews' dimension_runs blobs. A lane that was skipped or failed is ignored (it did
// not incur a representative cost). Returns the average and how many reviews contributed at least
// one ran lane. Pure — the unit test drives it directly.
func avgPerLaneMicroCents(runBlobs [][]byte) (avgMicroCents int64, sampledReviews int) {
	var totalMicro int64
	var lanes int64
	for _, blob := range runBlobs {
		var runs []dimensionRun
		if err := json.Unmarshal(blob, &runs); err != nil {
			continue // a malformed blob doesn't sink the estimate
		}
		contributed := false
		for _, r := range runs {
			if r.Status != "succeeded" {
				continue
			}
			totalMicro += r.CostMicroCents
			lanes++
			contributed = true
		}
		if contributed {
			sampledReviews++
		}
	}
	if lanes == 0 {
		return 0, sampledReviews
	}
	return totalMicro / lanes, sampledReviews
}

// buildEstimate assembles the estimate from a lane count, whether a verify lane is included, and the
// averaged history. With no history (sampledReviews == 0) it falls back to a rough per-lane constant.
func buildEstimate(enabledLanes int, verifyEnabled bool, avgMicroCents int64, sampledReviews int) ReviewCostEstimate {
	laneCount := enabledLanes
	if verifyEnabled {
		laneCount++
	}
	perLane := avgMicroCents
	if sampledReviews == 0 || perLane <= 0 {
		perLane = fallbackPerLaneMicroCents
	}
	estMicro := perLane * int64(laneCount)
	return ReviewCostEstimate{
		LaneCount:             laneCount,
		VerifyEnabled:         verifyEnabled,
		BasedOnReviews:        sampledReviews,
		PerLaneCostMicroCents: perLane,
		EstCostMicroCents:     estMicro,
		EstCostCents:          int64(math.Round(float64(estMicro) / 1e6)),
	}
}

// EstimateConfig projects a per-review cost for the business's CURRENT review config: the number of
// enabled dimensions (a default single "general" lane when none are configured) plus the verify
// lane, priced by the business's own recent observed per-lane cost. Ownership is enforced by the
// underlying RLS-scoped reads (WithPrincipal + business_id predicate) — a foreign business sees only
// its own (empty) data. (manyforge-8qs.3)
func (s *ReviewDimensionService) EstimateConfig(ctx context.Context, principalID, businessID uuid.UUID) (ReviewCostEstimate, error) {
	panel, err := s.ListPanel(ctx, principalID, businessID)
	if err != nil {
		return ReviewCostEstimate{}, err
	}
	enabled := 0
	for _, d := range panel {
		if d.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		enabled = 1 // a business with no configured dimensions still runs the default "general" lane
	}
	cfg, err := s.GetConfig(ctx, principalID, businessID)
	if err != nil {
		return ReviewCostEstimate{}, err
	}

	var runBlobs [][]byte
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		blobs, qerr := dbgen.New(tx).ListRecentDimensionRuns(ctx, dbgen.ListRecentDimensionRunsParams{
			BusinessID: businessID,
			Limit:      estimateReviewWindow,
		})
		if qerr != nil {
			return qerr
		}
		runBlobs = blobs
		return nil
	}); err != nil {
		return ReviewCostEstimate{}, err
	}

	avgMicro, sampled := avgPerLaneMicroCents(runBlobs)
	return buildEstimate(enabled, cfg.VerifyEnabled, avgMicro, sampled), nil
}
