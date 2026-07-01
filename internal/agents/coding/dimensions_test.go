package coding

import (
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
		{Key: "security", Order: 1, Enabled: true},                                    // no scope → all
		{Key: "ui", Order: 2, Enabled: true, ScopeGlobs: []string{"frontend/**"}},     // scoped out
		{Key: "docs", Order: 3, Enabled: false},                                       // disabled
		{Key: "perf", Order: 4, Enabled: true, ScopeGlobs: []string{"**/*.go"}},       // in scope
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

func TestDefaultDimensionsSane(t *testing.T) {
	dims := defaultDimensions()
	if len(dims) < 4 {
		t.Fatalf("expected a real default panel, got %d", len(dims))
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
			t.Fatalf("default panel must include %q", must)
		}
	}
}
