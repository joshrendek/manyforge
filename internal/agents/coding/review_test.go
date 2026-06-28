package coding

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func iptr(n int) *int { return &n }

func TestBuildReview_SplitsInlineVsBody(t *testing.T) {
	doc := FindingsDoc{
		Summary: "overall ok",
		Findings: []connectors.Finding{
			{File: "a.go", Line: iptr(11), Severity: "warning", Title: "on diff", Detail: "fix it"},     // inline
			{File: "a.go", Line: iptr(999), Severity: "info", Title: "off diff line", Detail: "x"},       // body (line not in diff)
			{File: "b.go", Line: iptr(3), Severity: "error", Title: "file not in diff", Detail: "y"},      // body (file not changed)
			{File: "c.go", Line: nil, Severity: "info", Title: "no line", Detail: "z"},                    // body (no line)
		},
	}
	changed := map[string]map[int]bool{"a.go": {11: true}}

	rev := buildReview(doc, changed, "sha123", nil, nil)

	if rev.CommitID != "sha123" {
		t.Fatalf("commit id = %q", rev.CommitID)
	}
	if len(rev.Comments) != 1 {
		t.Fatalf("want 1 inline comment, got %d: %+v", len(rev.Comments), rev.Comments)
	}
	c := rev.Comments[0]
	if c.Path != "a.go" || c.Line != 11 || !strings.Contains(c.Body, "on diff") {
		t.Fatalf("inline comment wrong: %+v", c)
	}
	// The three non-diff findings must surface in the body so nothing is lost.
	for _, want := range []string{"overall ok", "off diff line", "file not in diff", "no line"} {
		if !strings.Contains(rev.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, rev.Body)
		}
	}
	// The inline finding must NOT be duplicated in the body's leftover list.
	if strings.Contains(rev.Body, "on diff") {
		t.Fatalf("inline finding should not also appear in body:\n%s", rev.Body)
	}
}

func TestBuildReview_NoDiffInfoPostsEverythingInBody(t *testing.T) {
	doc := FindingsDoc{
		Summary:  "s",
		Findings: []connectors.Finding{{File: "a.go", Line: iptr(1), Severity: "info", Title: "t", Detail: "d"}},
	}
	rev := buildReview(doc, map[string]map[int]bool{}, "sha", nil, nil)
	if len(rev.Comments) != 0 {
		t.Fatalf("no diff info → expected 0 inline comments, got %d", len(rev.Comments))
	}
	if !strings.Contains(rev.Body, "Automated code review") || !strings.Contains(rev.Body, "t") {
		t.Fatalf("body should contain header + the finding:\n%s", rev.Body)
	}
}

func TestBuildReview_NotesSkippedAndOmitted(t *testing.T) {
	doc := FindingsDoc{Summary: "s"}
	rev := buildReview(doc, map[string]map[int]bool{}, "sha",
		[]string{"bin.png"}, []string{"big.go"})
	if !strings.Contains(rev.Body, "bin.png") || !strings.Contains(rev.Body, "not reviewed") {
		t.Fatalf("body must note the skipped file:\n%s", rev.Body)
	}
	if !strings.Contains(rev.Body, "big.go") || !strings.Contains(rev.Body, "omitted") {
		t.Fatalf("body must note the omitted file:\n%s", rev.Body)
	}
}
