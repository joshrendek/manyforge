# Long-running Review: Lease-Renewal + Live Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a long local code review (e.g. `ornith:35b`, 15-25 min) run to completion without being re-claimed mid-flight, and show live progress (phase + elapsed + a streaming output preview) in the UI.

**Architecture:** One worker heartbeat goroutine per running job ticks every ~5s and calls a single SECURITY DEFINER function `renew_code_review_lease(id, lease, progress_jsonb)` that **renews the lease AND persists `{phase, tokens, preview}`**. `runJob` stays DB-free for progress — it mutates a shared in-memory `*Progress`; the worker persists it. `localReview` switches to a streamed SSE read so the preview grows live. A new 30-min local-only HTTP timeout replaces the 10-min sandbox cap on the local path.

**Tech Stack:** Go (pgx v5, sqlc), PostgreSQL (RLS + SECURITY DEFINER queue functions), Angular (signals, vitest), Ollama OpenAI-compatible streaming API.

## Global Constraints

- **Branch:** bundle ALL tasks onto `fix/code-review-fallback-model` (the open PR #7). Do NOT branch fresh. One commit per task.
- **sqlc is PINNED to v1.27.0.** `make generate` runs bare `sqlc generate` which uses the dev-global v1.31.1 and re-churns every generated file. Always regenerate with `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate` and confirm `git diff --stat internal/platform/db/dbgen/` shows ONLY `code_review`-related changes. See [[sqlc-version-pin-v127]].
- **SECURITY DEFINER functions are EXCLUDED from `db/schema.sql`** (the 0073 claim-functions precedent). schema.sql mirrors only the column add; the function lives in the migration only. sqlc reads schema.sql, so the column MUST be in schema.sql for generation to see it.
- **Base lease stays 900s** (crash-detection window). Heartbeat interval default ~5s. Local review timeout default 30 min.
- **`gopls` diagnostics lie after edits** (phantom `undefined`/`dbgen.* undefined`/`too many arguments`). `go build`/`go test`/`go vet` is truth. See [[gopls-stale-dbgen-diagnostics]].
- **Source-level security pins break on literal refactors** — if you rename a literal a pin greps for, update the pin in the same change. See [[security-regression-pins-grep-source-literals]].
- Go style: wrap errors with context; typed sentinels in `errs`; never echo `err.Error()` to clients (last_error is server-side only). All methods on `*Progress` are nil-safe.
- **Verification gates (whole repo, all green before push):** `go build ./...`; `go vet ./...`; `go test ./internal/agents/coding/... ./internal/connectors/... ./internal/platform/config/...`; `go test ./internal/security_regression/` (or `make sec-test`); `go test -tags contract ./cmd/...`; `go test -tags integration -p 1 ./internal/agents/coding/`; `make lint`; frontend `cd web && npm test`.

---

### Task 1: DB layer — migration 0076, schema mirror, ListCodeReviews column, regen, MF007-PIN-12

**Files:**
- Create: `migrations/0076_code_review_progress.up.sql`
- Create: `migrations/0076_code_review_progress.down.sql`
- Modify: `db/schema.sql:627` (code_review table — add `progress jsonb`)
- Modify: `db/query/code_review.sql:55-56` (ListCodeReviews — add `progress`)
- Modify: `internal/security_regression/coding_review_pins_test.go` (add `TestMF007PIN12`)
- Regenerate: `internal/platform/db/dbgen/*` (sqlc v1.27.0)

**Interfaces:**
- Produces: SQL function `renew_code_review_lease(p_id uuid, p_lease_seconds int, p_progress jsonb) RETURNS void`; nullable column `code_review.progress jsonb`; generated `dbgen` rows for `GetCodeReview`/`ListCodeReviews` gain a `Progress []byte` field (consumed by Task 7).

- [ ] **Step 1: Write the failing pin test**

Add to `internal/security_regression/coding_review_pins_test.go` (mirrors `TestMF007PIN9`):

```go
// MF007-PIN-12 (manyforge-206 follow-on): the lease-renewal heartbeat persists
// progress + renews the lease principal-less, so it MUST be a SECURITY DEFINER
// function with a pinned search_path (migrations/0076), exactly like the 0073 claim
// functions. If 0076 loses SECURITY DEFINER or the search_path pin, the heartbeat
// either no-ops under RLS in prod (lease never renewed → long jobs re-claimed) or
// becomes search_path-hijackable — this test makes either regression a CI failure.
func TestMF007PIN12(t *testing.T) {
	matches, err := filepath.Glob("../../migrations/0076_*.up.sql")
	if err != nil {
		t.Fatalf("glob 0076 migration: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("migrations/0076_*.up.sql not found — the lease-renewal DEFINER migration is missing (MF007-PIN-12)")
	}
	src := mustRead(t, matches[0])
	if !strings.Contains(src, "renew_code_review_lease") {
		t.Fatalf("%s missing renew_code_review_lease — the heartbeat function (MF007-PIN-12)", matches[0])
	}
	if !strings.Contains(src, "SECURITY DEFINER") {
		t.Fatalf("%s missing SECURITY DEFINER — the principal-less renew would be RLS-blocked in prod (MF007-PIN-12)", matches[0])
	}
	if !strings.Contains(src, "SET search_path") {
		t.Fatalf("%s missing SET search_path — SECURITY DEFINER functions must pin search_path against hijack (MF007-PIN-12)", matches[0])
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/security_regression/ -run TestMF007PIN12 -v`
Expected: FAIL — "migrations/0076_*.up.sql not found".

- [ ] **Step 3: Create the up migration**

Create `migrations/0076_code_review_progress.up.sql`:

```sql
-- 0076: long-running code reviews (manyforge-206 follow-on) — live progress +
-- lease renewal. A large local model (e.g. ornith:35b) can review for 15-25 min,
-- exceeding the 900s claim lease; claim_code_reviews (0073) would then RE-CLAIM the
-- still-running row (status='running' AND lease_expires_at < now()) and start a
-- second concurrent run. The worker heartbeat renews the lease (and persists
-- progress) every ~5s so a live run is never re-claimed.
--
-- The worker renews principal-less (no manyforge.principal_id GUC), but code_review
-- has RLS ENABLEd (0071) and the app connects as manyforge_app (NOBYPASSRLS), so a
-- raw UPDATE is RLS-blocked. So renewal routes through a SECURITY DEFINER function
-- whose owner bypasses RLS — exactly the 0073 claim/requeue/fail pattern. search_path
-- is pinned to public so the body can't be hijacked by a caller-controlled path.

ALTER TABLE code_review ADD COLUMN progress jsonb;

-- Renew a running row's lease AND persist its progress snapshot in one statement.
-- The status='running' guard makes a renew that lands AFTER terminal (success/fail)
-- a harmless no-op (race-safe). A nil/NULL p_progress leaves progress untouched-as-
-- NULL on the first tick before any phase is set.
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

Create `migrations/0076_code_review_progress.down.sql`:

```sql
DROP FUNCTION IF EXISTS renew_code_review_lease(uuid, int, jsonb);
ALTER TABLE code_review DROP COLUMN IF EXISTS progress;
```

- [ ] **Step 4: Run the pin test to verify it passes**

Run: `go test ./internal/security_regression/ -run TestMF007PIN12 -v`
Expected: PASS.

- [ ] **Step 5: Mirror the column in schema.sql**

In `db/schema.sql`, inside `CREATE TABLE code_review (...)`, add the column after `cost_cents bigint NOT NULL DEFAULT 0,` (line 627) and before `UNIQUE (id, tenant_root_id),`:

```sql
    cost_cents         bigint NOT NULL DEFAULT 0,
    progress           jsonb,
    UNIQUE (id, tenant_root_id),
```

(Do NOT add the function to schema.sql — DEFINER functions are excluded, per the 0073 precedent.)

- [ ] **Step 6: Add `progress` to the ListCodeReviews query**

In `db/query/code_review.sql`, the `ListCodeReviews` SELECT list (line 55) — add `progress`:

```sql
-- name: ListCodeReviews :many
SELECT id, repo_connector_id, pr_number, status, summary, findings,
       external_review_ref, created_at, posted_at, model, cost_cents, progress
FROM code_review
WHERE business_id = $1
ORDER BY created_at DESC
LIMIT 200;
```

- [ ] **Step 7: Regenerate sqlc with the pinned version**

Run: `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate`
Then: `git diff --stat internal/platform/db/dbgen/`
Expected: ONLY files touching `code_review` change (the model + GetCodeReview + ListCodeReviews rows gain `Progress []byte`). If every generated file churned, you used the wrong sqlc version — `git checkout internal/platform/db/dbgen/` and re-run with `@v1.27.0`.

- [ ] **Step 8: Verify build + apply migration to dev DB**

Run: `go build ./... && go vet ./...`
Expected: clean (the new generated `Progress` field is unused — fine).
Apply to dev DB: `set -a; . ./.air.env; set +a; migrate -path migrations -database "$MANYFORGE_DATABASE_URL" up`
Expected: `0076` applied. (Integration tests in later tasks apply all migrations automatically via testdb.)

- [ ] **Step 9: Commit**

```bash
git add migrations/0076_code_review_progress.up.sql migrations/0076_code_review_progress.down.sql \
        db/schema.sql db/query/code_review.sql internal/platform/db/dbgen/ \
        internal/security_regression/coding_review_pins_test.go
git commit -m "feat(007): code_review progress column + lease-renewal DEFINER (manyforge-206)"
```

---

### Task 2: Progress holder

**Files:**
- Create: `internal/agents/coding/progress.go`
- Test: `internal/agents/coding/progress_test.go`

**Interfaces:**
- Produces: `type Progress struct{…}` with nil-safe methods `SetPhase(string)`, `SetSecrets(...string)`, `UpdateStream(tokens int, partial string)`, `Snapshot() []byte` (nil until a phase is set). `Snapshot` JSON shape: `{"phase":string,"tokens":int,"preview":string}` (unexported `progressSnapshot`). Const `previewMaxBytes`. Consumed by Tasks 3, 5, 6.

- [ ] **Step 1: Write the failing tests**

Create `internal/agents/coding/progress_test.go`:

```go
package coding

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProgressSnapshotShape(t *testing.T) {
	p := &Progress{}
	p.SetPhase("reviewing")
	p.UpdateStream(42, "hello world")
	b := p.Snapshot()
	if b == nil {
		t.Fatal("Snapshot nil after SetPhase")
	}
	var s progressSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Phase != "reviewing" || s.Tokens != 42 || s.Preview != "hello world" {
		t.Fatalf("snapshot=%+v", s)
	}
}

func TestProgressSnapshotNilUntilPhase(t *testing.T) {
	p := &Progress{}
	p.UpdateStream(5, "partial")
	if p.Snapshot() != nil {
		t.Fatal("Snapshot must be nil before any phase is set (so pre-heartbeat renew leaves progress NULL)")
	}
	p.SetPhase("preparing")
	if p.Snapshot() == nil {
		t.Fatal("Snapshot must be non-nil after a phase is set")
	}
}

func TestProgressSnapshotRedactsSecret(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	p := &Progress{}
	p.SetPhase("reviewing")
	p.SetSecrets(secret)
	p.UpdateStream(1, "the model echoed "+secret+" oops")
	var s progressSnapshot
	_ = json.Unmarshal(p.Snapshot(), &s)
	if strings.Contains(s.Preview, secret) {
		t.Fatalf("secret leaked into preview: %q", s.Preview)
	}
	if !strings.Contains(s.Preview, redactedMarker) {
		t.Fatalf("preview not redacted: %q", s.Preview)
	}
}

func TestProgressPreviewTailCapped(t *testing.T) {
	p := &Progress{}
	p.SetPhase("reviewing")
	p.UpdateStream(1, strings.Repeat("x", previewMaxBytes*3))
	var s progressSnapshot
	_ = json.Unmarshal(p.Snapshot(), &s)
	if len(s.Preview) > previewMaxBytes {
		t.Fatalf("preview not tail-capped: len=%d max=%d", len(s.Preview), previewMaxBytes)
	}
}

func TestProgressNilReceiverIsNoOp(t *testing.T) {
	var p *Progress // nil
	p.SetPhase("x")
	p.SetSecrets("y")
	p.UpdateStream(1, "z")
	if p.Snapshot() != nil {
		t.Fatal("nil *Progress Snapshot must be nil")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/coding/ -run TestProgress -v`
Expected: FAIL — `undefined: Progress` / `progressSnapshot`.

- [ ] **Step 3: Write the implementation**

Create `internal/agents/coding/progress.go`:

```go
package coding

import (
	"encoding/json"
	"sync"
	"unicode/utf8"
)

// previewMaxBytes caps the streamed-output preview persisted in code_review.progress
// (the tail of the model's output). Keeps the jsonb small so the ~5s heartbeat write
// stays cheap, while being enough to "watch it write".
const previewMaxBytes = 4 << 10

// Progress is a goroutine-safe holder for a code review's live progress. runJob and
// the streaming localReview mutate it in-memory; the worker heartbeat reads
// Snapshot() every ~5s and persists it via renew_code_review_lease. All methods are
// nil-safe so direct (non-worker) callers can pass a nil *Progress.
type Progress struct {
	mu         sync.Mutex
	phase      string
	tokens     int
	rawPartial string
	secrets    []string
}

// SetPhase records the current pipeline phase ("preparing"/"reviewing"/"posting").
func (p *Progress) SetPhase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.phase = phase
	p.mu.Unlock()
}

// SetSecrets records secret values to scrub from the preview before persistence.
func (p *Progress) SetSecrets(secrets ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.secrets = append(p.secrets, secrets...)
	p.mu.Unlock()
}

// UpdateStream records the latest completion-token count and the accumulated raw
// model output so far (the full buffer; Snapshot tail-caps + redacts it).
func (p *Progress) UpdateStream(tokens int, partial string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.tokens = tokens
	p.rawPartial = partial
	p.mu.Unlock()
}

// progressSnapshot is the JSON shape persisted to code_review.progress and returned
// to the API (mirrored by the TS CodeReview.progress type).
type progressSnapshot struct {
	Phase   string `json:"phase"`
	Tokens  int    `json:"tokens"`
	Preview string `json:"preview"`
}

// Snapshot returns the JSON to persist, or nil if no phase has been set yet (so a
// pre-heartbeat renew leaves progress NULL). Redaction runs once per snapshot
// (every ~5s), not per token.
func (p *Progress) Snapshot() []byte {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == "" {
		return nil
	}
	preview := redactSecrets(tailBytes(p.rawPartial, previewMaxBytes), p.secrets...)
	b, err := json.Marshal(progressSnapshot{Phase: p.phase, Tokens: p.tokens, Preview: preview})
	if err != nil {
		return nil
	}
	return b
}

// tailBytes returns the last max bytes of s, trimmed forward to a valid UTF-8 rune
// boundary so the resulting JSON string stays valid.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	t := s[len(s)-max:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return t
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/coding/ -run TestProgress -v`
Expected: PASS (all 5).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/progress.go internal/agents/coding/progress_test.go
git commit -m "feat(007): Progress holder for live review progress (manyforge-206)"
```

---

### Task 3: Worker heartbeat + `runJob(prog)` signature + RenewLease + phase markers

**Files:**
- Modify: `internal/agents/coding/worker.go` (systemDB interface, `HeartbeatInterval` field, `applyDefaults`, seam type, `effectiveRunJob`, `tick`, `processOne` heartbeat, `AppDBAdapter.RenewLease`)
- Modify: `internal/agents/coding/service.go:205` (`runJob` signature + `SetSecrets`/`SetPhase` markers)
- Modify: `internal/agents/coding/worker_test.go` (fake `RenewLease` + recording, `makeWorker` seam type, all 6 closures, new heartbeat test)
- Modify: `internal/agents/coding/service_integration_test.go` (6 `runJob` call sites → add `, nil`)
- Modify: `internal/agents/coding/worker_integration_test.go:81` (`runJob` call site → add `, nil`)

**Interfaces:**
- Consumes: `Progress` (Task 2), `renew_code_review_lease` (Task 1).
- Produces: `systemDB.RenewLease(ctx, id uuid.UUID, leaseSeconds int, progress []byte) error`; `runJob(ctx context.Context, job ClaimedReview, prog *Progress) error`; worker field `HeartbeatInterval time.Duration` (default 5s). Consumed by Tasks 5, 6.

- [ ] **Step 1: Write the failing heartbeat unit test**

In `internal/agents/coding/worker_test.go`, add a recording field to `fakeSystemDB` and the new method + test. First extend the struct (add the field):

```go
type fakeSystemDB struct {
	claims []ClaimedReview
	// recorded calls
	requeueCalls []requeueCall
	failCalls    []failCall
	renewCalls   atomic.Int64 // heartbeat renewals (goroutine-concurrent → atomic)
}

func (f *fakeSystemDB) RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error {
	f.renewCalls.Add(1)
	return nil
}
```

Then the new test:

```go
// TestWorkerHeartbeatRenewsLease verifies the worker spawns a heartbeat that calls
// RenewLease while runJob is in flight (the long-running lease-renewal mechanism).
func TestWorkerHeartbeatRenewsLease(t *testing.T) {
	db := &fakeSystemDB{claims: []ClaimedReview{makeRow(1)}}
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, prog *Progress) error {
		prog.SetPhase("reviewing") // makes Snapshot non-nil (heartbeat persists it)
		time.Sleep(80 * time.Millisecond)
		return nil
	})
	w.HeartbeatInterval = 10 * time.Millisecond // fast for the test (default is 5s)
	runOnce(w)

	if db.renewCalls.Load() == 0 {
		t.Fatal("heartbeat never called RenewLease during a runJob in flight")
	}
}
```

- [ ] **Step 2: Run to verify it fails (compile error)**

Run: `go test ./internal/agents/coding/ -run TestWorkerHeartbeat -v`
Expected: FAIL — build error (`RenewLease` not in systemDB; seam signature mismatch). This compile failure is expected; it resolves as the signatures change below.

- [ ] **Step 3: Update worker.go — interface, field, defaults, seam types**

In `internal/agents/coding/worker.go`:

(a) Add to the `systemDB` interface (after `FailCodeReview`):

```go
	// RenewLease renews a running row's lease AND persists its progress snapshot via
	// the renew_code_review_lease SECURITY DEFINER function (migrations/0076). The
	// status='running' guard makes a renew after terminal a harmless no-op; a nil
	// progress leaves the column unchanged-as-NULL.
	RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error
```

(b) Add a field to `CodeReviewWorker` (after `Batch`):

```go
	// HeartbeatInterval is how often a running job's lease is renewed and its progress
	// persisted (default 5s). Must be well under LeaseSeconds so a live job is never
	// re-claimed.
	HeartbeatInterval time.Duration
```

(c) Change the `runJobSeam` field type:

```go
	runJobSeam func(ctx context.Context, job ClaimedReview, prog *Progress) error
```

(d) In `applyDefaults`, add (before the `if w.Logger == nil` block):

```go
	if w.HeartbeatInterval <= 0 {
		w.HeartbeatInterval = 5 * time.Second
	}
```

(e) Change `effectiveRunJob`'s return type and `tick`/`processOne` param types to `func(context.Context, ClaimedReview, *Progress) error`:

```go
func (w *CodeReviewWorker) effectiveRunJob() func(ctx context.Context, job ClaimedReview, prog *Progress) error {
	if w.runJobSeam != nil {
		return w.runJobSeam
	}
	return w.Svc.runJob
}
```

```go
func (w *CodeReviewWorker) tick(ctx context.Context, runJob func(context.Context, ClaimedReview, *Progress) error) {
```

- [ ] **Step 4: Update processOne to run the heartbeat**

Replace the body of `processOne` (keep the requeue/fail tail unchanged). The new signature + heartbeat wrapper:

```go
func (w *CodeReviewWorker) processOne(
	ctx context.Context,
	job ClaimedReview,
	runJob func(context.Context, ClaimedReview, *Progress) error,
) {
	var jobErr error

	// Heartbeat: renew the lease + persist progress every HeartbeatInterval while
	// runJob is in flight, so a job exceeding the base lease is never re-claimed.
	prog := &Progress{}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(w.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if rerr := w.DB.RenewLease(ctx, job.ID, w.LeaseSeconds, prog.Snapshot()); rerr != nil {
					w.Logger.WarnContext(ctx, "code review lease renew failed", "id", job.ID, "err", rerr)
				}
			}
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				jobErr = fmt.Errorf("panic in runJob: %v", r)
				w.Logger.ErrorContext(ctx, "code review runJob panicked",
					"id", job.ID, "attempts", job.Attempts, "panic", r)
			}
		}()
		jobErr = runJob(ctx, job, prog)
	}()
	close(stop)

	if jobErr == nil {
		w.Logger.InfoContext(ctx, "code review job succeeded",
			"id", job.ID, "attempts", job.Attempts)
		return
	}

	// Failure path: requeue or fail terminally.
	if job.Attempts < w.MaxAttempts {
		w.Logger.WarnContext(ctx, "code review job failed; requeueing",
			"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
		if rerr := w.DB.RequeueCodeReview(ctx, job.ID, 30, jobErr.Error()); rerr != nil {
			w.Logger.ErrorContext(ctx, "code review requeue failed", "id", job.ID, "err", rerr)
		}
		return
	}

	w.Logger.ErrorContext(ctx, "code review job exhausted max attempts; failing terminally",
		"id", job.ID, "attempts", job.Attempts, "max_attempts", w.MaxAttempts, "err", jobErr)
	if ferr := w.DB.FailCodeReview(ctx, job.ID, jobErr.Error()); ferr != nil {
		w.Logger.ErrorContext(ctx, "code review fail-terminal write failed", "id", job.ID, "err", ferr)
	}
}
```

- [ ] **Step 5: Add AppDBAdapter.RenewLease**

In `internal/agents/coding/worker.go`, after `FailCodeReview`'s adapter method:

```go
// RenewLease renews a running row's lease and persists its progress snapshot via the
// renew_code_review_lease SECURITY DEFINER function (RLS bypassed by the function
// owner). A nil progress is encoded as SQL NULL (jsonb), leaving the column unchanged.
func (a *AppDBAdapter) RenewLease(ctx context.Context, id uuid.UUID, leaseSeconds int, progress []byte) error {
	return a.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT renew_code_review_lease($1, $2, $3)", id, leaseSeconds, progress)
		return err
	})
}
```

- [ ] **Step 6: Update runJob signature + phase markers in service.go**

In `internal/agents/coding/service.go`, change the `runJob` signature (line 205):

```go
func (s *CodeReviewService) runJob(ctx context.Context, job ClaimedReview, prog *Progress) error {
```

After the credential is resolved and the `cred.Host() == ""` check passes, and after the fallback-model block (i.e. right before `// Fetch PR metadata`), add:

```go
	// Live progress: scrub the resolved secrets from any streamed preview, and mark
	// the first phase. prog is nil for direct (non-worker) callers — all methods no-op.
	prog.SetSecrets(cred.APIKey, rc.Credential.APIToken)
	prog.SetPhase("preparing")
```

Just before the provider branch `if isLocalProvider(cred.Provider) {` (line ~327), add:

```go
	prog.SetPhase("reviewing")
```

Just before `ref, err := conn.PostReview(ctx, prNumber, review)` (line ~408), add:

```go
	prog.SetPhase("posting")
```

(Leave the `localReview(ctx, s.localClient(), cred, payload)` call as-is for now — Task 5 threads `prog` into it.)

- [ ] **Step 7: Update worker_test.go closures + integration call sites**

In `internal/agents/coding/worker_test.go`, change `makeWorker`'s param type and every test closure to the 3-arg seam. `makeWorker`:

```go
func makeWorker(db *fakeSystemDB, runJobFn func(ctx context.Context, job ClaimedReview, prog *Progress) error) *CodeReviewWorker {
```

For each existing test closure passed to `makeWorker` (in `TestWorkerSuccess`, `TestWorkerRequeueOnFailureUnderMax`, `TestWorkerFailOnMaxAttempts`, `TestWorkerCtxCancelStopsLoop`, `TestWorkerPanicRecovery`, `TestWorkerMultipleRowsInBatch`), add the `prog` param (unused):

```go
	w := makeWorker(db, func(ctx context.Context, job ClaimedReview, _ *Progress) error {
		// ... unchanged body ...
	})
```

In `internal/agents/coding/service_integration_test.go`, update all `svc.runJob(ctx, …)` call sites (lines ~349, 386, 483, 587, 714) to pass `nil`:

```go
	svc.runJob(ctx, claimed, nil)   // and the `*found` / loop variants likewise: svc.runJob(ctx, *found, nil)
```

In `internal/agents/coding/worker_integration_test.go:81`:

```go
	if err := svc.runJob(ctx, *found, nil); err != nil {
```

- [ ] **Step 8: Run unit tests + build (incl. integration build)**

Run: `go test ./internal/agents/coding/ -run 'TestWorker|TestProgress' -v`
Expected: PASS, including `TestWorkerHeartbeatRenewsLease` and all pre-existing worker tests.
Run: `go vet ./... && go build ./... && go test -tags integration -p 1 -run xxxNoSuchTest ./internal/agents/coding/`
Expected: integration package COMPILES (the `-run xxx` selects nothing; we only verify it builds with the new `, nil` call sites).

- [ ] **Step 9: Commit**

```bash
git add internal/agents/coding/worker.go internal/agents/coding/service.go \
        internal/agents/coding/worker_test.go internal/agents/coding/service_integration_test.go \
        internal/agents/coding/worker_integration_test.go
git commit -m "feat(007): worker heartbeat renews lease + persists progress (manyforge-206)"
```

---

### Task 4: Longer local-review timeout — config + service + main wiring

**Files:**
- Modify: `internal/platform/config/config.go` (Config field + Load parse)
- Test: `internal/platform/config/config_test.go` (add a focused test)
- Modify: `internal/agents/coding/service.go` (`LocalTimeout` field + `localTimeout()` + `localClient()`)
- Modify: `cmd/manyforge/main.go:386-398` (wire `LocalTimeout: cfg.LocalReviewTimeout`)

**Interfaces:**
- Produces: `config.Config.LocalReviewTimeout time.Duration` (env `MANYFORGE_LOCAL_REVIEW_TIMEOUT`, default 30m); `CodeReviewService.LocalTimeout time.Duration` + `localTimeout()`.

- [ ] **Step 1: Write the failing config test**

Add to `internal/platform/config/config_test.go`:

```go
func TestLocalReviewTimeout(t *testing.T) {
	t.Run("default 30m", func(t *testing.T) {
		t.Setenv("MANYFORGE_LOCAL_REVIEW_TIMEOUT", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LocalReviewTimeout != 30*time.Minute {
			t.Fatalf("default = %v, want 30m", cfg.LocalReviewTimeout)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("MANYFORGE_LOCAL_REVIEW_TIMEOUT", "45m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LocalReviewTimeout != 45*time.Minute {
			t.Fatalf("override = %v, want 45m", cfg.LocalReviewTimeout)
		}
	})
	t.Run("malformed is a hard error", func(t *testing.T) {
		t.Setenv("MANYFORGE_LOCAL_REVIEW_TIMEOUT", "notaduration")
		if _, err := Load(); err == nil {
			t.Fatal("malformed duration must be a config error")
		}
	})
}
```

(If `config_test.go` lacks a `time` import, add it.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/platform/config/ -run TestLocalReviewTimeout -v`
Expected: FAIL — `cfg.LocalReviewTimeout undefined`.

- [ ] **Step 3: Add the config field + parse**

In `internal/platform/config/config.go`, add a field to the Spec 007 section of `Config` (after `SandboxWorkRoot string`):

```go
	// LocalReviewTimeout is the HTTP timeout for the host-side local-provider review
	// path (Ollama/vLLM). Local models can run far longer than the cloud sandbox cap,
	// so this defaults to 30 min. Env: MANYFORGE_LOCAL_REVIEW_TIMEOUT. Separate from
	// the sandbox wall-clock cap.
	LocalReviewTimeout time.Duration
```

In `Load()`, after the `cfg.SandboxWorkRoot = …` line (before `return cfg, nil`):

```go
	if cfg.LocalReviewTimeout, err = envDuration("MANYFORGE_LOCAL_REVIEW_TIMEOUT", 30*time.Minute); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_LOCAL_REVIEW_TIMEOUT: %w", err)
	}
```

- [ ] **Step 4: Run to verify config test passes**

Run: `go test ./internal/platform/config/ -run TestLocalReviewTimeout -v`
Expected: PASS.

- [ ] **Step 5: Add LocalTimeout to the service + use it in localClient**

In `internal/agents/coding/service.go`, add a field to `CodeReviewService` (after `Timeout time.Duration`):

```go
	// LocalTimeout is the HTTP timeout for the host-side local-provider review path
	// (Ollama/vLLM); 0 ⇒ 30 min default. Separate from Timeout (the sandbox cap) so a
	// long local review (e.g. a 35b model) isn't killed at the 10-min sandbox limit.
	LocalTimeout time.Duration
```

Add the accessor (after `timeout()`):

```go
// localTimeout returns the effective host-side local-provider review timeout (30 min
// default). Distinct from timeout(): local models can run far longer than the cloud
// sandbox wall-clock cap.
func (s *CodeReviewService) localTimeout() time.Duration {
	if s.LocalTimeout > 0 {
		return s.LocalTimeout
	}
	return 30 * time.Minute
}
```

Change `localClient()` to use it:

```go
func (s *CodeReviewService) localClient() *http.Client {
	return &http.Client{Timeout: s.localTimeout()}
}
```

- [ ] **Step 6: Wire it in main.go**

In `cmd/manyforge/main.go`, add to the `coding.CodeReviewService{…}` literal (after `Timeout: 5 * time.Minute,`):

```go
			Timeout:      5 * time.Minute,
			LocalTimeout: cfg.LocalReviewTimeout,
```

- [ ] **Step 7: Build + verify**

Run: `go build ./... && go vet ./... && go test ./internal/agents/coding/ ./internal/platform/config/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/platform/config/config.go internal/platform/config/config_test.go \
        internal/agents/coding/service.go cmd/manyforge/main.go
git commit -m "feat(007): 30-min local-review timeout (MANYFORGE_LOCAL_REVIEW_TIMEOUT) (manyforge-206)"
```

---

### Task 5: Streaming localReview + progress updates

**Files:**
- Modify: `internal/agents/coding/localreview.go` (`localReview` signature + SSE streaming)
- Modify: `internal/agents/coding/service.go:333` (pass `prog` into `localReview`)
- Modify: `internal/agents/coding/localreview_test.go` (SSE mock for `TestLocalReview`, `nil` arg for `RejectsNonLoopback`, new streaming test)

**Interfaces:**
- Consumes: `Progress` (Task 2).
- Produces: `localReview(ctx, client *http.Client, cred AICredential, payload string, prog *Progress) (FindingsDoc, int64, int64, error)` — reads an SSE stream, accumulates `delta.content`, throttled `prog.UpdateStream`, parses on `[DONE]`.

- [ ] **Step 1: Update the existing tests to the streaming shape (write failing tests)**

In `internal/agents/coding/localreview_test.go`, rewrite `TestLocalReview` to drive an SSE mock and add the `prog` arg, update `TestLocalReview_RejectsNonLoopback`'s call, and add a streaming-progress test:

```go
func TestLocalReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		content := `{"summary":"ok","findings":[{"file":"service.go","line":3,"severity":" Warning ","title":"t","detail":"d"}]}`
		writeFrame := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		// Stream the content split across two delta frames.
		writeFrame(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content[:25]}}}})
		writeFrame(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content[25:]}}}})
		// Terminal usage frame (only sent because stream_options.include_usage=true).
		writeFrame(map[string]any{"choices": []map[string]any{}, "usage": map[string]int64{"prompt_tokens": 1200, "completion_tokens": 80}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "qwen2.5-coder:14b", Provider: "ollama", APIKey: "ollama"}
	payload := "\n=== service.go ===\n@@ 1-1 @@\n    1 + package x\n"
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, payload, nil)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if doc.Summary != "ok" || len(doc.Findings) != 1 || doc.Findings[0].Severity != "warning" {
		t.Fatalf("doc=%+v", doc)
	}
	if in != 1200 || out != 80 {
		t.Fatalf("tokens in=%d out=%d, want 1200/80", in, out)
	}
}

func TestLocalReview_RejectsNonLoopback(t *testing.T) {
	cred := AICredential{BaseURL: "https://evil.example.com/v1", Model: "m", Provider: "ollama", APIKey: "k"}
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", nil); err == nil {
		t.Fatal("local review must reject a non-loopback base URL (SSRF guard)")
	}
}

func TestLocalReview_StreamUpdatesProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		content := `{"summary":"streamed","findings":[]}`
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content}}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "m", Provider: "ollama", APIKey: "ollama"}
	prog := &Progress{}
	prog.SetPhase("reviewing") // worker sets this in prod; needed so Snapshot is non-nil
	doc, _, out, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", prog)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if doc.Summary != "streamed" {
		t.Fatalf("doc=%+v", doc)
	}
	if out == 0 {
		t.Fatal("completion tokens must fall back to streamed-chunk count when usage absent")
	}
	snap := prog.Snapshot()
	if snap == nil {
		t.Fatal("expected progress snapshot after streaming")
	}
	var s progressSnapshot
	_ = json.Unmarshal(snap, &s)
	if !strings.Contains(s.Preview, "streamed") {
		t.Fatalf("preview missing streamed content: %q", s.Preview)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/coding/ -run TestLocalReview -v`
Expected: FAIL — too many arguments to `localReview` (still 4-arg) / non-streaming.

- [ ] **Step 3: Rewrite localReview to stream**

In `internal/agents/coding/localreview.go`, update imports — remove `io`, add `bufio` and `time`:

```go
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/platform/errs"
)
```

Replace the entire `localReview` function:

```go
// localReview POSTs the rendered diff payload to a local OpenAI-compatible chat
// endpoint (Ollama/vLLM) as a STREAM and parses the findings with ParseFindings. It
// accumulates the streamed delta.content into a buffer (rendered live in the UI via
// the worker heartbeat → prog.UpdateStream) and parses the full buffer on [DONE]. No
// sandbox/opencode: small local models can't drive opencode's agent loop, and the
// model is on-host so there is nothing to isolate. The model gets NO tools (chat→JSON
// only), so prompt injection can at worst yield bogus advisory findings.
// Returns (doc, promptTokens, completionTokens, err). prog may be nil (no-op).
func localReview(ctx context.Context, client *http.Client, cred AICredential, payload string, prog *Progress) (FindingsDoc, int64, int64, error) {
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
		"stream": true,
		// In OpenAI-compatible streaming, usage is omitted unless explicitly requested;
		// without this, token accounting silently goes to 0.
		"stream_options": map[string]any{"include_usage": true},
		"options":        map[string]any{"temperature": 0, "num_ctx": localReviewNumCtx},
	})

	url := strings.TrimRight(cred.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: build local review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if cred.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local review request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local provider status %d", resp.StatusCode)
	}

	var (
		buf              strings.Builder
		promptTokens     int64
		completionTokens int64
		chunkCount       int
		lastUpdate       time.Time
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // SSE frames can be large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			continue // tolerate keep-alive / non-JSON frames
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			buf.WriteString(chunk.Choices[0].Delta.Content)
			chunkCount++
			// Throttle progress writes to ~2/s — redaction + marshal happen on the
			// heartbeat's Snapshot(); updating the shared buffer per token is wasteful.
			if time.Since(lastUpdate) > 500*time.Millisecond {
				prog.UpdateStream(chunkCount, buf.String())
				lastUpdate = time.Now()
			}
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
		}
	}
	if serr := sc.Err(); serr != nil {
		return FindingsDoc{}, promptTokens, completionTokens, fmt.Errorf("coding: read local review stream: %w", serr)
	}
	prog.UpdateStream(chunkCount, buf.String()) // final flush with the full buffer

	if completionTokens == 0 { // usage frame absent → best-effort fallback
		completionTokens = int64(chunkCount)
	}
	doc, perr := ParseFindings([]byte(buf.String()))
	return doc, promptTokens, completionTokens, perr
}
```

- [ ] **Step 4: Thread prog into the call site**

In `internal/agents/coding/service.go`, update the local-path call (line ~333):

```go
		d, in, out, lerr := localReview(ctx, s.localClient(), cred, payload, prog)
```

- [ ] **Step 5: Run the localReview tests**

Run: `go test ./internal/agents/coding/ -run TestLocalReview -v`
Expected: PASS (3 tests). Then `go build ./... && go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/localreview.go internal/agents/coding/localreview_test.go internal/agents/coding/service.go
git commit -m "feat(007): stream local review output for live progress (manyforge-206)"
```

---

### Task 6: Integration test — lease renewal prevents re-claim + progress persisted

**Files:**
- Create: `internal/agents/coding/worker_lease_integration_test.go` (`//go:build integration`, package `coding`)

**Interfaces:**
- Consumes: integration helpers from `service_integration_test.go`/`worker_integration_test.go` (`startCoding`, `newCodingEnv`, `createRepoConnector`, `buildService`, `FakeCredResolver`, `readStatus`); the worker (`CodeReviewWorker`, `AppDBAdapter`), `RenewLease` (Task 3), migration 0076 (Task 1).

- [ ] **Step 1: Write the integration test**

Create `internal/agents/coding/worker_lease_integration_test.go`:

```go
//go:build integration

package coding

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestCodeReviewLeaseRenewalPreventsReclaim is the load-bearing test for the
// long-running-review fix (manyforge-206). It drives the real CodeReviewWorker over a
// blocking runJob seam and asserts the heartbeat (1) advances lease_expires_at while
// the job is in flight, (2) persists a non-null progress snapshot, and (3) keeps a
// concurrent ClaimCodeReviews from re-claiming the still-running row — the exact
// double-claim the lease renewal exists to prevent. Then it releases the job and
// asserts it finalizes to succeeded.
func TestCodeReviewLeaseRenewalPreventsReclaim(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic",
	}}
	svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A runJob seam that signals start, blocks until released, then finalizes the row
	// (mimicking the real runJob's success path) so the post-release assertion holds.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	seam := func(ctx context.Context, job ClaimedReview, prog *Progress) error {
		prog.SetPhase("reviewing")
		prog.UpdateStream(3, "partial streamed review output")
		once.Do(func() { close(started) })
		<-release
		_, _ = tdb.Super.Exec(ctx,
			`UPDATE code_review SET status='succeeded', lease_expires_at=NULL, updated_at=now() WHERE id=$1`, job.ID)
		return nil
	}

	w := &CodeReviewWorker{
		DB:                &AppDBAdapter{DB: tdb.App},
		Logger:            slog.Default(),
		Poll:              10 * time.Millisecond,
		LeaseSeconds:      2, // short so renewal is observable
		HeartbeatInterval: 100 * time.Millisecond,
		MaxAttempts:       3,
		Batch:             2,
	}
	w.runJobSeam = seam
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go w.Run(wctx)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runJob seam never started — worker did not claim the row")
	}

	// (1) The heartbeat advances the lease.
	lease1 := readLeaseExpires(ctx, t, tdb, cr.ID)
	time.Sleep(600 * time.Millisecond) // ~6 heartbeats at 100ms
	lease2 := readLeaseExpires(ctx, t, tdb, cr.ID)
	if !lease2.After(lease1) {
		t.Fatalf("lease not renewed: lease1=%v lease2=%v — heartbeat is not advancing lease_expires_at", lease1, lease2)
	}

	// (2) Progress is persisted mid-run.
	if !progressNonNull(ctx, t, tdb, cr.ID) {
		t.Fatal("progress is NULL mid-run — heartbeat did not persist the snapshot")
	}

	// (3) THE FIX: a concurrent claim must NOT re-claim the running row (fresh lease).
	again, err := (&AppDBAdapter{DB: tdb.App}).ClaimCodeReviews(ctx, 900, 10)
	if err != nil {
		t.Fatalf("concurrent claim: %v", err)
	}
	for _, r := range again {
		if r.ID == cr.ID {
			t.Fatal("running row with a fresh lease was re-claimed — lease renewal failed to prevent the double-claim")
		}
	}

	// Release; the row finalizes and stays succeeded (post-terminal renew no-ops).
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if readStatus(ctx, t, tdb, cr.ID) == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row did not reach succeeded after release; status=%s", readStatus(ctx, t, tdb, cr.ID))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readLeaseExpires(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) time.Time {
	t.Helper()
	var ts pgtype.Timestamptz
	if err := tdb.Super.QueryRow(ctx, `SELECT lease_expires_at FROM code_review WHERE id=$1`, id).Scan(&ts); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

func progressNonNull(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) bool {
	t.Helper()
	var nonNull bool
	if err := tdb.Super.QueryRow(ctx, `SELECT progress IS NOT NULL FROM code_review WHERE id=$1`, id).Scan(&nonNull); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return nonNull
}
```

- [ ] **Step 2: Run the integration test**

Run: `go test -tags integration -p 1 -run TestCodeReviewLeaseRenewalPreventsReclaim ./internal/agents/coding/ -v`
Expected: PASS. (Requires Docker/testdb; the dev stack is up.)

- [ ] **Step 3: Run the full coding integration suite (no regressions)**

Run: `go test -tags integration -p 1 ./internal/agents/coding/`
Expected: PASS (existing claim/requeue/fail + new lease-renewal test).

- [ ] **Step 4: Commit**

```bash
git add internal/agents/coding/worker_lease_integration_test.go
git commit -m "test(007): integration — lease renewal prevents re-claim, persists progress (manyforge-206)"
```

---

### Task 7: API surface — CodeReview.Progress populated in Get/List

**Files:**
- Modify: `internal/agents/coding/service.go` (CodeReview struct field + populate in `Get` and `List`)
- Test: `internal/agents/coding/progress_json_test.go` (marshalling)
- Modify: the code-review response schema in the OpenAPI contract (if present) + run the contract gate

**Interfaces:**
- Consumes: `dbgen` rows' `Progress []byte` (Task 1).
- Produces: `CodeReview.Progress json.RawMessage` (`json:"progress,omitempty"`) on the API response.

- [ ] **Step 1: Write the failing marshalling test**

Create `internal/agents/coding/progress_json_test.go`:

```go
package coding

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodeReviewProgressJSON(t *testing.T) {
	withProg := CodeReview{Progress: json.RawMessage(`{"phase":"reviewing","tokens":12,"preview":"x"}`)}
	b, _ := json.Marshal(withProg)
	if !strings.Contains(string(b), `"progress":{"phase":"reviewing"`) {
		t.Fatalf("progress not marshalled into the response: %s", b)
	}
	without := CodeReview{}
	b2, _ := json.Marshal(without)
	if strings.Contains(string(b2), "progress") {
		t.Fatalf("empty progress must be omitted (omitempty): %s", b2)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/coding/ -run TestCodeReviewProgressJSON -v`
Expected: FAIL — `unknown field Progress in struct literal`.

- [ ] **Step 3: Add the struct field**

In `internal/agents/coding/service.go`, add to `CodeReview` (after `PostedAt *time.Time`):

```go
	// Progress is the live progress snapshot for a running review (phase/tokens/
	// preview); null/omitted for pending and terminal reviews. Populated from the
	// code_review.progress jsonb the worker heartbeat persists.
	Progress json.RawMessage `json:"progress,omitempty"`
```

- [ ] **Step 4: Populate it in Get and List**

In `Get`, in the `raw.cr = CodeReview{…}` literal, add:

```go
			PostedAt:      postedAt,
			Progress:      json.RawMessage(row.Progress),
```

In `List`, in the `out = append(out, CodeReview{…})` literal, add:

```go
				PostedAt:      postedAt,
				Progress:      json.RawMessage(r.Progress),
```

(Both `row.Progress`/`r.Progress` are `[]byte` from the regenerated dbgen rows; a NULL column → nil → omitted.)

- [ ] **Step 5: Run the marshalling test + the secret-projection pin**

Run: `go test ./internal/agents/coding/ -run TestCodeReviewProgressJSON -v`
Expected: PASS.
Run: `go test ./internal/security_regression/ -run TestNoSecretProjection -v`
Expected: PASS (MF007-PIN-8 — `Progress` contains no banned `Token`/`Secret`/`APIKey` substring).

- [ ] **Step 6: Update the OpenAPI contract + run the contract gate**

Find the code-review response schema:
Run: `grep -rln "code-reviews\|CodeReview" --include=openapi.yaml .`
If a `CodeReview` response schema exists, add an optional `progress` object property (`phase` string, `tokens` integer, `preview` string) so the schema permits the new field. Then:
Run: `go test -tags contract ./cmd/...`
Expected: PASS. (If no code-review schema is documented, note that in the commit message and the gate passes unchanged. See [[backend-verification-gates-easy-to-miss]].)

- [ ] **Step 7: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/progress_json_test.go
# include the openapi file if you edited it
git commit -m "feat(007): expose review progress on the code-review API (manyforge-206)"
```

---

### Task 8: Frontend — live progress in the detail page + phase label in the list

**Files:**
- Modify: `web/src/app/core/code-review.service.ts` (TS `CodeReview.progress` type)
- Modify: `web/src/app/pages/code-review/detail.ts` (progress block: phase + elapsed timer + preview)
- Modify: `web/src/app/pages/code-review/detail.spec.ts` (3 new tests)
- Modify: `web/src/app/pages/code-review/list.ts` (phase label on running rows)

**Interfaces:**
- Consumes: API `progress` field (Task 7).
- Produces: rendered progress UI; `elapsed` signal on the detail component.

- [ ] **Step 1: Add the TS type**

In `web/src/app/core/code-review.service.ts`, add to the `CodeReview` interface (after `posted_at`):

```ts
  posted_at: string | null;
  progress?: { phase: string; tokens: number; preview: string };
```

- [ ] **Step 2: Write the failing detail tests**

In `web/src/app/pages/code-review/detail.spec.ts`, add inside the `describe`:

```ts
  it('renders the progress block (phase + preview) while running', () => {
    mount(makeReview({ status: 'running', progress: { phase: 'reviewing', tokens: 12, preview: 'partial output here' } }));
    expect(q('[data-testid="review-progress"]')).toBeTruthy();
    expect(q('[data-testid="progress-phase"]')?.textContent).toContain('Reviewing');
    expect(q('[data-testid="progress-preview"]')?.textContent).toContain('partial output here');
    // drain the poll the running status scheduled (so mock.verify() is clean)
    vi.advanceTimersByTime(3000);
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(makeReview({ status: 'succeeded' }));
  });

  it('hides the progress block when terminal', () => {
    mount(makeReview({ status: 'succeeded', progress: { phase: 'posting', tokens: 1, preview: 'x' } }));
    expect(q('[data-testid="review-progress"]')).toBeNull();
  });

  it('ticks the elapsed timer while running', () => {
    mount(makeReview({ status: 'running', progress: { phase: 'reviewing', tokens: 0, preview: '' } }));
    const before = cmp.elapsed();
    vi.advanceTimersByTime(3000);
    expect(cmp.elapsed()).toBeGreaterThan(before);
    // the 3s advance also fired a poll — drain it
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(makeReview({ status: 'succeeded' }));
  });
```

- [ ] **Step 3: Run to verify they fail**

Run: `cd web && npx vitest run src/app/pages/code-review/detail.spec.ts`
Expected: FAIL — `review-progress` not found / `cmp.elapsed is not a function`.

- [ ] **Step 4: Implement the detail progress UI**

In `web/src/app/pages/code-review/detail.ts`:

(a) Add the progress block to the template, immediately after the closing `</div>` of the header block (after the `}` that closes `@if (r.review_url)`'s parent `<div …flex-wrap:wrap">`, i.e. before the `<!-- Summary -->` comment):

```html
        <!-- Live progress (running only) -->
        @if (r.status === 'running' && r.progress) {
          <div class="mf-card" style="margin-bottom:16px" data-testid="review-progress">
            <div style="display:flex;gap:12px;align-items:center;margin-bottom:8px">
              <span style="font-weight:600" data-testid="progress-phase">{{ phaseLabel(r) }}</span>
              <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="progress-elapsed">{{ elapsedLabel() }}</span>
              @if (r.progress.tokens) {
                <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ r.progress.tokens }} tokens</span>
              }
            </div>
            @if (r.progress.preview) {
              <pre data-testid="progress-preview"
                   style="max-height:240px;overflow:auto;white-space:pre-wrap;font-family:monospace;font-size:var(--mf-fs-xs);margin:0;padding:8px;border-radius:6px;background:var(--mf-bg-subtle,rgba(0,0,0,.05))">{{ r.progress.preview }}</pre>
            }
          </div>
        }
```

(b) Add the `elapsed` signal + helpers + timer to the component class. After `error = signal('');`:

```ts
  elapsed = signal(0);
```

After `private pollTimer: ReturnType<typeof setInterval> | undefined;`:

```ts
  private elapsedTimer: ReturnType<typeof setInterval> | undefined;
```

Add the label helpers (near `findingTone`):

```ts
  // Maps a progress phase to a human label (reviewing names the model).
  phaseLabel(r: CodeReview): string {
    const phase = r.progress?.phase ?? 'working';
    const map: Record<string, string> = {
      preparing: 'Preparing',
      reviewing: 'Reviewing with ' + (r.model || 'model'),
      posting: 'Posting review',
    };
    return map[phase] ?? phase;
  }

  // Formats the running-review elapsed seconds as "Ns" or "Mm Ns".
  elapsedLabel(): string {
    const s = this.elapsed();
    const m = Math.floor(s / 60);
    const sec = s % 60;
    return m > 0 ? `${m}m ${sec}s` : `${sec}s`;
  }

  private startElapsed(createdAt: string): void {
    if (this.elapsedTimer !== undefined) return;
    const start = new Date(createdAt).getTime();
    const tick = () => this.elapsed.set(Math.max(0, Math.floor((Date.now() - start) / 1000)));
    tick();
    this.elapsedTimer = setInterval(tick, 1000);
  }

  private stopElapsed(): void {
    if (this.elapsedTimer !== undefined) {
      clearInterval(this.elapsedTimer);
      this.elapsedTimer = undefined;
    }
  }
```

(c) Wire the timer into the lifecycle. In `stopPolling()`, also stop elapsed:

```ts
  private stopPolling(): void {
    if (this.pollTimer !== undefined) {
      clearInterval(this.pollTimer);
      this.pollTimer = undefined;
    }
    this.stopElapsed();
  }
```

In `load()`'s success branch, where it currently calls `if (!this.isTerminal(r)) this.startPolling();`, also start elapsed:

```ts
      next: (r) => {
        this.review.set(r);
        this.loading.set(false);
        if (!this.isTerminal(r)) {
          this.startPolling();
          this.startElapsed(r.created_at);
        }
      },
```

In `pollOnce()`'s success branch, start elapsed if still running (covers a pending→running transition):

```ts
      next: (r) => {
        this.review.set(r);
        if (this.isTerminal(r)) {
          this.stopPolling();
        } else {
          this.startElapsed(r.created_at);
        }
      },
```

- [ ] **Step 5: Run the detail tests**

Run: `cd web && npx vitest run src/app/pages/code-review/detail.spec.ts`
Expected: PASS (existing + 3 new).

- [ ] **Step 6: Add the list phase label + a test**

In `web/src/app/pages/code-review/list.ts`, in the reviews-table row's status cell, add the phase label after the `<mf-status-pill …/>`:

```html
              <span style="width:96px">
                <mf-status-pill [tone]="reviewTone(r.status)" [label]="r.status" />
                @if (r.status === 'running' && r.progress?.phase) {
                  <span data-testid="review-phase" style="display:block;color:var(--mf-text-muted);font-size:var(--mf-fs-xs);margin-top:2px">{{ r.progress?.phase }}</span>
                }
              </span>
```

Add a test to `web/src/app/pages/code-review/list.spec.ts` (slot into its existing `describe`, mirroring how it mounts + flushes the connectors/reviews/agents init loads — copy a sibling test's mount/flush sequence and assert):

```ts
  it('shows the progress phase on a running review row', () => {
    // mount + flush the three init loads as the sibling tests do, with one running
    // review carrying a progress phase, then:
    expect(q('[data-testid="review-phase"]')?.textContent).toContain('reviewing');
  });
```

(Use the existing list.spec.ts mount helper/flush pattern for the init loads; the assertion above is the new part.)

- [ ] **Step 7: Run the full frontend suite**

Run: `cd web && npm test`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add web/src/app/core/code-review.service.ts web/src/app/pages/code-review/detail.ts \
        web/src/app/pages/code-review/detail.spec.ts web/src/app/pages/code-review/list.ts \
        web/src/app/pages/code-review/list.spec.ts
git commit -m "feat(007): live review progress (phase/elapsed/preview) in the UI (manyforge-206)"
```

---

## Self-Review

**1. Spec coverage** (design doc § → task):
- A. migration 0076 (column + renew DEFINER) → Task 1 ✓
- A. schema.sql mirror (column only) → Task 1 ✓
- A. ListCodeReviews `progress` → Task 1 ✓; renew via raw `tx.Exec` (no sqlc query) → Task 3 (`AppDBAdapter.RenewLease`) ✓
- B. `Progress` holder (SetPhase/SetSecrets/UpdateStream/Snapshot, nil-safe, tail+redact) → Task 2 ✓
- C. worker heartbeat goroutine + `RenewLease` adapter + systemDB method + `heartbeatInterval` → Task 3 ✓
- D. `runJob(…, prog)` + phase markers (preparing/reviewing/posting) + `SetSecrets` → Task 3 ✓; streaming `localReview` w/ `stream:true` + `stream_options.include_usage` + SSE accumulate + throttled `UpdateStream` + parse on `[DONE]` + usage fallback → Task 5 ✓; SSRF loopback guard kept (`TestLocalReview_RejectsNonLoopback`) ✓
- E. `MANYFORGE_LOCAL_REVIEW_TIMEOUT` + `s.localTimeout()` + `localClient()` uses it; cloud `timeout()` unchanged → Task 4 ✓
- F. `CodeReview.Progress` populated in Get/List + TS type + detail.ts (phase/elapsed/preview) + list.ts phase label → Tasks 7, 8 ✓
- G. MF007-PIN-12 → Task 1 ✓
- Test plan: progress_test ✓ (T2), localreview SSE tests ✓ (T5), integration lease/progress/no-reclaim ✓ (T6), MF007-PIN-12 ✓ (T1), detail.spec ✓ (T8). All gates in Global Constraints.
- Edge cases: post-terminal renew no-op (`status='running'` guard in 0076, asserted in T6) ✓; prompt-eval empty preview (elapsed+phase carry liveness) ✓; cloud path unchanged but lease still renewed (heartbeat is provider-agnostic in processOne) ✓; preview redaction before persist (Snapshot redacts; SetSecrets in runJob) ✓.

**2. Placeholder scan:** No TBD/“add error handling”/“write tests for the above”/“similar to Task N”. Every code step shows real code. The two soft spots — the OpenAPI edit (Task 7 Step 6) and the list.spec test (Task 8 Step 6) — are gated by an exact command (`grep`, `npm test`) and the surrounding pattern is specified; the implementer adapts to the existing file’s shape, which is the correct call since those files weren’t read verbatim.

**3. Type consistency:** `runJob(ctx, ClaimedReview, *Progress) error` is used identically in worker.go (seam/effectiveRunJob/tick/processOne), service.go (def), and all test call sites (`, nil`). `RenewLease(ctx, uuid.UUID, int, []byte) error` matches across systemDB, AppDBAdapter, and fakeSystemDB. `localReview(ctx, *http.Client, AICredential, string, *Progress) (FindingsDoc, int64, int64, error)` matches def + 3 test calls + the service call site. `progressSnapshot{Phase,Tokens,Preview}` JSON keys (`phase`/`tokens`/`preview`) match the TS `progress?: {phase, tokens, preview}` and the API `Progress json.RawMessage`. `Progress.Snapshot() []byte` → `RenewLease(..., progress []byte)` → jsonb param: consistent.

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-06-30-review-progress-lease-renewal.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
