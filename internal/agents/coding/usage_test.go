package coding

import (
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
func TestCostCentsFromUsage(t *testing.T) {
	if cents, priced := costCentsFromUsage(sandboxUsage{Cost: 0.0582}); !priced || cents != 6 {
		t.Fatalf("cost=0.0582 → cents=%d priced=%v, want 6/true", cents, priced)
	}
	if cents, priced := costCentsFromUsage(sandboxUsage{Cost: 0, Input: 100}); priced || cents != 0 {
		t.Fatalf("cost=0 must fall through to catalog (priced=false), got cents=%d priced=%v", cents, priced)
	}
}

func TestClampInt32(t *testing.T) {
	cases := map[int64]int32{0: 0, 5: 5, -3: 0, 2147483647: 2147483647, 9000000000: 2147483647}
	for in, want := range cases {
		if got := clampInt32(in); got != want {
			t.Fatalf("clampInt32(%d) = %d, want %d", in, got, want)
		}
	}
}
