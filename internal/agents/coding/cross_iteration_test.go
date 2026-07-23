package coding

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestCrossIterationFingerprint_LineIndependent(t *testing.T) {
	a := vFinding("a.go", vLine(10), "error", "nil deref")
	b := vFinding("a.go", vLine(99), "error", "nil deref") // same file+title, shifted line
	if crossIterationFingerprint(a) != crossIterationFingerprint(b) {
		t.Error("fingerprint must be line-independent (a finding survives a line shift between commits)")
	}
	c := vFinding("b.go", vLine(10), "error", "nil deref") // different file
	if crossIterationFingerprint(a) == crossIterationFingerprint(c) {
		t.Error("different file must yield a different fingerprint")
	}
}

func TestCrossIterationFingerprint_PrefersRuleIDThenTitle(t *testing.T) {
	// Same rule_id + file ⇒ same fingerprint even if the title is reworded.
	a := connectors.Finding{File: "a.go", Title: "one wording", RuleID: "no-raw-sql"}
	b := connectors.Finding{File: "a.go", Title: "different wording", RuleID: "no-raw-sql"}
	if crossIterationFingerprint(a) != crossIterationFingerprint(b) {
		t.Error("same rule_id + file ⇒ same fingerprint regardless of title")
	}
	// No rule_id ⇒ title-based, case-insensitive.
	c := connectors.Finding{File: "a.go", Title: "Unchecked Error"}
	d := connectors.Finding{File: "a.go", Title: "unchecked error"}
	if crossIterationFingerprint(c) != crossIterationFingerprint(d) {
		t.Error("title fallback must be case-insensitive")
	}
	if crossIterationFingerprint(a) == crossIterationFingerprint(c) {
		t.Error("rule-cited and title-only findings should differ")
	}
}

func TestClassifyCrossIteration_NewCarryoverResolved(t *testing.T) {
	fA := crossIterationFingerprint(vFinding("a.go", vLine(1), "error", "A"))
	fB := crossIterationFingerprint(vFinding("b.go", vLine(1), "error", "B"))
	prior := map[string]string{fA: "open", fB: "open"}
	current := []connectors.Finding{
		vFinding("a.go", vLine(5), "error", "A"), // A, moved line → CARRYOVER
		vFinding("c.go", vLine(1), "error", "C"), // C → NEW
	}
	delta, fps := classifyCrossIteration(prior, current)
	if delta.New != 1 || delta.Carryover != 1 || delta.Resolved != 1 {
		t.Fatalf("delta = %+v, want New=1 Carryover=1 Resolved=1 (B gone)", delta)
	}
	if len(fps) != 2 {
		t.Fatalf("distinct current fingerprints = %d, want 2", len(fps))
	}
}

func TestClassifyCrossIteration_AlreadyResolvedNotRecounted(t *testing.T) {
	fA := crossIterationFingerprint(vFinding("a.go", vLine(1), "error", "A"))
	// A was already resolved in a prior iteration and is still absent ⇒ NOT counted as resolved again.
	prior := map[string]string{fA: "resolved"}
	delta, _ := classifyCrossIteration(prior, nil)
	if delta.Resolved != 0 {
		t.Fatalf("an already-resolved fingerprint must not be recounted; Resolved=%d want 0", delta.Resolved)
	}
}

func TestClassifyCrossIteration_DistinctFingerprintsCountedOnce(t *testing.T) {
	current := []connectors.Finding{
		vFinding("a.go", vLine(1), "error", "dup"),
		vFinding("a.go", vLine(9), "error", "dup"), // same line-free fingerprint
	}
	delta, fps := classifyCrossIteration(nil, current)
	if len(fps) != 1 || delta.New != 1 {
		t.Fatalf("a shared line-free fingerprint must count once; delta=%+v fps=%d", delta, len(fps))
	}
}

func TestFindingDelta_SummaryLine(t *testing.T) {
	s := findingDelta{New: 2, Carryover: 3, Resolved: 1}.summaryLine()
	for _, want := range []string{"2 new", "3 carried over", "1 resolved"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary line %q missing %q", s, want)
		}
	}
}
