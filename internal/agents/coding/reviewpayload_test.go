package coding

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

// Constrained-ness tracks model capability, not network locality: huggingface is a public
// ZeroGPU Space (remote) yet belongs with the on-host small-model providers, because the GPU
// is released between opencode turns and every turn re-prefills the diff. See manyforge-bhx.
func TestIsConstrainedProvider(t *testing.T) {
	for _, p := range []string{"ollama", "vllm", "huggingface"} {
		if !isConstrainedProvider(p) {
			t.Fatalf("%q should be constrained (tight diff budget)", p)
		}
	}
	for _, p := range []string{"openrouter", "anthropic", "openai"} {
		if isConstrainedProvider(p) {
			t.Fatalf("%q should NOT be constrained", p)
		}
	}
}

// The whole point of the constrained budget is that it is strictly smaller than the default;
// a refactor that inverts or equalizes them silently un-fixes manyforge-6h1/ornith:9b.
func TestConstrainedBudgetIsTighterThanDefault(t *testing.T) {
	if constrainedProviderMaxTotalBytes >= reviewMaxTotalBytes {
		t.Fatalf("constrained budget %d must be < default %d", constrainedProviderMaxTotalBytes, reviewMaxTotalBytes)
	}
}

func TestAssembleDiffPayload(t *testing.T) {
	files := []connectors.ChangedFile{
		{Path: "config.yaml", Patch: "@@ -1,0 +1,1 @@\n+key: v\n"}, // non-code but reviewable → sorts after code
		{Path: "a.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+added\n"},   // code → first
		{Path: "bin.png", Patch: ""},                               // no patch → skipped
	}
	payload, skipped, omitted, filtered := assembleDiffPayload(files, reviewMaxTotalBytes)
	if len(skipped) != 1 || skipped[0] != "bin.png" {
		t.Fatalf("skipped=%v, want [bin.png]", skipped)
	}
	if len(omitted) != 0 {
		t.Fatalf("omitted=%v, want none", omitted)
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered=%v, want none (no docs in this set)", filtered)
	}
	ia, ic := strings.Index(payload, "=== a.go ==="), strings.Index(payload, "=== config.yaml ===")
	if ia < 0 || ic < 0 {
		t.Fatalf("payload missing a file header:\n%s", payload)
	}
	if ia > ic {
		t.Fatalf("code file must come before non-code; a.go@%d config.yaml@%d", ia, ic)
	}
	if !strings.Contains(payload, "added") {
		t.Fatalf("payload missing hunk content:\n%s", payload)
	}
}

func TestAssembleDiffPayload_OmitsOverBudget(t *testing.T) {
	big := "@@ -1,0 +1,1 @@\n+" + strings.Repeat("x", reviewMaxTotalBytes) + "\n"
	_, _, omitted, _ := assembleDiffPayload([]connectors.ChangedFile{{Path: "big.go", Patch: big}}, reviewMaxTotalBytes)
	if len(omitted) != 1 || omitted[0] != "big.go" {
		t.Fatalf("omitted=%v, want [big.go]", omitted)
	}
}

func TestIsNonReviewableDoc(t *testing.T) {
	for _, p := range []string{
		"docs/superpowers/plans/2026-06-30-frontend-performance.md",
		"README.md", "notes.markdown", "guide.mdx", "spec.rst", "x.adoc",
		".beads/issues.jsonl", "frontend/docs/guide.md",
	} {
		if !isNonReviewableDoc(p) {
			t.Errorf("%q should be non-reviewable (prose/plan/tracker)", p)
		}
	}
	for _, p := range []string{
		"internal/agents/coding/service.go", "frontend/src/app/app.component.ts",
		"config.yaml", "Dockerfile", "deploy/values.json", "scripts/run.sh",
		"docsite.go", // "docs" is a filename substring, not a path segment → reviewable
	} {
		if isNonReviewableDoc(p) {
			t.Errorf("%q should be reviewable code/config", p)
		}
	}
}

func TestAssembleDiffPayload_FiltersDocs(t *testing.T) {
	files := []connectors.ChangedFile{
		{Path: "internal/svc.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+code\n"},
		{Path: "docs/plans/perf.md", Patch: "@@ -1,0 +1,1 @@\n+# big plan doc\n"},
		{Path: ".beads/issues.jsonl", Patch: "@@ -1,0 +1,1 @@\n+{}\n"},
		{Path: "config.yaml", Patch: "@@ -1,0 +1,1 @@\n+k: v\n"},
	}
	payload, _, _, filtered := assembleDiffPayload(files, reviewMaxTotalBytes)
	if len(filtered) != 2 {
		t.Fatalf("filtered=%v, want the .md plan + .beads tracker", filtered)
	}
	if strings.Contains(payload, "perf.md") || strings.Contains(payload, "issues.jsonl") {
		t.Fatalf("prose/plan/tracker docs must NOT reach the review payload:\n%s", payload)
	}
	if !strings.Contains(payload, "=== internal/svc.go ===") || !strings.Contains(payload, "=== config.yaml ===") {
		t.Fatalf("reviewable code/config must be in the payload:\n%s", payload)
	}
}

func TestCommentableMap(t *testing.T) {
	files := []connectors.ChangedFile{{Path: "a.go", Commentable: map[int]bool{3: true}}}
	m := commentableMap(files)
	if !m["a.go"][3] {
		t.Fatalf("commentableMap dropped a.go:3: %v", m)
	}
}
