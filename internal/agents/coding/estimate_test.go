package coding

import (
	"encoding/json"
	"testing"
)

func runsBlob(t *testing.T, runs ...dimensionRun) []byte {
	t.Helper()
	b, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAvgPerLaneMicroCents_AveragesOnlySucceededLanes(t *testing.T) {
	blobs := [][]byte{
		// review 1: two succeeded lanes (1_000_000 + 3_000_000) + one skipped (ignored)
		runsBlob(t,
			dimensionRun{Status: "succeeded", CostMicroCents: 1_000_000},
			dimensionRun{Status: "succeeded", CostMicroCents: 3_000_000},
			dimensionRun{Status: "skipped", CostMicroCents: 0},
		),
		// review 2: one succeeded lane (2_000_000) + one failed (ignored)
		runsBlob(t,
			dimensionRun{Status: "succeeded", CostMicroCents: 2_000_000},
			dimensionRun{Status: "failed", CostMicroCents: 9_999_999},
		),
	}
	// (1M + 3M + 2M) / 3 succeeded lanes = 2M.
	avg, sampled := avgPerLaneMicroCents(blobs)
	if avg != 2_000_000 {
		t.Errorf("avg per lane = %d, want 2_000_000 (only succeeded lanes)", avg)
	}
	if sampled != 2 {
		t.Errorf("sampled reviews = %d, want 2", sampled)
	}
}

func TestAvgPerLaneMicroCents_MalformedBlobSkipped_NoLanesZero(t *testing.T) {
	blobs := [][]byte{[]byte("not json"), runsBlob(t, dimensionRun{Status: "skipped"})}
	avg, sampled := avgPerLaneMicroCents(blobs)
	if avg != 0 {
		t.Errorf("no ran lanes ⇒ avg 0; got %d", avg)
	}
	if sampled != 0 {
		t.Errorf("a review with only skipped lanes contributes nothing; sampled=%d want 0", sampled)
	}
}

func TestBuildEstimate_UsesHistoryTimesLaneCountPlusVerify(t *testing.T) {
	// 3 enabled dims + verify lane = 4 lanes; observed 2M/lane ⇒ 8M µ¢ = 8¢.
	est := buildEstimate(3, true, 2_000_000, 5)
	if est.LaneCount != 4 {
		t.Errorf("lane_count = %d, want 4 (3 dims + verify)", est.LaneCount)
	}
	if est.PerLaneCostMicroCents != 2_000_000 {
		t.Errorf("per-lane = %d, want observed 2_000_000", est.PerLaneCostMicroCents)
	}
	if est.EstCostMicroCents != 8_000_000 || est.EstCostCents != 8 {
		t.Errorf("est = %dµ¢/%d¢, want 8_000_000/8", est.EstCostMicroCents, est.EstCostCents)
	}
	if est.BasedOnReviews != 5 {
		t.Errorf("based_on_reviews = %d, want 5", est.BasedOnReviews)
	}
}

func TestBuildEstimate_FallbackWhenNoHistory(t *testing.T) {
	// No history (sampled 0) ⇒ fallback per-lane constant; 2 dims, no verify.
	est := buildEstimate(2, false, 0, 0)
	if est.PerLaneCostMicroCents != fallbackPerLaneMicroCents {
		t.Errorf("no history must use the fallback per-lane constant; got %d", est.PerLaneCostMicroCents)
	}
	if est.LaneCount != 2 {
		t.Errorf("lane_count = %d, want 2 (no verify lane)", est.LaneCount)
	}
	if est.EstCostMicroCents != 2*fallbackPerLaneMicroCents {
		t.Errorf("est = %d, want 2×fallback", est.EstCostMicroCents)
	}
	if est.BasedOnReviews != 0 {
		t.Errorf("based_on_reviews must be 0 so the UI marks it approximate; got %d", est.BasedOnReviews)
	}
}
