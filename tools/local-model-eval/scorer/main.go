// Command scorer reads opencode review output on stdin, validates it with the
// REAL coding.ParseFindings contract (the same the sandbox uses), and scores it
// against the eval fixture's four planted issues. Output:
//
//	PARSE_OK findings=N        (or PARSE_FAIL: <err>)
//	  [SEV] file:line — title  (one per finding)
//	  HIT/MISS <planted issue> (one per planted issue)
//	CAUGHT X/4
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/manyforge/manyforge/internal/agents/coding"
)

type plantedIssue struct {
	name     string
	keywords []string // any (case-insensitive) substring in a finding counts as covered
}

var planted = []plantedIssue{
	{"nil-deref (Greet)", []string{"nil", "panic", "dereference", "deref"}},
	{"ignored error (statusHandler)", []string{"ignored", "unchecked", "discard", "return value", "not checked", "error from"}},
	{"hardcoded secret (stripeKey)", []string{"hardcoded", "hard-coded", "secret", "credential", "api key", "apikey", "stripekey", "sk_live"}},
	{"unbounded input (uploadHandler)", []string{"unbounded", "content-length", "denial of service", " dos", "size limit", "without limit", "memory exhaust", "allocat"}},
}

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Println("READ_FAIL:", err)
		os.Exit(1)
	}
	doc, err := coding.ParseFindings(raw)
	if err != nil {
		fmt.Println("PARSE_FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("PARSE_OK findings=%d\n", len(doc.Findings))

	var hay strings.Builder
	for _, f := range doc.Findings {
		loc := f.File
		if f.Line != nil {
			loc = fmt.Sprintf("%s:%d", f.File, *f.Line)
		}
		fmt.Printf("  [%s] %s — %s\n", strings.ToUpper(f.Severity), loc, f.Title)
		hay.WriteString(strings.ToLower(f.Title + " " + f.Detail + " " + f.File + " "))
	}
	text := hay.String()

	caught := 0
	for _, p := range planted {
		hit := false
		for _, kw := range p.keywords {
			if strings.Contains(text, kw) {
				hit = true
				break
			}
		}
		if hit {
			caught++
			fmt.Printf("  HIT  %s\n", p.name)
		} else {
			fmt.Printf("  MISS %s\n", p.name)
		}
	}
	fmt.Printf("CAUGHT %d/4\n", caught)
}
