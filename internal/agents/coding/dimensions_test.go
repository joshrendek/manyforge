package coding

import (
	"errors"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "internal/agents/coding/service.go", true},
		{"**/*.go", "main.go", true}, // ** matches zero segments
		{"**/*.go", "web/src/app.ts", false},
		{"frontend/**", "frontend/src/app/app.component.ts", true},
		{"frontend/**", "frontend", true},
		{"frontend/**", "internal/svc.go", false},
		{"*.go", "main.go", true},
		{"*.go", "internal/svc.go", false}, // bare * does not cross a separator
		{"**/*.sql", "db/query/code_review.sql", true},
		{"**/*.tsx", "web/src/x.tsx", true},
		{"web/**/*.ts", "web/src/app/app.ts", true},
		{"web/**/*.ts", "web/app.ts", true}, // ** matches zero middle segments
		{"web/**/*.ts", "internal/app.ts", false},
		{"docs/**", "./docs/plan.md", true}, // leading ./ tolerated
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchesScope(t *testing.T) {
	paths := []string{"internal/agents/coding/service.go", "README.md"}
	if !matchesScope(nil, paths) {
		t.Fatal("empty globs must match everything")
	}
	if !matchesScope([]string{"**/*.go"}, paths) {
		t.Fatal("**/*.go should match the .go file")
	}
	if matchesScope([]string{"frontend/**"}, paths) {
		t.Fatal("frontend/** should not match a backend-only change set")
	}
	if !matchesScope([]string{"frontend/**", "**/*.go"}, paths) {
		t.Fatal("any matching glob should activate the dimension")
	}
}

func TestActiveDimensions(t *testing.T) {
	dims := []Dimension{
		{Key: "security", Order: 1, Enabled: true},                                // no scope → all
		{Key: "ui", Order: 2, Enabled: true, ScopeGlobs: []string{"frontend/**"}}, // scoped out
		{Key: "docs", Order: 3, Enabled: false},                                   // disabled
		{Key: "perf", Order: 4, Enabled: true, ScopeGlobs: []string{"**/*.go"}},   // in scope
	}
	active, skipped := activeDimensions(dims, []string{"internal/svc.go"})
	if len(active) != 2 || active[0].Key != "security" || active[1].Key != "perf" {
		t.Fatalf("active=%v, want [security perf] in order", keysOf(active))
	}
	sk := map[string]string{}
	for _, s := range skipped {
		sk[s.Key] = s.Reason
	}
	if sk["ui"] == "" || sk["docs"] != "disabled" {
		t.Fatalf("skipped reasons wrong: %v", sk)
	}
	if sk["ui"] != "scope: no matching files" {
		t.Fatalf("ui skip reason = %q", sk["ui"])
	}
}

func keysOf(dims []Dimension) []string {
	out := make([]string, len(dims))
	for i, d := range dims {
		out[i] = d.Key
	}
	return out
}

func TestApplySeverityFloor(t *testing.T) {
	findings := []connectors.Finding{
		{Title: "a", Severity: "info"},
		{Title: "b", Severity: "warning"},
		{Title: "c", Severity: "error"},
	}
	got := applySeverityFloor(findings, "warning")
	if len(got) != 2 || got[0].Title != "b" || got[1].Title != "c" {
		t.Fatalf("floor=warning kept %v, want [b c]", titlesOf(got))
	}
	if len(applySeverityFloor(findings, "info")) != 3 {
		t.Fatal("floor=info should keep all")
	}
	if len(applySeverityFloor(findings, "error")) != 1 {
		t.Fatal("floor=error should keep only the error")
	}
}

func titlesOf(fs []connectors.Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Title
	}
	return out
}

func TestDedupeFindings(t *testing.T) {
	findings := []connectors.Finding{
		{File: "a.go", Line: iptr(10), Severity: "warning", Title: "SQL injection", Dimension: "security"},
		{File: "a.go", Line: iptr(10), Severity: "error", Title: "sql injection", Dimension: "correctness"}, // dup (case-insensitive), higher sev
		{File: "a.go", Line: iptr(20), Severity: "info", Title: "SQL injection", Dimension: "security"},     // different line → kept
		{File: "b.go", Line: nil, Severity: "info", Title: "naming", Dimension: "docs"},
	}
	got := dedupeFindings(findings)
	if len(got) != 3 {
		t.Fatalf("dedupe kept %d, want 3: %v", len(got), titlesOf(got))
	}
	// The merged finding keeps the higher severity and unions dimensions.
	if got[0].Severity != "error" {
		t.Fatalf("merged severity = %q, want error (max)", got[0].Severity)
	}
	if got[0].Dimension != "correctness, security" {
		t.Fatalf("merged dimensions = %q, want 'correctness, security'", got[0].Dimension)
	}
}

// TestDefaultPanel pins the ZERO-CONFIG default (spec 008): a business that has configured
// no dimensions gets a SINGLE general lane — the default reviewInstructions, all files, no
// severity floor — so a default review is byte-for-byte the pre-panel single-agent review
// (no cost/latency regression). It must NOT be the multi-specialist dimensionCatalog.
// TestAggregateReview pins the fan-out aggregation (spec 008 FR-005/FR-013): per-lane
// severity floors, dimension tagging (general lane left untagged), cross-lane dedupe, summed
// usage across ALL lanes (incl. failed-but-billed), and partial-success semantics.
func TestAggregateReview(t *testing.T) {
	sec := Dimension{Key: "security", MinSeverity: "warning"}
	corr := Dimension{Key: "correctness", MinSeverity: "info"}
	gen := Dimension{Key: generalDimensionKey, MinSeverity: "info"}

	t.Run("single general lane is untagged and preserves the doc", func(t *testing.T) {
		res := []laneResult{{
			Dim:      gen,
			TokensIn: 100, TokensOut: 20,
			Doc: FindingsDoc{Summary: "LGTM", Findings: []connectors.Finding{
				{File: "a.go", Line: iptr(1), Severity: "info", Title: "x"},
			}},
		}}
		doc, in, out, cost, err := aggregateReview(res)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if doc.Summary != "LGTM" {
			t.Fatalf("summary=%q", doc.Summary)
		}
		if len(doc.Findings) != 1 || doc.Findings[0].Dimension != "" {
			t.Fatalf("general lane must leave findings untagged (legacy shape): %+v", doc.Findings)
		}
		if in != 100 || out != 20 || cost != 0 {
			t.Fatalf("usage in=%d out=%d cost=%d, want 100/20/0", in, out, cost)
		}
	})

	t.Run("multi-lane tags, floors, dedupes, sums", func(t *testing.T) {
		res := []laneResult{
			{Dim: sec, TokensIn: 10, TokensOut: 5, CostMicroCents: 3 * microCentsPerCent, Doc: FindingsDoc{Summary: "sec", Findings: []connectors.Finding{
				{File: "a.go", Line: iptr(1), Severity: "error", Title: "SQL injection"}, // kept (>= warning)
				{File: "a.go", Line: iptr(2), Severity: "info", Title: "nit"},            // dropped by warning floor
			}}},
			{Dim: corr, TokensIn: 20, TokensOut: 7, CostMicroCents: 4 * microCentsPerCent, Doc: FindingsDoc{Summary: "corr", Findings: []connectors.Finding{
				{File: "a.go", Line: iptr(1), Severity: "warning", Title: "sql injection"}, // dup of sec's (case-insensitive) → merged
				{File: "b.go", Line: iptr(9), Severity: "info", Title: "logic"},            // kept (info floor)
			}}},
		}
		doc, in, out, cost, err := aggregateReview(res)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if in != 30 || out != 12 || cost != 7 {
			t.Fatalf("summed usage in=%d out=%d cost=%d, want 30/12/7", in, out, cost)
		}
		if len(doc.Findings) != 2 {
			t.Fatalf("want 2 findings after floor+dedupe, got %d: %+v", len(doc.Findings), doc.Findings)
		}
		var merged *connectors.Finding
		for i := range doc.Findings {
			if doc.Findings[i].File == "a.go" && doc.Findings[i].Line != nil && *doc.Findings[i].Line == 1 {
				merged = &doc.Findings[i]
			}
		}
		if merged == nil || merged.Severity != "error" {
			t.Fatalf("merged finding must keep max severity (error): %+v", merged)
		}
		if merged.Dimension != "correctness, security" {
			t.Fatalf("merged dimension tags = %q, want 'correctness, security'", merged.Dimension)
		}
		if !strings.Contains(doc.Summary, "sec") || !strings.Contains(doc.Summary, "corr") {
			t.Fatalf("summary must join lane summaries: %q", doc.Summary)
		}
	})

	t.Run("partial success: one lane fails, survivors proceed, all tokens summed", func(t *testing.T) {
		res := []laneResult{
			{Dim: sec, TokensIn: 50, TokensOut: 10, CostMicroCents: 2 * microCentsPerCent, Err: errors.New("lane boom")}, // failed but billed
			{Dim: corr, TokensIn: 5, TokensOut: 1, Doc: FindingsDoc{Summary: "ok", Findings: []connectors.Finding{
				{File: "b.go", Line: iptr(3), Severity: "error", Title: "bug"},
			}}},
		}
		doc, in, out, cost, err := aggregateReview(res)
		if err != nil {
			t.Fatalf("partial success must not error: %v", err)
		}
		if in != 55 || out != 11 || cost != 2 {
			t.Fatalf("failed lane's tokens must still be summed: in=%d out=%d cost=%d, want 55/11/2", in, out, cost)
		}
		if len(doc.Findings) != 1 || doc.Findings[0].Dimension != "correctness" {
			t.Fatalf("survivor findings wrong: %+v", doc.Findings)
		}
	})

	t.Run("all lanes fail: first error returned, tokens still summed", func(t *testing.T) {
		boom := errors.New("first boom")
		res := []laneResult{
			{Dim: sec, TokensIn: 3, Err: boom},
			{Dim: corr, TokensIn: 4, Err: errors.New("second")},
		}
		_, in, _, _, err := aggregateReview(res)
		if err == nil {
			t.Fatal("all lanes failing must return an error")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("must return the FIRST lane error, got %v", err)
		}
		if in != 7 {
			t.Fatalf("tokens summed even when all fail: in=%d, want 7", in)
		}
	})

	t.Run("sub-cent lanes sum to a real total (per-lane cent rounding must not zero it)", func(t *testing.T) {
		// The threat.gg #36 regression: cheap OpenRouter models across 6 lanes, each costing a
		// fraction of a cent but summing to ~1.72 cents. Rounding to whole cents PER LANE recorded
		// $0.00 for the whole review; summing in micro-cents and rounding once must not.
		res := []laneResult{
			{Dim: sec, CostMicroCents: 202_600},                        // 0.2026¢
			{Dim: corr, CostMicroCents: 225_800},                       // 0.2258¢
			{Dim: Dimension{Key: "performance"}, CostMicroCents: 200_300},
			{Dim: Dimension{Key: "ui"}, CostMicroCents: 260_500},
			{Dim: Dimension{Key: "tests"}, CostMicroCents: 661_900},
			{Dim: Dimension{Key: "docs"}, CostMicroCents: 170_500},
		}
		_, _, _, cost, err := aggregateReview(res) // total = 1_721_600 µ¢ → 2¢
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cost != 2 {
			t.Fatalf("six sub-cent lanes must sum to 2 cents, got %d (per-lane rounding regression)", cost)
		}
	})
}

// TestBuildDimensionRuns pins the persisted per-lane accounting: ran lanes carry
// succeeded/failed + usage + finding count; skipped dimensions carry status "skipped" + reason.
func TestBuildDimensionRuns(t *testing.T) {
	results := []laneResult{
		{Dim: Dimension{Key: "security"}, Model: "m1", Provider: "openrouter", TokensIn: 10, TokensOut: 4, CostCents: 2,
			Doc: FindingsDoc{Findings: []connectors.Finding{{Title: "a"}, {Title: "b"}}}},
		{Dim: Dimension{Key: "correctness"}, Model: "m2", Provider: "anthropic", TokensIn: 5, Err: errors.New("timed out after 8m"), FailReason: "timed out"},
		{Dim: Dimension{Key: "tests"}, Model: "m3", Provider: "openrouter", Err: errors.New("boom")}, // no FailReason set
	}
	skipped := []SkippedDimension{{Key: "ui", Reason: "scope: no matching files"}}

	runs := buildDimensionRuns(results, skipped)
	if len(runs) != 4 {
		t.Fatalf("want 4 runs (3 ran + 1 skipped), got %d", len(runs))
	}
	if runs[0].Status != "succeeded" || runs[0].FindingCount != 2 || runs[0].Model != "m1" || runs[0].TokensIn != 10 {
		t.Fatalf("ran-succeeded record wrong: %+v", runs[0])
	}
	if runs[0].LastError != "" {
		t.Fatalf("succeeded lane must not carry a last_error, got %q", runs[0].LastError)
	}
	if runs[1].Status != "failed" || runs[1].TokensIn != 5 || runs[1].FindingCount != 0 {
		t.Fatalf("failed record wrong: %+v", runs[1])
	}
	if runs[1].LastError != "timed out" { // client-safe reason surfaces, NOT the raw err text
		t.Fatalf("failed lane must persist its FailReason as last_error, got %q", runs[1].LastError)
	}
	if runs[2].Status != "failed" || runs[2].LastError != "sandbox error" { // default category when a site left FailReason empty
		t.Fatalf("failed lane w/o FailReason must default last_error to %q, got %+v", "sandbox error", runs[2])
	}
	if runs[3].Status != "skipped" || runs[3].SkippedReason != "scope: no matching files" {
		t.Fatalf("skipped record wrong: %+v", runs[3])
	}
}

// aggregateReview must NEVER return a nil error with an empty doc — that would post a bogus
// "No issues found" review implying the PR was checked when no lane ran (manyforge-t2s).
func TestAggregateReviewAllSkippedErrors(t *testing.T) {
	doc, _, _, _, err := aggregateReview(nil) // zero lanes ran
	if err == nil {
		t.Fatal("aggregateReview(no lanes): want error, got nil")
	}
	if len(doc.Findings) != 0 || doc.Summary != "" {
		t.Fatalf("want empty doc on error, got %+v", doc)
	}
}

func TestDefaultPanel(t *testing.T) {
	panel := defaultPanel()
	if len(panel) != 1 {
		t.Fatalf("zero-config default must be ONE lane (no cost regression), got %d: %v", len(panel), keysOf(panel))
	}
	d := panel[0]
	if d.Prompt != reviewInstructions {
		t.Fatal("default lane must use reviewInstructions verbatim so behavior is unchanged")
	}
	if len(d.ScopeGlobs) != 0 {
		t.Fatalf("default lane must review ALL files (no scope), got %v", d.ScopeGlobs)
	}
	if !d.Enabled {
		t.Fatal("default lane must be enabled")
	}
	if severityRank(d.MinSeverity) != severityRank("info") {
		t.Fatalf("default lane must have no severity floor (info) so every finding posts, got %q", d.MinSeverity)
	}
	// Catch-all: the default lane must be active for ANY change set.
	active, _ := activeDimensions(panel, []string{"anything/at/all.xyz"})
	if len(active) != 1 {
		t.Fatal("default lane must be active for any changed files")
	}
}

func TestDimensionCatalogSane(t *testing.T) {
	dims := dimensionCatalog()
	if len(dims) < 4 {
		t.Fatalf("expected a real specialist catalog, got %d", len(dims))
	}
	seen := map[string]bool{}
	for _, d := range dims {
		if d.Key == "" || d.Label == "" || d.Prompt == "" {
			t.Fatalf("dimension %+v missing key/label/prompt", d)
		}
		if severityRank(d.MinSeverity) < 0 || d.MinSeverity == "" {
			t.Fatalf("dimension %q bad min severity %q", d.Key, d.MinSeverity)
		}
		seen[d.Key] = true
	}
	for _, must := range []string{"security", "correctness"} {
		if !seen[must] {
			t.Fatalf("dimension catalog must include %q", must)
		}
	}
}
