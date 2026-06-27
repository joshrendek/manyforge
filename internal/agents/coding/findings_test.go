package coding

import (
	"testing"
)

func TestParseFindings(t *testing.T) {
	good := `{"summary":"looks ok","findings":[{"file":"a.go","line":3,"severity":"warning","title":"naming","detail":"rename x"}]}`
	doc, err := ParseFindings([]byte(good))
	if err != nil || doc.Summary != "looks ok" || len(doc.Findings) != 1 {
		t.Fatalf("good parse failed: %+v %v", doc, err)
	}
	bad := []string{
		``,                                  // empty
		`not json`,                          // malformed
		`{"findings":[]}`,                   // missing summary
		`{"summary":"s","findings":[{"file":"a","severity":"bad","title":"t"}]}`, // bad severity
		`{"summary":"s","findings":[{"severity":"info","title":"t"}]}`,           // missing file
	}
	for i, b := range bad {
		if _, err := ParseFindings([]byte(b)); err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
	}
}

// sst/opencode's `run` prints the model's final message, which models routinely
// wrap in a ```json markdown fence (and sometimes a line of prose). The real
// dogfood output from google/gemini-2.5-pro came back fenced — ParseFindings must
// extract the JSON object rather than reject it (manyforge-fqo / manyforge-ht8).
func TestParseFindings_FencedAndProse(t *testing.T) {
	cases := []string{
		"```json\n{\"summary\":\"ok\",\"findings\":[]}\n```",
		"```\n{\"summary\":\"ok\",\"findings\":[]}\n```",
		"Here is the review:\n{\"summary\":\"ok\",\"findings\":[]}\n",
		"  \n{\"summary\":\"ok\",\"findings\":[]}",
	}
	for i, c := range cases {
		doc, err := ParseFindings([]byte(c))
		if err != nil {
			t.Fatalf("case %d: expected parse, got error: %v", i, err)
		}
		if doc.Summary != "ok" {
			t.Fatalf("case %d: summary=%q", i, doc.Summary)
		}
	}
}

// Some models (e.g. z-ai/glm-5.2) emit prose that itself contains braces, and/or
// add extra JSON fields. The old first-{-to-last-} extractor grabbed the prose
// brace and failed with "invalid character 'c' …". ParseFindings must skip prose
// braces and tolerate unknown fields, keeping the real findings object (manyforge-fqo).
func TestParseFindings_SkipsProseBracesAndExtraFields(t *testing.T) {
	in := "Reviewing the {createReview} helper now.\n" +
		"```json\n{\"summary\":\"ok\",\"severity_legend\":\"extra\",\"findings\":[" +
		"{\"file\":\"a.go\",\"line\":3,\"severity\":\"warning\",\"title\":\"t\",\"detail\":\"d\",\"confidence\":0.9}]}\n```"
	doc, err := ParseFindings([]byte(in))
	if err != nil {
		t.Fatalf("expected parse, got %v", err)
	}
	if doc.Summary != "ok" || len(doc.Findings) != 1 || doc.Findings[0].File != "a.go" {
		t.Fatalf("doc=%+v", doc)
	}
}

// Local models vary on severity whitespace/case (qwen2.5-coder:7b emitted
// " warning" with a leading space). ParseFindings normalizes it (manyforge-fqo).
func TestParseFindings_NormalizesSeverity(t *testing.T) {
	in := `{"summary":"s","findings":[{"file":"a.go","line":1,"severity":" Warning ","title":"t","detail":"d"}]}`
	doc, err := ParseFindings([]byte(in))
	if err != nil {
		t.Fatalf("expected parse, got %v", err)
	}
	if doc.Findings[0].Severity != "warning" {
		t.Fatalf("severity = %q, want normalized 'warning'", doc.Findings[0].Severity)
	}
}

// opencode reviews the checkout copied to /tmp/src (entrypoint cwd), so it reports
// absolute paths under that prefix. ParseFindings must strip it so findings are
// repo-relative (for GitHub line links) — manyforge-fqo.
func TestParseFindings_StripsSandboxPrefix(t *testing.T) {
	in := "```json\n{\"summary\":\"s\",\"findings\":[" +
		"{\"file\":\"/tmp/src/cmd/manyforge/main.go\",\"line\":12,\"severity\":\"warning\",\"title\":\"t\",\"detail\":\"d\"}," +
		"{\"file\":\"internal/already/relative.go\",\"line\":1,\"severity\":\"info\",\"title\":\"t2\",\"detail\":\"d2\"}]}\n```"
	doc, err := ParseFindings([]byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Findings[0].File != "cmd/manyforge/main.go" {
		t.Fatalf("prefix not stripped: %q", doc.Findings[0].File)
	}
	if doc.Findings[1].File != "internal/already/relative.go" {
		t.Fatalf("relative path mangled: %q", doc.Findings[1].File)
	}
}

