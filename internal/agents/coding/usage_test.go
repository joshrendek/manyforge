package coding

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The entrypoint writes /out/usage.json as sqlite3 -json output: a one-element
// array of {input, output, reasoning}. readSandboxUsage must parse that and degrade
// to zero on any absence/garbage (a review is never failed for missing usage).
func TestReadSandboxUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.json"),
		[]byte(`[{"input":1200,"output":340,"reasoning":56}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	u := readSandboxUsage(dir)
	if u.Input != 1200 || u.Output != 340 || u.Reasoning != 56 {
		t.Fatalf("parsed usage = %+v", u)
	}

	// Missing file → zero.
	if got := readSandboxUsage(t.TempDir()); got != (sandboxUsage{}) {
		t.Fatalf("missing usage.json should be zero, got %+v", got)
	}
	// Empty array (no session row) → zero.
	d2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(d2, "usage.json"), []byte(`[]`), 0o644)
	if got := readSandboxUsage(d2); got != (sandboxUsage{}) {
		t.Fatalf("empty array should be zero, got %+v", got)
	}
}

// usage.json now also carries opencode's OWN computed cost (dollars) plus the cache
// token breakdown; readSandboxUsage must surface them so the host can bill accurately
// (cache-read tokens are the dominant, previously-ignored cost of the agentic loop).
func TestReadSandboxUsage_CostAndCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.json"),
		[]byte(`[{"cost":0.0582,"input":9886,"output":404,"reasoning":3539,"cache_read":205696,"cache_write":12}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	u := readSandboxUsage(dir)
	if u.Cost != 0.0582 || u.CacheRead != 205696 || u.CacheWrite != 12 {
		t.Fatalf("parsed usage = %+v", u)
	}
}

// costCentsFromUsage prefers opencode's own cost (it prices cache reads correctly).
// A zero cost means opencode couldn't price the model (custom slug) → not "priced",
// so the caller falls back to catalog pricing.
// addUsage must sum every field so a lane that fails then succeeds on retry bills for
// both attempts (manyforge-6h1) — cost + all token categories, not just a subset.
func TestAddUsage(t *testing.T) {
	a := sandboxUsage{Cost: 0.10, Input: 100, Output: 10, Reasoning: 5, CacheRead: 200, CacheWrite: 3}
	b := sandboxUsage{Cost: 0.05, Input: 40, Output: 8, Reasoning: 2, CacheRead: 60, CacheWrite: 1}
	got := addUsage(a, b)
	// Cost is a float sum (compared within epsilon; production rounds it via math.Round);
	// token categories are exact integer sums.
	if math.Abs(got.Cost-0.15) > 1e-9 {
		t.Errorf("Cost = %v, want ~0.15", got.Cost)
	}
	if got.Input != 140 || got.Output != 18 || got.Reasoning != 7 || got.CacheRead != 260 || got.CacheWrite != 4 {
		t.Errorf("token sums wrong: %+v", got)
	}
	// identity: adding a zero record changes nothing (first attempt of a lane).
	if got := addUsage(sandboxUsage{}, a); got != a {
		t.Fatalf("addUsage(zero, a) = %+v, want %+v", got, a)
	}
}

func TestMicroCentsFromUsage(t *testing.T) {
	// $0.0582 = 5.82¢ = 5_820_000 micro-cents (kept precise; the review total rounds later).
	if mc, priced := microCentsFromUsage(sandboxUsage{Cost: 0.0582}); !priced || mc != 5_820_000 {
		t.Fatalf("cost=0.0582 → microCents=%d priced=%v, want 5_820_000/true", mc, priced)
	}
	// Sub-cent lane (the threat.gg case): 0.2026¢ must survive as 202_600 µ¢, NOT round to 0.
	if mc, priced := microCentsFromUsage(sandboxUsage{Cost: 0.002026}); !priced || mc != 202_600 {
		t.Fatalf("cost=0.002026 → microCents=%d priced=%v, want 202_600/true (sub-cent must not vanish)", mc, priced)
	}
	// Zero cost → opencode couldn't price the model; caller falls back to the catalog.
	if mc, priced := microCentsFromUsage(sandboxUsage{Cost: 0, Input: 100}); priced || mc != 0 {
		t.Fatalf("cost=0 must fall through to catalog (priced=false), got microCents=%d priced=%v", mc, priced)
	}
}

// TestRoundMicroCentsToCents pins round-half-UP and the never-negative clamp — the review total is
// rounded here exactly once. (Disproves the auto-review claim that 500_000 µ¢ rounds down.)
func TestRoundMicroCentsToCents(t *testing.T) {
	for _, c := range []struct {
		micro, want int64
	}{
		{0, 0},
		{1, 0},
		{499_999, 0},                   // just below half a cent → 0
		{500_000, 1},                   // exactly half a cent → rounds UP to 1
		{500_001, 1},
		{999_999, 1},
		{1_000_000, 1},                 // exactly 1¢
		{1_499_999, 1},
		{1_500_000, 2},                 // 1.5¢ → 2 (round half up)
		{1_721_600, 2},                 // the threat.gg #36 total
		{-1, 0},                        // cost is never negative → clamp to 0
		{1_000_000_000_000, 1_000_000}, // 1e12 µ¢ = 1e6¢, no overflow
	} {
		if got := roundMicroCentsToCents(c.micro); got != c.want {
			t.Errorf("roundMicroCentsToCents(%d) = %d, want %d", c.micro, got, c.want)
		}
	}
}

// stubEstimator is a CostEstimator whose CostMicroCents returns a fixed value (and records that it
// was consulted) so the lane-cost branch can be exercised in isolation.
type stubEstimator struct {
	microCents int64
	err        error
	called     bool
}

func (s *stubEstimator) CostCents(context.Context, string, string, int64, int64) (int64, error) {
	return s.microCents / microCentsPerCent, s.err
}
func (s *stubEstimator) CostMicroCents(context.Context, string, string, int64, int64) (int64, error) {
	s.called = true
	return s.microCents, s.err
}

// TestLaneCostMicroCents pins the four-way per-lane pricing branch — the path that sets the cost
// recorded for every review lane.
func TestLaneCostMicroCents(t *testing.T) {
	ctx := context.Background()

	t.Run("opencode priced the lane → use its sub-cent cost, catalog untouched", func(t *testing.T) {
		est := &stubEstimator{microCents: 999_999}
		got := laneCostMicroCents(ctx, sandboxUsage{Cost: 0.002026}, est, "openrouter", "m", 1, 1)
		if got != 202_600 {
			t.Fatalf("got %d, want 202600 (opencode's own cost)", got)
		}
		if est.called {
			t.Fatal("catalog must NOT be consulted when opencode priced the lane")
		}
	})
	t.Run("opencode couldn't price → catalog fallback keeps sub-cent precision", func(t *testing.T) {
		est := &stubEstimator{microCents: 202_600}
		got := laneCostMicroCents(ctx, sandboxUsage{Cost: 0, Input: 100}, est, "openrouter", "m", 1, 1)
		if got != 202_600 {
			t.Fatalf("got %d, want 202600 (catalog micro-cents, not rounded to 0)", got)
		}
		if !est.called {
			t.Fatal("catalog must be consulted when opencode couldn't price the model")
		}
	})
	t.Run("catalog error → 0 (usage never fails a review)", func(t *testing.T) {
		est := &stubEstimator{err: errors.New("boom")}
		if got := laneCostMicroCents(ctx, sandboxUsage{Cost: 0}, est, "openrouter", "m", 1, 1); got != 0 {
			t.Fatalf("got %d, want 0 on pricing error", got)
		}
	})
	t.Run("nil pricing seam → 0", func(t *testing.T) {
		if got := laneCostMicroCents(ctx, sandboxUsage{Cost: 0}, nil, "openrouter", "m", 1, 1); got != 0 {
			t.Fatalf("got %d, want 0 with nil pricing", got)
		}
	})
}

func TestClampInt32(t *testing.T) {
	cases := map[int64]int32{0: 0, 5: 5, -3: 0, 2147483647: 2147483647, 9000000000: 2147483647}
	for in, want := range cases {
		if got := clampInt32(in); got != want {
			t.Fatalf("clampInt32(%d) = %d, want %d", in, got, want)
		}
	}
}
