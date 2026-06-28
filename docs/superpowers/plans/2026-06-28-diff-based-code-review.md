# Diff-based Code Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Send the code-review model the changed *hunks* (annotated with current-file line numbers) instead of whole files, on both the local (host-side) and cloud (opencode sandbox) paths.

**Architecture:** GitHub already returns each changed file's unified-diff `patch`; today `ChangedLines` parses it for commentable line numbers and discards the text. We retain the patch, render it as annotated hunks via one shared renderer in the `github` package, and feed those hunks to both review paths. Over-budget and patchless files are dropped and surfaced (never silently). Multi-call chunking of huge diffs is out of scope (deferred to `manyforge-206`).

**Tech Stack:** Go 1.x, standard library only (no new deps); bash entrypoint for the sandbox; Docker for the sandbox image.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-06-28-diff-based-code-review-design.md`. Issue `manyforge-fqo` #1 (epic `manyforge-7ml`).
- **Budget:** total payload cap `localReviewMaxTotalBytes` rises **48KB → 64KB**; per-file cap `localReviewMaxFileBytes` stays **32KB** (now applied to *rendered* hunk text).
- **No silent caps:** skipped (no patch) and omitted (over budget) files must appear in the review summary body AND a `log`/audit trail.
- **Render format (approved, exact):** `=== <path> ===` header per file; `@@ <newStart>-<newEnd> @@` per hunk; each line = right-aligned new-side line number, a `+` (added) or space (context) marker, then the line text. Removed lines are omitted.
- **Prompt sync:** the review instructions exist in THREE copies that must stay in sync — `internal/agents/coding/localreview.go` (`reviewInstructions`/`reviewSchemaLine`), `deploy/sandbox/entrypoint.sh` (`INSTRUCTIONS`/`PROMPT`), and `tools/local-model-eval/run.sh`.
- **sqlc is NOT involved** (no schema/query changes).
- **Verification gates (run before final push):** `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`; `make lint` (staticcheck); `go test -tags contract ./cmd/...`; `make sec-test`; integration `go test -tags integration -p 1 ./internal/agents/coding/`.

---

### Task 1: Structured hunk parser + renderer

Lift the hunk-walking logic out of `commentableLines` into a reusable parser, and add the annotated-hunk renderer. Pure functions in the `github` package — no I/O.

**Files:**
- Modify: `internal/connectors/github/diff.go`
- Test: `internal/connectors/github/diff_test.go`

**Interfaces:**
- Consumes: nothing (uses existing `parseHunkNewStart`).
- Produces:
  - `type DiffLine struct { NewLineNo int; Added bool; Text string }`
  - `type Hunk struct { NewStart int; Lines []DiffLine }`
  - `func ParseHunks(patch string) []Hunk`
  - `func RenderAnnotatedHunks(patch string) string`
  - `commentableLines` reimplemented on top of `ParseHunks` (same signature/behavior).

- [ ] **Step 1: Write the failing tests**

Add to `internal/connectors/github/diff_test.go` (no import change needed — `testing` is already imported, and the assertions below are exact-string so `strings` is not required). Append:

```go
func TestParseHunks(t *testing.T) {
	patch := "@@ -1,1 +1,2 @@\n ctx\n+added\n"
	hs := ParseHunks(patch)
	if len(hs) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hs))
	}
	h := hs[0]
	if h.NewStart != 1 {
		t.Fatalf("NewStart=%d, want 1", h.NewStart)
	}
	if len(h.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %+v", len(h.Lines), h.Lines)
	}
	if h.Lines[0] != (DiffLine{NewLineNo: 1, Added: false, Text: "ctx"}) {
		t.Fatalf("line0=%+v", h.Lines[0])
	}
	if h.Lines[1] != (DiffLine{NewLineNo: 2, Added: true, Text: "added"}) {
		t.Fatalf("line1=%+v", h.Lines[1])
	}
}

func TestParseHunks_EmptyOrBinary(t *testing.T) {
	if ParseHunks("") != nil {
		t.Fatal("empty patch must yield nil hunks")
	}
}

func TestRenderAnnotatedHunks(t *testing.T) {
	// Format is deterministic ("%5d %s %s\n" per line): right-aligned new-side line
	// number, a space, the +/space marker, a space, then the text. Assert it exactly.
	patch := "@@ -1,1 +1,2 @@\n ctx\n+added\n"
	want := "@@ 1-2 @@\n    1   ctx\n    2 + added\n"
	if got := RenderAnnotatedHunks(patch); got != want {
		t.Fatalf("render mismatch:\nwant: %q\ngot:  %q", want, got)
	}
	if RenderAnnotatedHunks("") != "" {
		t.Fatal("empty patch must render empty string")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/connectors/github/ -run 'TestParseHunks|TestRenderAnnotatedHunks' -v`
Expected: FAIL — `undefined: ParseHunks`, `undefined: RenderAnnotatedHunks`, `undefined: DiffLine`.

- [ ] **Step 3: Implement the parser + renderer and refactor `commentableLines`**

In `internal/connectors/github/diff.go`, add `"fmt"` to the imports:

```go
import (
	"fmt"
	"strconv"
	"strings"
)
```

Replace the existing `commentableLines` function (lines 14-45) with the structured types, `ParseHunks`, `RenderAnnotatedHunks`, and the slimmed `commentableLines`:

```go
// DiffLine is one new-side line of a diff hunk (added or context). Removed lines
// have no new-side number and are not represented.
type DiffLine struct {
	NewLineNo int    // new-side (RIGHT) line number
	Added     bool   // true if a '+' line, false if a context line
	Text      string // line content without the leading +/space marker
}

// Hunk is a contiguous run of new-side lines from one "@@" block.
type Hunk struct {
	NewStart int
	Lines    []DiffLine
}

// ParseHunks parses a GitHub unified-diff patch (the files[].patch field) into its
// hunks, tracking the new-side (RIGHT) line number of every added and context line.
// Removed lines advance only the old side and are dropped. Returns nil for an
// empty/absent patch (binary or too-large files have no patch).
func ParseHunks(patch string) []Hunk {
	var hunks []Hunk
	var cur *Hunk
	newLine := 0
	inHunk := false
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(ln, "@@") {
			newLine = parseHunkNewStart(ln)
			inHunk = newLine > 0
			if inHunk {
				hunks = append(hunks, Hunk{NewStart: newLine})
				cur = &hunks[len(hunks)-1]
			} else {
				cur = nil
			}
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "+"):
			cur.Lines = append(cur.Lines, DiffLine{NewLineNo: newLine, Added: true, Text: ln[1:]})
			newLine++
		case strings.HasPrefix(ln, "-"):
			// removed: old side only, do not advance the new side
		case strings.HasPrefix(ln, "\\"):
			// "\ No newline at end of file" marker — ignore
		case ln == "":
			// trailing artifact from splitting on the final newline — not a real
			// hunk body line (genuine blank context lines start with a space)
		default:
			// context line (leading space) → present on the new side
			cur.Lines = append(cur.Lines, DiffLine{NewLineNo: newLine, Added: false, Text: ln[1:]})
			newLine++
		}
	}
	return hunks
}

// RenderAnnotatedHunks renders a patch as gutter-numbered hunks: each changed line
// shows its current-file (new-side) line number and a +/space marker, so a model
// can cite exact, in-diff line numbers. Returns "" when the patch has no hunks.
func RenderAnnotatedHunks(patch string) string {
	hunks := ParseHunks(patch)
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hunks {
		end := h.NewStart
		if n := len(h.Lines); n > 0 {
			end = h.Lines[n-1].NewLineNo
		}
		fmt.Fprintf(&b, "@@ %d-%d @@\n", h.NewStart, end)
		for _, l := range h.Lines {
			marker := " "
			if l.Added {
				marker = "+"
			}
			fmt.Fprintf(&b, "%5d %s %s\n", l.NewLineNo, marker, l.Text)
		}
	}
	return b.String()
}

// commentableLines returns the set of new-side line numbers that fall inside a diff
// hunk — the only lines GitHub accepts as inline-comment targets. Built on
// ParseHunks so there is a single hunk parser. Empty for an empty/absent patch.
func commentableLines(patch string) map[int]bool {
	out := map[int]bool{}
	for _, h := range ParseHunks(patch) {
		for _, l := range h.Lines {
			out[l.NewLineNo] = true
		}
	}
	return out
}
```

(Leave `parseHunkNewStart` unchanged below it.)

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `go test ./internal/connectors/github/ -v`
Expected: PASS — new tests pass AND existing `TestCommentableLines`, `TestCommentableLines_MultiHunk`, `TestCommentableLines_EmptyOrBinary` still pass (regression-proofs the refactor).

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/github/diff.go internal/connectors/github/diff_test.go
git commit -m "feat(007): structured hunk parser + annotated-hunk renderer (manyforge-fqo)"
```

---

### Task 2: `ChangedFiles` connector method (patch + commentable lines)

Add a connector method that returns each changed file's patch text alongside its commentable lines, in one API fetch. Keep `ChangedLines` for now (removed in Task 5) so the tree stays green.

**Files:**
- Modify: `internal/connectors/repo_connector.go` (interface + new type)
- Modify: `internal/connectors/github/client.go` (implement `ChangedFiles`)
- Test: `internal/connectors/github/client_test.go`

**Interfaces:**
- Consumes: `commentableLines` (Task 1, github package).
- Produces:
  - `type ChangedFile struct { Path string; Patch string; Commentable map[int]bool }`
  - `ChangedFiles(ctx context.Context, prNumber int) ([]ChangedFile, error)` on `RepoConnector` and the github `client`.

- [ ] **Step 1: Write the failing test**

In `internal/connectors/github/client_test.go`, append:

```go
func TestChangedFiles(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pulls/42/files") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"filename":"a.go","patch":"@@ -1,1 +1,2 @@\n ctx\n+added\n"},
			{"filename":"bin.png","patch":""}
		]`))
	})
	got, err := c.ChangedFiles(t.Context(), 42)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(got), got)
	}
	var a connectors.ChangedFile
	for _, f := range got {
		if f.Path == "a.go" {
			a = f
		}
	}
	if a.Patch == "" {
		t.Fatalf("a.go patch must be retained, got empty")
	}
	if !a.Commentable[1] || !a.Commentable[2] {
		t.Fatalf("a.go lines 1,2 expected commentable; got %v", a.Commentable)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/connectors/github/ -run TestChangedFiles -v`
Expected: FAIL — `c.ChangedFiles undefined` and `connectors.ChangedFile undefined`.

- [ ] **Step 3: Add the type + interface method**

In `internal/connectors/repo_connector.go`, add the type (place it just above `PullRequest`):

```go
// ChangedFile is one file of a PR diff: its new-version path, the raw unified-diff
// patch text (the files[].patch field; "" for binary or GitHub-omitted patches),
// and the set of new-side line numbers that are valid inline-comment targets.
type ChangedFile struct {
	Path        string
	Patch       string
	Commentable map[int]bool
}
```

In the `RepoConnector` interface, add `ChangedFiles` directly below the existing `ChangedLines` method (keep `ChangedLines` for now):

```go
	// ChangedFiles returns the PR's changed files with patch text and commentable
	// new-side lines, in one fetch — serving both the diff-based review payload and
	// inline-comment placement. Files with no patch (binary/too-large) appear with an
	// empty Patch and empty Commentable set.
	ChangedFiles(ctx context.Context, prNumber int) ([]ChangedFile, error)
```

- [ ] **Step 4: Implement `ChangedFiles` as the fetch; make `ChangedLines` delegate to it**

In `internal/connectors/github/client.go`, replace the existing `ChangedLines` method (lines 97-140) with `ChangedFiles` (the real fetch) plus a thin `ChangedLines` that delegates — so there is exactly one pagination loop (no duplication):

```go
// ChangedFiles fetches the PR's changed files and returns, per file, the raw patch
// text and the commentable new-side lines. One fetch serves both the diff-based
// review payload and inline-comment placement. 404 → errs.ErrNotFound.
func (c *client) ChangedFiles(ctx context.Context, prNumber int) ([]connectors.ChangedFile, error) {
	var out []connectors.ChangedFile
	for page := 1; page <= changedFilesPageCap; page++ {
		url := fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100&page=%d", c.apiBase, c.repo, prNumber, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("github: build pr files request: %w", err)
		}
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("github: fetch pr files: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github: fetch pr files status %d", resp.StatusCode)
		}
		var files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&files)
		_ = resp.Body.Close()
		if derr != nil {
			return nil, fmt.Errorf("github: decode pr files: %w", derr)
		}
		for _, f := range files {
			out = append(out, connectors.ChangedFile{
				Path:        f.Filename,
				Patch:       f.Patch,
				Commentable: commentableLines(f.Patch),
			})
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

// ChangedLines is retained until the service is switched to ChangedFiles (Task 5).
// It delegates so there is a single fetch path.
func (c *client) ChangedLines(ctx context.Context, prNumber int) (map[string]map[int]bool, error) {
	files, err := c.ChangedFiles(ctx, prNumber)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		out[f.Path] = f.Commentable
	}
	return out, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/connectors/... -v`
Expected: PASS — `TestChangedFiles` passes; existing `TestChangedLines` still passes.

- [ ] **Step 6: Commit**

```bash
git add internal/connectors/repo_connector.go internal/connectors/github/client.go internal/connectors/github/client_test.go
git commit -m "feat(007): ChangedFiles connector method — retain patch text (manyforge-fqo)"
```

---

### Task 3: Diff payload assembler + commentable map helper

Add the pure function that renders `[]connectors.ChangedFile` into the review payload, source-first, within budget, reporting skipped/omitted. Pure additions — `readChangedFiles` and `localReview` are untouched this task (switched in Task 5).

**Files:**
- Modify: `internal/agents/coding/localreview.go` (constants, imports, `assembleDiffPayload`, `commentableMap`)
- Test: `internal/agents/coding/localreview_test.go`

**Interfaces:**
- Consumes: `github.RenderAnnotatedHunks` (Task 1), `connectors.ChangedFile` (Task 2), existing `codeExt`, `localReviewMaxFileBytes`, `localReviewMaxTotalBytes`.
- Produces:
  - `func assembleDiffPayload(files []connectors.ChangedFile) (payload string, skipped, omitted []string)`
  - `func commentableMap(files []connectors.ChangedFile) map[string]map[int]bool`

- [ ] **Step 1: Write the failing tests**

In `internal/agents/coding/localreview_test.go`, add `"strings"` and the connectors import to the import block:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)
```

Append:

```go
func TestAssembleDiffPayload(t *testing.T) {
	files := []connectors.ChangedFile{
		{Path: "doc.md", Patch: "@@ -1,0 +1,1 @@\n+hello\n"},     // non-code → sorts last
		{Path: "a.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+added\n"}, // code → first
		{Path: "bin.png", Patch: ""},                             // no patch → skipped
	}
	payload, skipped, omitted := assembleDiffPayload(files)
	if len(skipped) != 1 || skipped[0] != "bin.png" {
		t.Fatalf("skipped=%v, want [bin.png]", skipped)
	}
	if len(omitted) != 0 {
		t.Fatalf("omitted=%v, want none", omitted)
	}
	ia, id := strings.Index(payload, "=== a.go ==="), strings.Index(payload, "=== doc.md ===")
	if ia < 0 || id < 0 {
		t.Fatalf("payload missing a file header:\n%s", payload)
	}
	if ia > id {
		t.Fatalf("code file must come before non-code; a.go@%d doc.md@%d", ia, id)
	}
	if !strings.Contains(payload, "added") {
		t.Fatalf("payload missing hunk content:\n%s", payload)
	}
}

func TestAssembleDiffPayload_OmitsOverBudget(t *testing.T) {
	big := "@@ -1,0 +1,1 @@\n+" + strings.Repeat("x", localReviewMaxTotalBytes) + "\n"
	_, _, omitted := assembleDiffPayload([]connectors.ChangedFile{{Path: "big.go", Patch: big}})
	if len(omitted) != 1 || omitted[0] != "big.go" {
		t.Fatalf("omitted=%v, want [big.go]", omitted)
	}
}

func TestCommentableMap(t *testing.T) {
	files := []connectors.ChangedFile{{Path: "a.go", Commentable: map[int]bool{3: true}}}
	m := commentableMap(files)
	if !m["a.go"][3] {
		t.Fatalf("commentableMap dropped a.go:3: %v", m)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agents/coding/ -run 'TestAssembleDiffPayload|TestCommentableMap' -v`
Expected: FAIL — `undefined: assembleDiffPayload`, `undefined: commentableMap`.

- [ ] **Step 3: Bump the budget and add the functions**

In `internal/agents/coding/localreview.go`, add the `connectors` and `github` imports to the import block:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/platform/errs"
)
```

Change the total budget constant (line 44) from `48 << 10` to `64 << 10` and update its comment:

```go
const (
	localReviewMaxFileBytes  = 32 << 10 // skip any single file whose rendered hunks exceed this
	localReviewMaxTotalBytes = 64 << 10 // cap total rendered diff to fit localReviewNumCtx
	localReviewNumCtx        = 16384    // Ollama context window; ~64KB diff + prompt + output fits
)
```

Add the two functions (place them just after `readChangedFiles`, before `isLoopbackHost`):

```go
// assembleDiffPayload renders the changed files' hunks into the local-review
// payload: source files first (the budget is small; spend it on code), then
// path-sorted, stopping at the total budget. It returns the payload plus the paths
// it could not include — skipped (no usable patch: binary or omitted by GitHub) and
// omitted (dropped because the budget filled) — so callers can surface them.
func assembleDiffPayload(files []connectors.ChangedFile) (payload string, skipped, omitted []string) {
	ordered := append([]connectors.ChangedFile(nil), files...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ci := codeExt[strings.ToLower(filepath.Ext(ordered[i].Path))]
		cj := codeExt[strings.ToLower(filepath.Ext(ordered[j].Path))]
		if ci != cj {
			return ci // code before non-code
		}
		return ordered[i].Path < ordered[j].Path
	})
	var b strings.Builder
	total := 0
	for _, f := range ordered {
		rendered := github.RenderAnnotatedHunks(f.Patch)
		if rendered == "" {
			skipped = append(skipped, f.Path)
			continue
		}
		block := fmt.Sprintf("\n=== %s ===\n%s", f.Path, rendered)
		if len(block) > localReviewMaxFileBytes || total+len(block) > localReviewMaxTotalBytes {
			omitted = append(omitted, f.Path)
			continue
		}
		b.WriteString(block)
		total += len(block)
	}
	return b.String(), skipped, omitted
}

// commentableMap reduces ChangedFiles to the file→commentable-lines map buildReview
// needs to place inline comments.
func commentableMap(files []connectors.ChangedFile) map[string]map[int]bool {
	out := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		out[f.Path] = f.Commentable
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agents/coding/ -run 'TestAssembleDiffPayload|TestCommentableMap' -v`
Expected: PASS.

- [ ] **Step 5: Run the package to confirm nothing else broke**

Run: `go build ./internal/agents/coding/ && go test ./internal/agents/coding/ -run 'TestReadChangedFiles|TestLocalReview' -v`
Expected: PASS — `readChangedFiles`/`localReview` are unchanged and still compile (the new budget constant doesn't affect their tiny-file tests).

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/localreview.go internal/agents/coding/localreview_test.go
git commit -m "feat(007): diff payload assembler + commentable map (manyforge-fqo)"
```

---

### Task 4: Surface skipped/omitted files in the review body

Extend `buildReview`/`renderReviewBody` to append the skipped/omitted notes. Update the single caller to pass `nil, nil` for now (real values wired in Task 5) so the tree stays green.

**Files:**
- Modify: `internal/agents/coding/review.go`
- Modify: `internal/agents/coding/service.go:379` (caller — pass `nil, nil`)
- Test: `internal/agents/coding/review_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: new signatures
  - `func buildReview(doc FindingsDoc, changed map[string]map[int]bool, commitID string, skipped, omitted []string) connectors.Review`
  - `func renderReviewBody(summary string, leftover []connectors.Finding, inlineCount int, skipped, omitted []string) string`

- [ ] **Step 1: Update existing tests + add the notes test (failing)**

In `internal/agents/coding/review_test.go`:

Change the call in `TestBuildReview_SplitsInlineVsBody` from:
```go
	rev := buildReview(doc, changed, "sha123")
```
to:
```go
	rev := buildReview(doc, changed, "sha123", nil, nil)
```

Change the call in `TestBuildReview_NoDiffInfoPostsEverythingInBody` from:
```go
	rev := buildReview(doc, map[string]map[int]bool{}, "sha")
```
to:
```go
	rev := buildReview(doc, map[string]map[int]bool{}, "sha", nil, nil)
```

Append a new test:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agents/coding/ -run TestBuildReview -v`
Expected: FAIL — too many arguments to `buildReview` / `renderReviewBody` (signature mismatch); new test won't compile.

- [ ] **Step 3: Update `buildReview` and `renderReviewBody`**

In `internal/agents/coding/review.go`, change `buildReview`'s signature and its `renderReviewBody` call:

```go
func buildReview(doc FindingsDoc, changed map[string]map[int]bool, commitID string, skipped, omitted []string) connectors.Review {
	var comments []connectors.ReviewComment
	var leftover []connectors.Finding
	for _, f := range doc.Findings {
		if f.Line != nil {
			if lines, ok := changed[f.File]; ok && lines[*f.Line] {
				comments = append(comments, connectors.ReviewComment{
					Path: f.File,
					Line: *f.Line,
					Body: renderInlineComment(f),
				})
				continue
			}
		}
		leftover = append(leftover, f)
	}
	return connectors.Review{
		Summary:  doc.Summary,
		Findings: doc.Findings,
		Body:     renderReviewBody(doc.Summary, leftover, len(comments), skipped, omitted),
		CommitID: commitID,
		Comments: comments,
	}
}
```

Then change `renderReviewBody`'s signature and append the notes before `return b.String()`:

```go
func renderReviewBody(summary string, leftover []connectors.Finding, inlineCount int, skipped, omitted []string) string {
	var b strings.Builder
	b.WriteString("## 🤖 Automated code review\n\n")
	b.WriteString(summary)
	b.WriteString("\n\n")
	if inlineCount > 0 {
		fmt.Fprintf(&b, "_%d inline comment(s) posted on the changed lines._\n", inlineCount)
	}
	if len(leftover) > 0 {
		fmt.Fprintf(&b, "\n### Other findings (not on changed lines) — %d\n\n", len(leftover))
		for _, f := range leftover {
			loc := f.File
			if f.Line != nil {
				loc = fmt.Sprintf("%s:%d", f.File, *f.Line)
			}
			fmt.Fprintf(&b, "- **[%s]** `%s` — %s\n", strings.ToUpper(f.Severity), loc, f.Title)
			if strings.TrimSpace(f.Detail) != "" {
				b.WriteString("  " + f.Detail + "\n")
			}
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\nⓘ %d file(s) not reviewed (binary or too large): %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n⚠ %d file(s) omitted (diff too large for the review budget): %s\n", len(omitted), strings.Join(omitted, ", "))
	}
	return b.String()
}
```

- [ ] **Step 4: Update the caller so the package compiles**

In `internal/agents/coding/service.go` line 379, change:
```go
	review := buildReview(doc, changed, pr.HeadSHA)
```
to:
```go
	review := buildReview(doc, changed, pr.HeadSHA, nil, nil)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./internal/agents/coding/ && go test ./internal/agents/coding/ -run TestBuildReview -v`
Expected: PASS — all three `TestBuildReview*` pass.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/review.go internal/agents/coding/review_test.go internal/agents/coding/service.go
git commit -m "feat(007): surface skipped/omitted files in review body (manyforge-fqo)"
```

---

### Task 5: Wire the local path to diff-based review (and remove the old whole-file path)

Switch the service to `ChangedFiles` + the assembler, change `localReview` to take the rendered payload, update the prompt constants to describe diff input, and delete the now-dead `readChangedFiles`/`ChangedLines`. This is the cohesive switch-over that must land atomically.

**Files:**
- Modify: `internal/agents/coding/localreview.go` (`localReview` signature + body, prompt constants, delete `readChangedFiles`, drop `os` import)
- Modify: `internal/agents/coding/service.go` (call `ChangedFiles`, assemble once, pass payload + skipped/omitted)
- Modify: `internal/connectors/repo_connector.go` (remove `ChangedLines` from interface)
- Modify: `internal/connectors/github/client.go` (remove `ChangedLines`)
- Modify: `internal/connectors/github/client_test.go` (delete `TestChangedLines`)
- Modify: `internal/agents/coding/localreview_test.go` (`TestLocalReview*` use a payload string; delete `TestReadChangedFiles`)

**Interfaces:**
- Consumes: `assembleDiffPayload`, `commentableMap` (Task 3); `conn.ChangedFiles` (Task 2).
- Produces: `func localReview(ctx context.Context, client *http.Client, cred AICredential, payload string) (FindingsDoc, int64, int64, error)`.

- [ ] **Step 1: Update `localReview` tests to the payload signature, delete the readChangedFiles test (failing)**

In `internal/agents/coding/localreview_test.go`:

In `TestLocalReview`, change the call from:
```go
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, map[string]string{"service.go": "package x"})
```
to:
```go
	payload := "\n=== service.go ===\n@@ 1-1 @@\n    1 + package x\n"
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, payload)
```

In `TestLocalReview_RejectsNonLoopback`, change:
```go
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, cred, map[string]string{"a.go": "x"}); err == nil {
```
to:
```go
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n"); err == nil {
```

Delete `TestReadChangedFiles` entirely (the function it tests is removed this task). After deletion, `os` and `path/filepath` may be unused in the test file — remove them from the test's import block if `go vet` flags them (the assembler tests from Task 3 still use `strings` and `connectors`; `filepath`/`os` were only used by `TestReadChangedFiles`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agents/coding/ -run TestLocalReview -v`
Expected: FAIL — `localReview` still wants `map[string]string`, so the payload-string calls don't compile.

- [ ] **Step 3: Rewrite `localReview` to take the payload + update prompt constants**

In `internal/agents/coding/localreview.go`:

Update `reviewInstructions` — replace its final paragraph (line 30) with diff-aware wording:
```go
You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. If there are no genuine problems, return an empty findings array.`
```

Update `reviewSchemaLine` (line 33) — change "Review the provided file(s)" to "Review the provided diff hunks":
```go
const reviewSchemaLine = `Review the provided diff hunks and output ONLY a single JSON object — no prose, no markdown fences — matching exactly this schema: {"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}`
```

Delete the entire `readChangedFiles` function (lines 57-89).

Replace the `localReview` function header + payload-building block (lines 108-135) with:
```go
// localReview POSTs the rendered diff payload to a local OpenAI-compatible chat
// endpoint (Ollama/vLLM) and parses the findings with ParseFindings. No
// sandbox/opencode: small local models can't drive opencode's agent loop, and the
// model is on-host so there is nothing to isolate. The model gets NO tools
// (chat→JSON only), so prompt injection can at worst yield bogus advisory findings.
// Returns (doc, promptTokens, completionTokens, err).
func localReview(ctx context.Context, client *http.Client, cred AICredential, payload string) (FindingsDoc, int64, int64, error) {
	if !isLoopbackHost(cred.Host()) {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local review base URL must be loopback, got %q: %w", cred.Host(), errs.ErrValidation)
	}
	if strings.TrimSpace(payload) == "" {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: no reviewable changes for local review: %w", errs.ErrValidation)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model": cred.Model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewInstructions + "\n\n" + reviewSchemaLine},
			{"role": "user", "content": "Diff hunks to review:\n" + payload},
		},
		"stream":  false,
		"options": map[string]any{"temperature": 0, "num_ctx": localReviewNumCtx},
	})

	url := strings.TrimRight(cred.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
```

(The rest of `localReview` from `if err != nil { ... build local review request ... }` onward is unchanged.)

Remove `"os"` from the `localreview.go` import block (only `readChangedFiles` used it; `filepath` stays — `assembleDiffPayload` uses `filepath.Ext`).

- [ ] **Step 4: Switch the service to ChangedFiles + assembler**

In `internal/agents/coding/service.go`, replace lines 287-294 (the `ChangedLines` fetch) with:
```go
	// Fetch the PR's changed files (patch text + commentable lines) once. Used to
	// (a) render the diff-based review payload, (b) post findings as inline diff
	// comments, and (c) scope the sandbox. Best-effort: on failure `files` is nil and
	// the review degrades (cloud → whole-repo; local → "no reviewable changes").
	files, cerr := conn.ChangedFiles(ctx, prNumber)
	if cerr != nil {
		files = nil
	}
	changed := commentableMap(files)
	payload, skippedFiles, omittedFiles := assembleDiffPayload(files)
```

In the local branch, replace lines 310-311:
```go
		files := readChangedFiles(checkout, changedFilePaths(changed))
		d, in, out, lerr := localReview(ctx, s.localClient(), cred, files)
```
with:
```go
		d, in, out, lerr := localReview(ctx, s.localClient(), cred, payload)
```

In the `buildReview` call (now line ~379, with the `nil, nil` from Task 4), pass the real lists:
```go
	review := buildReview(doc, changed, pr.HeadSHA, skippedFiles, omittedFiles)
```

(The cloud branch still writes `review_files.txt` via `changedFilePaths(changed)` — unchanged; `review_diff.txt` is added in Task 6.)

- [ ] **Step 5: Remove `ChangedLines` from the interface, client, and its test**

In `internal/connectors/repo_connector.go`, delete the `ChangedLines` method and its doc comment from the `RepoConnector` interface (keep `ChangedFiles`).

In `internal/connectors/github/client.go`, delete the small delegating `ChangedLines` method added in Task 2, keeping `ChangedFiles`.

In `internal/connectors/github/client_test.go`, delete `TestChangedLines` (lines 73-94).

- [ ] **Step 6: Build + run the full coding & connectors suites**

Run: `go build ./... && go test ./internal/agents/coding/... ./internal/connectors/...`
Expected: PASS — `TestLocalReview*` pass with the payload signature; connectors compile with only `ChangedFiles`; no `readChangedFiles`/`ChangedLines` references remain.

- [ ] **Step 7: Vet + lint to catch unused imports**

Run: `go vet ./internal/agents/coding/... ./internal/connectors/... && make lint`
Expected: PASS — confirms the `os` import (and any test-file `os`/`filepath`) were correctly dropped.

- [ ] **Step 8: Commit**

```bash
git add internal/agents/coding/localreview.go internal/agents/coding/localreview_test.go internal/agents/coding/service.go internal/connectors/repo_connector.go internal/connectors/github/client.go internal/connectors/github/client_test.go
git commit -m "feat(007): diff-based local review — switch service to ChangedFiles + hunk payload (manyforge-fqo)"
```

---

### Task 6: Wire the cloud path + eval harness + image rebuild + live verify

Write the rendered diff for the sandbox, teach the entrypoint to prefer it, keep the eval harness prompt in sync, rebuild the image, and verify on PR #6.

**Files:**
- Modify: `internal/agents/coding/service.go` (write `review_diff.txt`)
- Modify: `deploy/sandbox/entrypoint.sh` (prefer the diff; updated instructions)
- Modify: `tools/local-model-eval/run.sh` (instruction sync)

**Interfaces:**
- Consumes: `payload` (Task 5, already computed before the provider branch).
- Produces: `/out/review_diff.txt` contract between host and sandbox.

- [ ] **Step 1: Write `review_diff.txt` in the cloud branch**

In `internal/agents/coding/service.go`, in the cloud (`else`) branch where `review_files.txt` is written (lines 318-323), add the diff write directly after the existing `review_files.txt` block:
```go
			// Hand opencode the rendered diff (annotated hunks) as the primary review
			// scope; it may still open the full files in the read-only checkout for
			// context. Absent/empty → falls back to review_files.txt (whole-file scope).
			if payload != "" {
				_ = os.WriteFile(filepath.Join(outDir, "review_diff.txt"), []byte(payload), 0o644)
			}
```

- [ ] **Step 2: Verify the service still builds + integration test (if Docker available)**

Run: `go build ./internal/agents/coding/`
Expected: PASS.

Run (best-effort, needs Colima + the dev image): `go test -tags integration -p 1 ./internal/agents/coding/`
Expected: PASS, or skipped if the sandbox image/Docker is unavailable in this environment (note it in the commit if skipped).

- [ ] **Step 3: Commit the host-side diff write**

```bash
git add internal/agents/coding/service.go
git commit -m "feat(007): write rendered diff to /out/review_diff.txt for the sandbox (manyforge-fqo)"
```

- [ ] **Step 4: Update the sandbox entrypoint to prefer the diff**

In `deploy/sandbox/entrypoint.sh`:

Replace the final line of `INSTRUCTIONS` (line 97, `Use the real file path and the line number in the current version of the file. ...`) with the diff-aware wording (keep it identical to the Go `reviewInstructions` final paragraph):
```sh
You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. If there are no genuine problems, return an empty findings array.'
```

Replace the SCOPE block (lines 99-104) with a diff-first version:
```sh
SCOPE='Review the code in the current project.'
if [ -s /out/review_diff.txt ]; then
  DIFF=$(cat /out/review_diff.txt)
  SCOPE="Review ONLY the following changed hunks from this pull request. Each block is headed by '=== <path> ===' and shows the changed lines with their current-file line numbers in the gutter. Cite those gutter line numbers. You MAY open the full files in the project for additional context.

${DIFF}"
elif [ -s /out/review_files.txt ]; then
  FILES=$(tr '\n' ' ' < /out/review_files.txt)
  SCOPE="Review ONLY these files changed in this pull request (paths are relative to the project root): ${FILES}
Focus on the changed code; do not report issues in files outside this list."
fi
```

- [ ] **Step 5: Sync the eval harness instructions**

In `tools/local-model-eval/run.sh`, update the instruction text (the block kept in sync, ending with the "current version of the file" sentence) to the same diff-aware final paragraph as above. The harness feeds fixed whole-file fixtures, so add a one-line comment noting it does not render hunks (exact format parity is not required here):
```sh
# NOTE: this harness sends whole-file fixtures, not rendered hunks; the prompt text
# is kept in sync with localreview.go/entrypoint.sh but the input format differs.
```

- [ ] **Step 6: Rebuild the sandbox image**

Run:
```bash
DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .
```
Expected: image builds successfully (the entrypoint is baked in).

- [ ] **Step 7: Commit the sandbox + harness changes**

```bash
git add deploy/sandbox/entrypoint.sh tools/local-model-eval/run.sh
git commit -m "feat(007): sandbox + eval harness consume diff hunks (manyforge-fqo)"
```

- [ ] **Step 8: Live-verify on PR #6**

Ensure air (`:8081`) and Ollama (`:11434`) are up. Trigger the **Local ReviewBot** agent `6aeb7a46` (ollama, qwen2.5-coder:14b) on PR #6 and confirm: real findings, posted as **inline** comments on changed lines, with any skipped/omitted files noted in the summary body. Capture the result for the handoff.

Optional: trigger a cloud review (ReviewBot `6c252395`) to exercise the sandbox diff path after the rebuild.

- [ ] **Step 9: Final full-suite gate**

Run:
```bash
go test ./internal/agents/coding/... ./internal/connectors/... && \
go vet ./... && make lint && \
go test -tags contract ./cmd/... && make sec-test
```
Expected: all PASS.

---

## Self-Review

**Spec coverage:**
- Annotated-hunk render → Task 1. ✓
- Retain patch / `ChangedFiles` → Task 2. ✓
- Assembler, budget 48→64KB, source-first, skipped/omitted buckets → Task 3. ✓
- Surface skipped/omitted in body + logs → Task 4 (body). *Note:* the spec also mentions a `log.Warn`; add it opportunistically in Task 5 Step 4 if a logger is in scope at that call site — the body note is the required surface, the log is best-effort.
- Local path wiring + prompt sync → Task 5. ✓
- Cloud path (`review_diff.txt` + entrypoint) + image rebuild + eval harness → Task 6. ✓
- DRY `commentableLines` refactor → Task 1. ✓
- Connector has single method (remove `ChangedLines`) → Task 5. ✓
- Safety net (fetch fails → cloud whole-repo, local "no reviewable changes") → preserved in Task 5 Step 4. ✓
- Test plan (ParseHunks/Render/assembler/buildReview, fakes n/a, contract/integration) → Tasks 1-6 + final gate. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `assembleDiffPayload(files []connectors.ChangedFile) (string, []string, []string)`, `commentableMap(...) map[string]map[int]bool`, `localReview(..., payload string)`, `buildReview(doc, changed, commitID, skipped, omitted)`, `renderReviewBody(summary, leftover, inlineCount, skipped, omitted)`, `ChangedFiles(ctx, prNumber) ([]ChangedFile, error)` — names/types used consistently across Tasks 2-6. ✓
```
