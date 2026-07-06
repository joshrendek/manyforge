package coding

import "testing"

// TestLaneLimit pins the per-agent concurrency cap semantics (manyforge-k8e.2):
// a single-GPU self-host caps at 1, cloud at 4, unset falls back to the default,
// and out-of-range values are clamped defensively.
//
// The end-to-end wiring (that this value actually bounds the fan-out) is guarded
// behaviorally by TestCodeReviewLanesRespectPerAgentCap, which observes the real
// fan-out width == 1 through runJob — so no brittle source-string pin is needed.
func TestLaneLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"explicit 1 (single-GPU LM Studio)", 1, 1},
		{"explicit 4 (cloud)", 4, 4},
		{"zero unset falls back to default", 0, defaultConcurrentLanes},
		{"negative falls back to default", -3, defaultConcurrentLanes},
		{"over cap clamps to 16", 99, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneLimit(AICredential{MaxConcurrentLanes: tc.in}); got != tc.want {
				t.Fatalf("laneLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
