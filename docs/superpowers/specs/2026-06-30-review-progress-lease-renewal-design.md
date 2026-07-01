# Long-running review: lease-renewal + live progress — design

- **Date:** 2026-06-30
- **Issue:** `manyforge-206` follow-on (long-running local reviews + UI progress). Parent epic `manyforge-7ml`, Spec 007.
- **Status:** approved design, ready for implementation plan
- **Branch:** `fix/code-review-fallback-model` (bundled onto the open PR #7)

## Problem

A large local model (e.g. `ornith:35b`) reviewing a PR can run 15-25 min. Today three things break or are missing:

1. **HTTP client timeout.** `localClient()` uses `s.timeout()` = 10 min (`service.go:116-121`); a long local POST to Ollama times out and the review fails.
2. **Worker lease re-claim (correctness bug).** `claim_code_reviews` (migration `0073`) selects `(status='running' AND lease_expires_at < now())`. The base lease is 900s (`worker.go:73-75`). If a job's wall-clock exceeds the lease, **another worker tick re-claims the still-running row** (bumping `attempts`) → a second review runs concurrently. Idempotent posting hides the duplicate GitHub comment, but two model runs execute and the first lease is silently stolen. So a longer timeout *requires* lease handling.
3. **No progress visibility.** During the long `running` phase the UI (`detail.ts`/`list.ts`, polling every 3s) shows only the bare `status`.

Facts from the code map: the worker runs `runJob` **synchronously** in `processOne` (`worker.go:153`) — no goroutine exists today; the DEFINER queue functions (`requeue`/`fail`, `0073`) are the model for a renew function; `localReview` posts with `"stream": false` (`localreview.go:127-135`); `GetCodeReview` is `SELECT *` (`code_review.sql:50-51`); the local path is a single chat→JSON call with **no tool calls**.

## Goal

Let a long local review run to completion without being re-claimed, and show live progress in the UI — including a streaming preview of the model's output — using one heartbeat mechanism.

## Approved decisions

| # | Decision | Choice |
|---|----------|--------|
| 1 | Progress richness | **Live output preview**: stream the model output and render it growing live in the UI (redacted), plus phase + a live elapsed timer. |
| 2 | Branch | **Bundle** onto `fix/code-review-fallback-model` (PR #7 grows to fallback + progress). |
| — | Lease | Add **renewal** (heartbeat); base lease stays 900s (crash-detection window). |
| — | Local timeout | New config, **default 30 min**, local path only. |

## The unifying insight

The lease-renewal heartbeat and the progress updater are the **same goroutine**. One ~5s tick calls one DEFINER function — `renew_code_review_lease(id, lease, progress_jsonb)` — that **renews the lease AND persists `{phase, tokens, preview}`**. `runJob` stays DB-free for progress (it mutates a shared holder); the worker persists. Token streaming is deliberately *not* the headline signal: for a big model the long wait is **prompt evaluation** (time-to-first-token), during which no API exposes progress and token count is stuck at 0 — so **elapsed + phase** is the honest baseline, and the streamed output preview is the rich signal once generation starts.

## Components

### A. Database — migration `0076_code_review_progress`
- `ALTER TABLE code_review ADD COLUMN progress jsonb;` (nullable; null until the first heartbeat).
- `renew_code_review_lease`:
  ```sql
  CREATE FUNCTION renew_code_review_lease(p_id uuid, p_lease_seconds int, p_progress jsonb) RETURNS void
  LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
      UPDATE code_review SET
          lease_expires_at = now() + make_interval(secs => p_lease_seconds),
          progress = p_progress,
          updated_at = now()
      WHERE id = p_id AND status = 'running';
  $$;
  REVOKE ALL ON FUNCTION renew_code_review_lease(uuid, int, jsonb) FROM PUBLIC;
  GRANT EXECUTE ON FUNCTION renew_code_review_lease(uuid, int, jsonb) TO manyforge_app;
  ```
  The `status='running'` guard makes a renew after terminal a harmless no-op (race-safe). A matching `.down.sql` drops the function + column.
- `db/schema.sql`: mirror the column add (the DEFINER function is excluded from schema.sql per the established convention for the claim functions — `[[security-regression-pins-grep-source-literals]]` / the 0073 precedent).
- A new sqlc query is **not** needed for renew (raw `tx.Exec` in the adapter, like `requeue`/`fail`).

### B. `Progress` holder — `internal/agents/coding/progress.go` (package `coding`)
```go
type Progress struct {
    mu         sync.Mutex
    phase      string
    tokens     int
    rawPartial string
    secrets    []string
}
func (p *Progress) SetPhase(phase string)                 // nil-safe
func (p *Progress) SetSecrets(secrets ...string)          // nil-safe
func (p *Progress) UpdateStream(tokens int, partial string) // nil-safe
func (p *Progress) Snapshot() []byte                      // nil → nil; else json {phase, tokens, preview}
```
`Snapshot` builds `{"phase":…, "tokens":…, "preview": redactSecrets(tail(rawPartial, previewMaxBytes), secrets…)}` — redaction runs **once per snapshot** (every ~5s), not per token. `previewMaxBytes` caps the preview (e.g. 4KB tail) so the jsonb stays small.

### C. Worker heartbeat — `worker.go`
In `processOne`, wrap the existing synchronous `runJob` call:
```go
prog := &Progress{}
stop := make(chan struct{})
go func() {
    t := time.NewTicker(w.heartbeatInterval()) // ~5s, default
    defer t.Stop()
    for {
        select {
        case <-stop: return
        case <-ctx.Done(): return
        case <-t.C:
            _ = w.DB.RenewLease(ctx, job.ID, w.LeaseSeconds, prog.Snapshot())
        }
    }
}()
jobErr = runJob(ctx, job, prog)
close(stop)
```
New `systemDB` method + `AppDBAdapter.RenewLease` (modeled on `RequeueCodeReview`):
```go
func (a *AppDBAdapter) RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error {
    return a.DB.WithTx(ctx, func(tx pgx.Tx) error {
        _, err := tx.Exec(ctx, "SELECT renew_code_review_lease($1, $2, $3)", id, leaseSeconds, progress)
        return err
    })
}
```
`HeartbeatInterval` is a worker field with a default (~5s); base `LeaseSeconds` unchanged (900s).

### D. `runJob` phases + streaming `localReview`
- `runJob(ctx context.Context, job ClaimedReview, prog *Progress) error` — the new param threads through. Direct test callers pass `nil`.
  - After cred resolve: `prog.SetSecrets(cred.APIKey, rc.Credential.APIToken)`.
  - Phase markers: `prog.SetPhase("preparing")` at entry; `"reviewing"` just before the provider branch (`service.go:327`); `"posting"` just before `conn.PostReview` (`service.go:408`).
- `localReview` gains `prog *Progress` and switches to `"stream": true` **with `"stream_options": {"include_usage": true}`** (in OpenAI-compat streaming, `usage` only arrives if requested — without it, token accounting silently goes to 0). It reads the SSE stream (`data: {…}` lines until `data: [DONE]`), accumulates `choices[0].delta.content` into a buffer, and calls `prog.UpdateStream(approxTokens, buffer)` as chunks arrive (throttled — at most every ~500ms). The terminal chunk carries `usage`; if absent, `completionTokens` falls back to the streamed-chunk count and `promptTokens` to 0 (best-effort, never blocks). On `[DONE]` it runs the SAME `ParseFindings(buffer)` and returns `(doc, promptTokens, completionTokens, err)`. **No change to the parse/redaction contract** — `redactDoc` in `runJob` still scrubs the final posted/stored doc, and the existing `TestLocalReview` is updated to drive an SSE mock. The loopback SSRF guard is unchanged.

### E. Longer local timeout — `service.go`
- New config `MANYFORGE_LOCAL_REVIEW_TIMEOUT` (parsed in platform/config), surfaced as `s.LocalTimeout time.Duration`. `s.localTimeout()` returns it or the **30 min** default.
- `localClient()` uses `s.localTimeout()` (was `s.timeout()`). The cloud sandbox path keeps `s.timeout()` (10 min). No per-job ctx deadline shorter than 30 min bounds the local call (the worker ctx has none).

### F. API + frontend
- `CodeReview` struct (`service.go:30-42`) gains `Progress json.RawMessage \`json:"progress,omitempty"\``, populated from the row in `Get` (`SELECT *` already returns the column). `ListCodeReviews` (explicit columns) adds `progress` so the list can show a phase badge.
- TS `CodeReview` (`core/code-review.service.ts:37-49`) gains `progress?: { phase: string; tokens: number; preview: string }`.
- `detail.ts`: when `status === 'running'`, render: the **phase** (e.g. "Reviewing with ornith:35b"), a **live elapsed timer** (a 1s `setInterval` computing `now - created_at`, cleared on terminal/destroy), and the **preview** in a monospace, scroll-pinned box that updates each 3s poll. `list.ts`: a small phase label on running rows (no preview).

### G. Security-regression
`MF007-PIN-12` in `internal/security_regression/coding_review_pins_test.go` (or a new file): glob `migrations/0076_*.up.sql`, assert it contains `SECURITY DEFINER` and `SET search_path` (modeled on `MF007-PIN-9`). Runs under `make sec-test`.

## Edge cases

- **Prompt-eval phase:** preview is empty, tokens 0 — the elapsed timer + "reviewing" phase carry liveness (by design; no API exposes prompt-eval progress).
- **Post-terminal renew race:** the `status='running'` guard no-ops a renew that lands after success/fail.
- **Worker shutdown mid-run:** the heartbeat goroutine exits on `ctx.Done()`; the lease then expires after 900s and crash-recovery re-claims (existing behavior).
- **Cloud path:** unchanged — `prog.SetPhase` still marks phases, but there is no streamed preview (opencode is a black box here); the lease is still renewed, which also benefits long cloud runs.
- **Preview secret safety:** the streamed preview is `redactSecrets`-scrubbed before persistence; and the local model cannot read the real key (no tools, key not in prompt).

## Non-goals (YAGNI)

- Tool-call progress (the local path has no tools; only the cloud/opencode path does — out of scope).
- Websocket/SSE to the browser (3s polling of `progress` is sufficient).
- Per-row streaming preview in the list view (phase label only).
- Env-overridable heartbeat interval (hardcoded default; the local timeout is the one knob exposed).

## Test plan

Automated tests required; all green before push.

### Unit — `internal/agents/coding`
- `progress_test.go`: `SetPhase`/`UpdateStream`/`Snapshot` shape; `Snapshot` redacts a planted secret in the preview tail; nil-receiver methods are no-ops; preview is tail-capped.
- `localreview_test.go`: a mock SSE server streams chunked `delta.content`; assert `localReview` accumulates the full content, `ParseFindings` succeeds, `prog.UpdateStream` was called, and token counts return. Keep `TestLocalReview_RejectsNonLoopback` green.

### Integration — `internal/agents/coding/service_integration_test.go` (the load-bearing test)
- A blocking fake review (runner/localReview that waits on a signal): assert (1) `lease_expires_at` advances during the run (heartbeat renewed it), (2) `progress` is non-null mid-run, (3) a concurrent `ClaimCodeReviews` while the row is `running` with a fresh lease returns **0 rows** — proving renewal prevents the double-claim. Then release the fake and assert it reaches `succeeded`.

### Security-regression
- `MF007-PIN-12` (source pin on `0076`). Under `make sec-test`.

### Frontend — `web/src/app/pages/code-review/detail.spec.ts`
- A `running` review with a `progress` payload renders the phase + the preview text; a `succeeded` review hides the progress block.

### Gates (whole repo, before push)
- `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`; `make lint`; `go test -tags contract ./cmd/...`; `make sec-test`; integration `go test -tags integration -p 1 ./internal/agents/coding/`; `make generate` (sqlc) if queries change; frontend `cd web && npm test`.

## Files touched

| File | Change |
|------|--------|
| `migrations/0076_code_review_progress.{up,down}.sql` | progress column + `renew_code_review_lease` DEFINER |
| `db/schema.sql` | mirror the column add |
| `db/query/code_review.sql` | add `progress` to `ListCodeReviews` |
| `internal/agents/coding/progress.go` (+ test) | `Progress` holder |
| `internal/agents/coding/worker.go` | heartbeat goroutine + `RenewLease` adapter + `systemDB` method + `heartbeatInterval` |
| `internal/agents/coding/service.go` | `runJob(…, prog)`, phase markers, `CodeReview.Progress`, `localTimeout()`/`localClient()` |
| `internal/agents/coding/localreview.go` (+ test) | streaming + `prog` param |
| `internal/platform/config` | `MANYFORGE_LOCAL_REVIEW_TIMEOUT` |
| `internal/security_regression/…` | `MF007-PIN-12` |
| `web/src/app/core/code-review.service.ts` | `progress` on the TS type |
| `web/src/app/pages/code-review/{detail,list}.ts` (+ detail.spec) | phase + elapsed + live preview |

## Rollout / verification

1. Land all tests green (units, integration, sec-test, contract, frontend); `make generate` if needed; `go vet`/`make lint`.
2. `air` rebuilds the branch (host-side; no sandbox image change). Verify with a live **`ornith:35b`** review on PR #7: watch the detail page show `preparing → reviewing` + the elapsed timer + the streaming preview, the lease stay renewed (no re-claim), and the review reach `succeeded` past the old 10-min mark.
3. Update `HANDOFF.md`. PR #7 now carries fallback + progress.
