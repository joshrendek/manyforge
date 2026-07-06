package coding

import (
	"os"
	"strings"
	"testing"
)

// TestLaneLimit pins the per-agent concurrency cap semantics (manyforge-k8e.2):
// a single-GPU self-host caps at 1, cloud at 4, unset falls back to the default,
// and out-of-range values are clamped defensively.
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

// TestFanOutUsesLaneLimit_SourcePin fails loudly if a refactor drops the per-agent cap
// and reverts the dimension-lane fan-out to a fixed constant — which would silently let
// a single-GPU reviewbot burst past its capacity (the whole point of manyforge-k8e.2).
func TestFanOutUsesLaneLimit_SourcePin(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	if !strings.Contains(string(src), "g.SetLimit(laneLimit(cred))") {
		t.Fatal("fan-out must call g.SetLimit(laneLimit(cred)) so per-agent max_concurrent_lanes is honored")
	}
}
