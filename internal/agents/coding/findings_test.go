package coding

import (
	"strings"
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

func TestRenderMarkdown(t *testing.T) {
	md := RenderMarkdown(FindingsDoc{Summary: "S"})
	if md == "" || !strings.Contains(md, "Automated code review") {
		t.Fatalf("render missing header: %s", md)
	}
}
