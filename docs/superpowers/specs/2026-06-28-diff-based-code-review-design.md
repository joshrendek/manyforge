# Diff-based code review — design

- **Date:** 2026-06-28
- **Issue:** `manyforge-fqo` item #1 (parent epic `manyforge-7ml`, Spec 007)
- **Status:** approved design, ready for implementation plan
- **Branch:** `feat/code-review-ui`

## Problem

The code-review agent sends **whole changed files** (capped to a byte budget) to
the model. This is the wrong altitude:

- On PR #6 the Local ReviewBot returned "No issues found" partly because
  `readChangedFiles` fills a ~48KB budget with whole source files, so the model
  spends its context re-reading unchanged code instead of focusing on the change.
- On the cloud path, gpt-4o "punted on the full manyforge tree and returned 0
  findings ('too extensive')", and a whole-repo review is ~4 min on gemini and
  dominates cost/latency.

GitHub already hands us the diff. `ChangedLines`
(`internal/connectors/github/client.go:100`) fetches each file's unified-diff
`patch`, parses it via `commentableLines` (`internal/connectors/github/diff.go:14`)
to get the inline-commentable line numbers, **and then throws the patch text
away**. The whole feature is: keep that patch, render it as annotated hunks, and
send the hunks instead of whole files — on both review paths, from one shared
renderer.

## Goals

1. Send the model the **changed hunks**, annotated with their real new-side
   (current-file) line numbers, instead of whole files.
2. Make the model's `line` citations precise and **in-diff**, so they reliably
   become inline PR comments (via the existing `buildReview` mapping) instead of
   falling back to the summary body.
3. Apply to **both** paths — local (host-side direct-API) and cloud (opencode
   sandbox) — from a single Go renderer (one source of truth).
4. Shrink payloads, reducing cost/latency and helping small local models.

## Non-goals (explicitly deferred)

- **Multi-call chunking** of very large diffs → deferred to `manyforge-206`
  (gemini 504 on large diffs). This increment uses a single call with a hard cap.
- **Key redaction in review output** and **provider generality beyond OpenRouter**
  (the other `manyforge-fqo` items) — not in this increment.
- **Showing removed (`-`) lines** in the rendered hunk. v1 renders added + context
  lines only (matches the approved format; removed lines have no new-side line
  number). The model can open full files for context on the cloud path. Possible
  future enhancement.

## Approved decisions

| # | Decision | Choice |
|---|----------|--------|
| 1 | What we send per file | **Annotated hunks** — changed hunks only, each line prefixed with its real new-side line number + a `+`/space marker. |
| 2 | Cloud/opencode path | **Diff + may read files** — write the rendered diff to `/out/review_diff.txt`; entrypoint tells opencode to review those changes and *may* open full files for context. |
| 3 | Diff exceeds budget | **Cap + explicit truncation** — fill deterministically (source files first, then path-sorted), drop overflow, surface it in prompt + log + summary body. No silent caps. Single model call. |
| 4 | File with no usable patch (binary / patch omitted) | **Skip + note** — review only files with a real patch; list skipped ones in the summary body. Keeps the payload uniform (all hunks). |

Safety net (independent of #4): a **whole** changed-files fetch failure keeps
today's exact behavior — the cloud path degrades to a whole-repo review
(`review_files.txt` absent), and the local path fails fast with the existing
"no reviewable files" `errs.ErrValidation` (it has no file list to work from).
This increment does not change either fallback.

## Approved render format

```
=== internal/foo/bar.go ===
@@ 41-48 @@
  41   func Foo(x int) error {
  42 +     if x < 0 {
  43 +         return nil   // swallows err
  44 +     }
  45       return doThing(x)
  46   }
```

- `=== <path> ===` header per file.
- `@@ <newStart>-<newEnd> @@` per hunk (new-side range).
- Each line: right-aligned new-side line number, a `+` (added) or space (context)
  marker, then the line text. Removed lines are omitted.

## Architecture & data flow

```
GitHub PR files API ──► patches (already fetched, currently dropped)
        │
        ▼
  ParseHunks(patch) ──► RenderAnnotatedHunks(patch)   ◄── single source of truth
        │                          │
        │               ┌──────────┴───────────┐
        ▼               ▼                      ▼
  commentableLines   local path            cloud path
  (buildReview)      assemble + POST   /out/review_diff.txt → entrypoint
                                        (opencode may also open files)
```

## Components

### 1. Structured hunk parser + renderer (`internal/connectors/github/diff.go`)

DRY refactor — there must be exactly one hunk parser. Today `commentableLines`
hand-parses hunks just to collect line numbers; lift that into a structured parser
that both `commentableLines` and the renderer consume.

```go
type DiffLine struct {
    NewLineNo int    // new-side line number
    Added     bool   // true if '+', false if context
    Text      string // line content without the +/space prefix
}

type Hunk struct {
    NewStart int
    Lines    []DiffLine // added + context only (removed lines excluded)
}

// ParseHunks parses a GitHub unified-diff patch into hunks carrying new-side line
// numbers. Returns nil for an empty/absent patch (binary or too-large files).
func ParseHunks(patch string) []Hunk

// RenderAnnotatedHunks renders a patch as gutter-numbered hunks (the approved
// format above). Returns "" when there are no hunks.
func RenderAnnotatedHunks(patch string) string
```

`commentableLines` is reimplemented in terms of `ParseHunks` (its existing tests
stay green). The renderer lives in the `github` package because it is pure
patch-formatting with no coding-domain knowledge; the `coding` package only wraps
each rendered block with its `=== path ===` header.

### 2. Connector method — fetch patch + commentable lines in one call (`internal/connectors/`)

`ChangedLines` is subsumed by a richer method so one API fetch serves both the
render payload and inline-comment validation:

```go
type ChangedFile struct {
    Path        string
    Patch       string       // raw unified diff (files[].patch); "" if binary/omitted
    Commentable map[int]bool // new-side commentable line numbers
}

// ChangedFiles returns the PR's changed files with patch text and commentable
// new-side lines. Files with no patch appear with empty Patch + Commentable.
ChangedFiles(ctx context.Context, prNumber int) ([]ChangedFile, error)
```

- Replace `ChangedLines` in `RepoConnector` (`repo_connector.go:18`) and the github
  client impl (`client.go:100`). `ChangedFiles` is a strict superset, so there is no
  reason to keep both.
- `buildReview` keeps its existing `map[string]map[int]bool` parameter; the service
  derives that map from `[]ChangedFile` with a tiny helper, so `buildReview` and its
  tests are untouched.
- Update all `RepoConnector` test fakes/mocks to implement `ChangedFiles`.

### 3. Diff payload assembler (`internal/agents/coding/localreview.go`)

Replaces `readChangedFiles`'s whole-file assembly. Given `[]ChangedFile` + the
checkout, it builds the user payload and reports what it dropped:

- **Fill order:** source files first (reuse `codeExt`), then path-sorted.
- **Budget:** total cap raised **48KB → 64KB** (`localReviewMaxTotalBytes`); keep a
  per-file cap (`localReviewMaxFileBytes`, 32KB) on the *rendered* hunk text.
- **Two reject buckets** (both surfaced, never silent):
  - `skipped` — file has no usable patch (binary / omitted by GitHub).
  - `omitted` — file dropped because the budget filled.
- Returns the assembled payload string plus the `skipped`/`omitted` path lists.

The local path POSTs this payload (same OpenAI-compatible call as today). The
existing loopback SSRF guard and token accounting are unchanged.

### 4. Cloud/opencode path (`internal/agents/coding/service.go` + `deploy/sandbox/entrypoint.sh`)

- Service writes the same rendered payload to `/out/review_diff.txt`, and keeps
  writing `review_files.txt` as the file read-scope.
- `entrypoint.sh` reads `review_diff.txt` and changes the prompt to: *"Review THESE
  changes (unified hunks; the gutter numbers are current-file line numbers). You
  MAY open the full files in the checkout for additional context."*
- **Requires a sandbox image rebuild** (`manyforge/opencode-sandbox:dev`).

### 5. Prompt sync (`localreview.go` ↔ `entrypoint.sh` ↔ `run.sh`)

- `reviewInstructions` / `reviewSchemaLine` change from *"Review the provided
  file(s)… line number in the current version of the file"* → *"Review the provided
  diff hunks; cite the gutter line numbers shown for each changed line."*
- Keep the three copies in sync (the existing KEEP-IN-SYNC comment). The bash eval
  harness (`tools/local-model-eval/run.sh`) gets the updated instruction text; exact
  Go-renderer parity is not required there (it feeds fixed fixtures), but note the
  divergence in a comment.

### 6. Surfacing skipped/omitted (`internal/agents/coding/review.go`)

`renderReviewBody` (or the service before calling `buildReview`) appends to the
summary body:

- `ⓘ N files not reviewed (binary/too large): a, b, …` (skipped)
- `⚠ N files omitted (diff too large): x, y, …` (omitted)

And a `log.Warn` with `skipped`/`omitted` counts. This satisfies the "no silent
caps" rule.

## Edge cases

- **Empty patch / binary file** → `RenderAnnotatedHunks` returns `""`; assembler
  buckets it as `skipped`.
- **Whole `ChangedFiles` fetch fails** → today's behavior: cloud degrades to
  whole-repo; local fails fast with "no reviewable files".
- **Pure-deletion file** (no new-side lines) → no hunks to render → `skipped`.
- **All files skipped/omitted** → local path returns the existing
  `errs.ErrValidation` "no reviewable files"; the body still notes what was skipped.
- **Finding cites an off-diff line** → unchanged: `buildReview` already routes it to
  the body. Annotated line numbers should make this rare.

## Test plan

Automated tests are required (per project policy). All must pass before push.

### Unit — `internal/connectors/github/diff_test.go`
- `ParseHunks`: single hunk, multi-hunk, correct new-side numbering across
  added/context/removed lines, `@@`-header parsing, empty/binary → nil.
- `RenderAnnotatedHunks`: exact gutter format for a known patch; multi-hunk; empty
  → `""`.
- Existing `TestCommentableLines*` stay green (now backed by `ParseHunks`).

### Unit — `internal/agents/coding` (new assembler test, replaces `TestReadChangedFiles`)
- Fill order (source files before non-source), per-file cap, budget overflow →
  `omitted`, patchless → `skipped`, payload contains rendered headers + hunks.
- Surfacing: body contains the skipped/omitted notes.

### Unit — existing, keep green
- `localreview_test.go` `TestLocalReview` / `TestLocalReview_RejectsNonLoopback`
  (adjust payload expectations to the new format).
- `review_test.go` `TestBuildReview_*` (signature unchanged).
- `findings_test.go` unchanged.

### Connector fakes
- Every `RepoConnector` fake implements `ChangedFiles`; remove `ChangedLines`.

### Integration / contract (keep green)
- `service_integration_test.go`, `worker_integration_test.go`
  (`-tags integration -p 1`).
- `go test -tags contract ./cmd/...` (OpenAPI drift), `go vet ./...`,
  `make lint` (staticcheck).

### Live verification (manual, then noted in HANDOFF)
- Trigger Local ReviewBot `6aeb7a46` (ollama, qwen2.5-coder:14b) on PR #6 → expect
  real, in-diff findings posted as inline comments.
- Optional: a cloud review after the sandbox rebuild.

## Files touched

| File | Change |
|------|--------|
| `internal/connectors/github/diff.go` | `Hunk`/`DiffLine`, `ParseHunks`, `RenderAnnotatedHunks`; reimplement `commentableLines`. |
| `internal/connectors/github/client.go` | `ChangedFile` fetch (patch + commentable) → `ChangedFiles`; drop `ChangedLines`. |
| `internal/connectors/repo_connector.go` | Interface: `ChangedLines` → `ChangedFiles` + `ChangedFile` type. |
| `internal/agents/coding/localreview.go` | Diff payload assembler (replaces `readChangedFiles`); budget 48→64KB; updated prompt constants. |
| `internal/agents/coding/service.go` | Call `ChangedFiles`; derive `changed` map; write `review_diff.txt`; pass payload to local path. |
| `internal/agents/coding/review.go` | Append skipped/omitted notes to the body. |
| `deploy/sandbox/entrypoint.sh` | Read `review_diff.txt`; updated prompt; rebuild image. |
| `tools/local-model-eval/run.sh` | Updated instruction text. |
| Test files | New diff/renderer/assembler tests; update fakes; adjust payload expectations. |

## Rollout / verification

1. Land all unit + integration tests green; `go vet`, `make lint`,
   `go test -tags contract ./cmd/...`.
2. Rebuild the sandbox image:
   `DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
3. Live-verify on PR #6 with Local ReviewBot `6aeb7a46`.
4. Update `HANDOFF.md`; close `manyforge-fqo` #1 (file follow-ups for the deferred
   items if not already tracked).
```
