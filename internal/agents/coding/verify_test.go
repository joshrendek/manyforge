package coding

import (
	"errors"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func vLine(i int) *int { return &i }

func vFinding(file string, line *int, sev, title string) connectors.Finding {
	return connectors.Finding{File: file, Line: line, Severity: sev, Title: title, Detail: "because reasons"}
}

func TestBuildVerifyPrompt_IncludesInstructionsAndCandidates(t *testing.T) {
	fs := []connectors.Finding{
		vFinding("a.go", vLine(10), "error", "nil deref"),
		vFinding("b.go", nil, "warning", "unchecked error"),
	}
	p := buildVerifyPrompt(fs)
	for _, want := range []string{
		"VERIFICATION pass",           // instructions present
		"CANDIDATE FINDINGS TO VERIFY (2)",
		"file=a.go line=10 severity=error",
		`title="nil deref"`,
		"file=b.go line=null severity=warning", // nil line renders as null
		`title="unchecked error"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("verify prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestApplyVerifyDrops_KeepsConfirmedDropsRest(t *testing.T) {
	orig := []connectors.Finding{
		vFinding("a.go", vLine(10), "error", "nil deref"),
		vFinding("b.go", vLine(3), "warning", "unchecked error"),
		vFinding("c.go", vLine(7), "info", "style nit"),
	}
	// Verifier confirms #1 and #2 (echoed, possibly with different Detail/Dimension — match ignores those).
	confirmed := []connectors.Finding{
		{File: "a.go", Line: vLine(10), Severity: "error", Title: "nil deref"},
		{File: "b.go", Line: vLine(3), Severity: "warning", Title: "Unchecked Error"}, // title case-insensitive
	}
	kept, dropped := applyVerifyDrops(orig, confirmed)
	if len(kept) != 2 || kept[0].File != "a.go" || kept[1].File != "b.go" {
		t.Fatalf("kept = %+v, want a.go + b.go", kept)
	}
	if len(dropped) != 1 || dropped[0].File != "c.go" {
		t.Fatalf("dropped = %+v, want c.go", dropped)
	}
	// Kept findings are the ORIGINAL objects (Detail preserved), not the verifier's echo.
	if kept[0].Detail != "because reasons" {
		t.Errorf("kept finding should retain original Detail, got %q", kept[0].Detail)
	}
}

func TestApplyVerifyDrops_EmptyConfirmedDropsAll(t *testing.T) {
	orig := []connectors.Finding{vFinding("a.go", vLine(1), "error", "x")}
	kept, dropped := applyVerifyDrops(orig, nil)
	if len(kept) != 0 || len(dropped) != 1 {
		t.Fatalf("kept=%d dropped=%d, want 0/1 (verifier rejected all)", len(kept), len(dropped))
	}
}

func TestVerifyOutcome_FailOpenOnLaneError(t *testing.T) {
	orig := []connectors.Finding{
		vFinding("a.go", vLine(10), "error", "nil deref"),
		vFinding("b.go", vLine(3), "warning", "unchecked error"),
	}
	// A failed verify lane (sandbox error, timeout, unparseable) must keep EVERY original finding.
	vres := laneResult{Err: errors.New("sandbox: timed out"), Doc: FindingsDoc{}}
	kept, dropped, failedOpen := verifyOutcome(orig, vres)
	if !failedOpen {
		t.Fatal("verifyOutcome must report failedOpen when the verify lane errored")
	}
	if len(kept) != len(orig) || len(dropped) != 0 {
		t.Fatalf("fail-open must keep all findings: kept=%d dropped=%d (want %d/0)", len(kept), len(dropped), len(orig))
	}
}

func TestVerifyOutcome_SuccessAppliesDrops(t *testing.T) {
	orig := []connectors.Finding{
		vFinding("a.go", vLine(10), "error", "nil deref"),
		vFinding("c.go", vLine(7), "info", "style nit"),
	}
	vres := laneResult{Doc: FindingsDoc{Findings: []connectors.Finding{
		{File: "a.go", Line: vLine(10), Severity: "error", Title: "nil deref"},
	}}}
	kept, dropped, failedOpen := verifyOutcome(orig, vres)
	if failedOpen {
		t.Fatal("a successful verify lane is not fail-open")
	}
	if len(kept) != 1 || kept[0].File != "a.go" {
		t.Fatalf("kept = %+v, want just a.go", kept)
	}
	if len(dropped) != 1 || dropped[0].File != "c.go" {
		t.Fatalf("dropped = %+v, want c.go", dropped)
	}
}

func TestVerifyOutcome_SuccessEmptyIsNotFailOpen(t *testing.T) {
	orig := []connectors.Finding{vFinding("a.go", vLine(1), "info", "x")}
	// Verifier ran fine and confirmed nothing → legitimately drop all (distinct from fail-open).
	kept, dropped, failedOpen := verifyOutcome(orig, laneResult{Doc: FindingsDoc{}})
	if failedOpen {
		t.Fatal("a clean empty verify result is a real 'all false positives', not fail-open")
	}
	if len(kept) != 0 || len(dropped) != 1 {
		t.Fatalf("kept=%d dropped=%d, want 0/1", len(kept), len(dropped))
	}
}
