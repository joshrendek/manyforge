# Code Review Backend — Durable Async + List/Delete/Get API — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make code-review execution durable + asynchronous (table-as-queue worker) and add the read/list/delete endpoints the UI needs.

**Architecture:** The `code_review` row *is* the work queue — claim/lease columns added to it. `POST /code-reviews` validates cheaply and inserts a `pending` row (202); a background worker claims `pending` rows with `FOR UPDATE SKIP LOCKED`, runs the existing clone→sandbox→post pipeline under the row's principal, and transitions `pending→running→succeeded/failed` with bounded retry. New list/delete/get endpoints reuse the existing permission gates and RLS.

**Tech Stack:** Go (`internal/` layout), pgx v5, sqlc (pinned **v1.27.0 bottle**), chi router, PostgreSQL RLS, testcontainers integration tests.

**Spec:** `docs/superpowers/specs/2026-06-21-code-review-ui-design.md` (§3–§5, §7, §8). **Issue:** `manyforge-elo` (epic `manyforge-7ml`).

## Global Constraints

- sqlc generate MUST use the pinned bottle: `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (global v1.31.1 re-churns generated code). Never hand-edit `internal/platform/db/dbgen/*`.
- Every service method takes `principalID, businessID uuid.UUID` and runs DB work inside `s.DB.WithPrincipal(ctx, principalID, fn)` for RLS. Foreign/unknown id → `errs.ErrNotFound` (no 403/404 oracle).
- Handlers stay thin: parse → call service → map typed `errs` sentinels (`ErrNotFound`→404, `ErrValidation`→400, else→500 generic). Never echo `err.Error()` except typed validation.
- List/Get responses MUST NOT project `secret_ref` or any credential/token field.
- New routes MUST be added to BOTH `specs/007-coding-review-agents/contracts/openapi.yaml` AND `inScope007Ops` in `cmd/manyforge/drift_007_test.go`, or the contract drift test fails. `is007Op` matches any path containing `/repo-connectors` or `/code-reviews`.
- Gates that MUST stay green: `make test`, `make lint`, `go test -tags contract ./cmd/...`, `make sec-test`, integration `go test -tags integration -p 1 ./internal/agents/coding/...` and `./internal/connectors/...`.
- Set `export PATH="$HOME/go/bin:$PATH"` before Go commands.

## File Structure

- Create: `migrations/0072_code_review_queue.up.sql`, `.down.sql` — ALTER `code_review` into the queue.
- Modify: `db/query/repo_connector.sql` — add `ListRepoConnectors`, `DeleteRepoConnector`.
- Modify: `db/query/code_review.sql` — add `ListCodeReviews`, `ClaimCodeReviews`, `RequeueCodeReview`, `FailCodeReview`; update `InsertCodeReview`, `UpdateCodeReviewResult`.
- Regenerate: `internal/platform/db/dbgen/*` (`make generate`).
- Modify: `internal/connectors/repo_service.go` — `List`, `Delete`. (Read its existing `Create`/`Resolve` first for the patterns.)
- Modify: `internal/agents/coding/service.go` — split `Trigger`→`Enqueue`+`runJob`; add `List`; extend read model (`findings`, `review_url`).
- Create: `internal/agents/coding/worker.go` — `CodeReviewWorker`.
- Modify: `internal/agents/coding/handler.go` — new routes; `triggerReview`→`Enqueue`.
- Modify: `cmd/manyforge/main.go` — mount routes, start worker, boot reconcile.
- Modify: `specs/007-coding-review-agents/contracts/openapi.yaml`, `cmd/manyforge/drift_007_test.go`.
- Modify: `internal/security_regression/coding_review_pins_test.go` — no-secret-projection pin.
- Tests: `internal/connectors/repo_service_integration_test.go` (List/Delete RLS), `internal/agents/coding/service_test.go` (Enqueue/List unit), `internal/agents/coding/worker_test.go` (claim/transition unit), `internal/agents/coding/service_integration_test.go` (async e2e).

---

### Task 1: Schema — turn `code_review` into the work queue

**Files:**
- Create: `migrations/0072_code_review_queue.up.sql`
- Create: `migrations/0072_code_review_queue.down.sql`

**Interfaces:**
- Produces: new `code_review` columns `principal_id uuid`, `agent_id uuid`, `attempts int`, `run_after timestamptz`, `lease_expires_at timestamptz`, `last_error text`; index `code_review_claim_idx`.

- [ ] **Step 1: Read the existing migration for conventions**

Read `migrations/0071_code_review.up.sql` (column style, RLS, ownership of `code_review`). Confirm the migration runner numbering (next is `0072`).

- [ ] **Step 2: Write the up migration**

Create `migrations/0072_code_review_queue.up.sql`:
```sql
-- Spec 007 slice 2 (manyforge-elo): turn code_review into a durable work queue.
-- The row IS the queue item; a background worker claims pending rows and runs the
-- review pipeline. Added columns are the claim/lease/retry bookkeeping plus the
-- principal/agent the worker needs to resolve secrets under the right RLS context.
ALTER TABLE code_review
  ADD COLUMN principal_id     uuid,
  ADD COLUMN agent_id         uuid,
  ADD COLUMN attempts         integer NOT NULL DEFAULT 0,
  ADD COLUMN run_after        timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN lease_expires_at timestamptz,
  ADD COLUMN last_error       text NOT NULL DEFAULT '';

-- Claim scan predicate: WHERE (status,'run_after') / expired lease. Partial-friendly.
CREATE INDEX code_review_claim_idx ON code_review (status, run_after);
```
`principal_id`/`agent_id` are nullable so the migration applies over existing terminal slice-1 rows; the service sets them on every new row.

- [ ] **Step 3: Write the down migration**

Create `migrations/0072_code_review_queue.down.sql`:
```sql
DROP INDEX IF EXISTS code_review_claim_idx;
ALTER TABLE code_review
  DROP COLUMN IF EXISTS principal_id,
  DROP COLUMN IF EXISTS agent_id,
  DROP COLUMN IF EXISTS attempts,
  DROP COLUMN IF EXISTS run_after,
  DROP COLUMN IF EXISTS lease_expires_at,
  DROP COLUMN IF EXISTS last_error;
```

- [ ] **Step 4: Apply + verify against the dev DB**

Run: `make migrate` (or the project's migrate target; confirm from Makefile).
Expected: migration 0072 applies cleanly; `\d code_review` shows the new columns. Verify with:
`psql "$DSN" -c "\d code_review"` (DSN = `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable`).

- [ ] **Step 5: Commit**

```bash
git add migrations/0072_code_review_queue.up.sql migrations/0072_code_review_queue.down.sql
git commit -m "feat(007): code_review queue columns (manyforge-elo)"
```

---

### Task 2: sqlc queries — connectors list/delete, reviews list/claim/finalize

**Files:**
- Modify: `db/query/repo_connector.sql`
- Modify: `db/query/code_review.sql`
- Regenerate: `internal/platform/db/dbgen/*`

**Interfaces:**
- Produces (sqlc names methods after the query name):
  - `ListRepoConnectors(ctx, businessID) ([]RepoConnector, error)`
  - `DeleteRepoConnector(ctx, DeleteRepoConnectorParams{ID, BusinessID}) (int64?, error)` — use `:execrows`
  - `ListCodeReviews(ctx, businessID) ([]CodeReview, error)`
  - `ClaimCodeReviews(ctx, ClaimCodeReviewsParams{LeaseSeconds, Limit}) ([]CodeReview, error)`
  - `RequeueCodeReview(ctx, RequeueCodeReviewParams{ID, RunAfterSeconds, LastError})`
  - `FailCodeReview(ctx, FailCodeReviewParams{ID, LastError})`
  - `InsertCodeReview` gains `PrincipalID, AgentID`; `UpdateCodeReviewResult` clears `lease_expires_at`.

- [ ] **Step 1: Read existing queries**

Read `db/query/repo_connector.sql` and `db/query/code_review.sql` fully — copy the exact column lists, RLS-relevant predicates, and the `InsertCodeReview`/`UpdateCodeReviewResult` shapes.

- [ ] **Step 2: Add repo-connector list + delete**

Append to `db/query/repo_connector.sql`:
```sql
-- ListRepoConnectors returns the caller's connectors (RLS-scoped). NEVER selects
-- secret_ref — the UI must not receive any credential handle.
-- name: ListRepoConnectors :many
SELECT id, business_id, type, display_name, base_url, repo, allow_private_base_url, status, created_at
FROM repo_connector
WHERE business_id = $1
ORDER BY created_at DESC;

-- DeleteRepoConnector removes one connector scoped to the business (RLS + explicit
-- predicate). Cascades to its code_review rows via the existing FK.
-- name: DeleteRepoConnector :execrows
DELETE FROM repo_connector WHERE id = $1 AND business_id = $2;
```

- [ ] **Step 3: Add code-review list**

Append to `db/query/code_review.sql`:
```sql
-- ListCodeReviews returns the business's reviews newest-first for the history UI.
-- name: ListCodeReviews :many
SELECT id, repo_connector_id, pr_number, status, summary, findings,
       external_review_ref, created_at, posted_at
FROM code_review
WHERE business_id = $1
ORDER BY created_at DESC
LIMIT 200;
```

- [ ] **Step 4: Add the queue claim + finalize queries**

Append to `db/query/code_review.sql`:
```sql
-- ClaimCodeReviews atomically leases up to $2 runnable rows ACROSS tenants (system
-- path; the worker is a system process). Runnable = pending past run_after OR a
-- running row whose lease expired (crash recovery). FOR UPDATE SKIP LOCKED lets
-- multiple workers claim disjoint rows.
-- name: ClaimCodeReviews :many
UPDATE code_review SET
  status = 'running',
  attempts = attempts + 1,
  lease_expires_at = now() + make_interval(secs => $1::int),
  updated_at = now()
WHERE id IN (
  SELECT id FROM code_review
  WHERE (status = 'pending' AND run_after <= now())
     OR (status = 'running' AND lease_expires_at < now())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT $2::int
)
RETURNING id, business_id, principal_id, agent_id, repo_connector_id, pr_number, attempts;

-- RequeueCodeReview returns a row to pending after a retriable failure.
-- name: RequeueCodeReview :exec
UPDATE code_review SET
  status = 'pending',
  run_after = now() + make_interval(secs => $2::int),
  lease_expires_at = NULL,
  last_error = $3,
  updated_at = now()
WHERE id = $1;

-- FailCodeReview marks a row terminally failed (max attempts exhausted).
-- name: FailCodeReview :exec
UPDATE code_review SET
  status = 'failed',
  lease_expires_at = NULL,
  last_error = $2,
  updated_at = now()
WHERE id = $1;
```

- [ ] **Step 5: Update Insert + Update to carry the new columns**

In `db/query/code_review.sql`, modify `InsertCodeReview` to also set `principal_id`, `agent_id` (and rely on `run_after`/`attempts` defaults). Modify `UpdateCodeReviewResult` to also set `lease_expires_at = NULL` on success. Keep the existing parameter order stable where possible; if you add params, update callers in Task 5/6. Show the edited statements in your diff.

- [ ] **Step 6: Generate + build**

Run: `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` then `go build ./...`
Expected: builds clean. (gopls may show stale "undefined dbgen method" — ignore; `go build` is truth.) `git diff --stat internal/platform/db/dbgen` shows only the new/changed queries.

- [ ] **Step 7: Commit**

```bash
git add db/query/repo_connector.sql db/query/code_review.sql internal/platform/db/dbgen
git commit -m "feat(007): sqlc queries for connector list/delete + review list/claim/finalize"
```

---

### Task 3: RepoConnectorService.List + Delete

**Files:**
- Modify: `internal/connectors/repo_service.go`
- Test: `internal/connectors/repo_service_integration_test.go` (create if absent; integration-tagged)

**Interfaces:**
- Consumes: `dbgen.ListRepoConnectors`, `dbgen.DeleteRepoConnector` (Task 2).
- Produces:
  - `func (s *RepoConnectorService) List(ctx, principalID, businessID uuid.UUID) ([]RepoConnectorSummary, error)`
  - `func (s *RepoConnectorService) Delete(ctx, principalID, businessID, id uuid.UUID) error` (→ `ErrNotFound` when 0 rows)
  - `type RepoConnectorSummary struct { ID, Type, DisplayName, BaseURL, Repo string; AllowPrivateBaseURL bool; CreatedAt time.Time }` (NO credential fields)

- [ ] **Step 1: Write the failing integration test (List + Delete + RLS)**

Read `internal/connectors/` for an existing connector integration test + its seed helpers; reuse them. Create `internal/connectors/repo_service_integration_test.go` (`//go:build integration`):
```go
func TestRepoConnectorListDelete(t *testing.T) {
    // seed tenant A, create 2 connectors; List returns 2 newest-first, no secret_ref.
    // Delete one → List returns 1. Delete a foreign id (tenant B) → ErrNotFound.
    // Delete already-deleted id → ErrNotFound.
}
```
Use the existing seed/sealer helpers (mirror `internal/agents/coding/service_integration_test.go` patterns). Assert `RepoConnectorSummary` has no token/secret field via the struct shape.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 -run TestRepoConnectorListDelete ./internal/connectors/`
Expected: FAIL (compile error: `List`/`Delete` undefined).

- [ ] **Step 3: Implement List + Delete**

Add to `internal/connectors/repo_service.go` (mirror the `WithPrincipal` + error-mapping pattern in `Resolve`):
```go
type RepoConnectorSummary struct {
    ID                  string
    Type                string
    DisplayName         string
    BaseURL             string
    Repo                string
    AllowPrivateBaseURL bool
    CreatedAt           time.Time
}

func (s *RepoConnectorService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]RepoConnectorSummary, error) {
    var out []RepoConnectorSummary
    err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
        rows, err := dbgen.New(tx).ListRepoConnectors(ctx, businessID)
        if err != nil {
            return err
        }
        for _, r := range rows {
            out = append(out, RepoConnectorSummary{
                ID: r.ID.String(), Type: r.Type, DisplayName: r.DisplayName,
                BaseURL: r.BaseUrl, Repo: r.Repo, AllowPrivateBaseURL: r.AllowPrivateBaseUrl,
                CreatedAt: r.CreatedAt.Time,
            })
        }
        return nil
    })
    if err != nil {
        return nil, fmt.Errorf("connectors: list repo connectors: %w", err)
    }
    return out, nil
}

func (s *RepoConnectorService) Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
    return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
        n, err := dbgen.New(tx).DeleteRepoConnector(ctx, dbgen.DeleteRepoConnectorParams{ID: id, BusinessID: businessID})
        if err != nil {
            return fmt.Errorf("connectors: delete repo connector: %w", err)
        }
        if n == 0 {
            return fmt.Errorf("connectors: repo connector not found: %w", errs.ErrNotFound)
        }
        return nil
    })
}
```
Adjust generated field names (`r.BaseUrl` etc.) to match what sqlc emitted — check `internal/platform/db/dbgen/repo_connector.sql.go`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -tags integration -p 1 -run TestRepoConnectorListDelete ./internal/connectors/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/repo_service.go internal/connectors/repo_service_integration_test.go
git commit -m "feat(007): RepoConnectorService List + Delete (RLS-scoped, no secret projection)"
```

---

### Task 4: Split Trigger → Enqueue (sync) + runJob (worker), extend read model

**Files:**
- Modify: `internal/agents/coding/service.go`
- Test: `internal/agents/coding/service_test.go` (extend; unit, no infra)

**Interfaces:**
- Consumes: `dbgen.InsertCodeReview` (now with principal/agent), `dbgen.ListCodeReviews`, `dbgen.GetCodeReview`.
- Produces:
  - `func (s *CodeReviewService) Enqueue(ctx, principalID, businessID, agentID, repoConnectorID uuid.UUID, prNumber int) (CodeReview, error)` — cheap validation (resolve connector, resolve cred, **egress pre-flight**), insert `pending` row with principal/agent, return `{ID, Status:"pending", PRNumber}`.
  - `func (s *CodeReviewService) runJob(ctx, job ClaimedReview) error` — the existing heavy pipeline (FetchPR→clone→sandbox→parse→post→finalize), re-resolving connector + cred under `job.PrincipalID`.
  - `func (s *CodeReviewService) List(ctx, principalID, businessID uuid.UUID) ([]CodeReview, error)`
  - Extend `CodeReview` struct with `Findings []connectors.Finding`, `FindingsCount int`, `CreatedAt`, `PostedAt *time.Time`; populate `ReviewURL` in `Get`/`List` from connector repo + `external_review_ref`.
  - `type ClaimedReview struct { ID, BusinessID, PrincipalID, AgentID, RepoConnectorID uuid.UUID; PRNumber, Attempts int }` (maps `dbgen.ClaimCodeReviewsRow`).

- [ ] **Step 1: Write failing unit tests**

Extend `internal/agents/coding/service_test.go`. The existing `TestTriggerRejects/AllowsHostInEgressAllowlist` tests call `Trigger`; rename the production method they call to `Enqueue` and assert it does NOT run the sandbox (Enqueue never touches `s.Sandbox`). Add:
```go
func TestEnqueueInsertsPendingWithPrincipalAndAgent(t *testing.T) {
    // fakeServiceDB capturing InsertCodeReview params; assert status pending,
    // principal_id + agent_id set; returns 202-shape CodeReview{Status:"pending"}.
}
func TestReviewURLConstructedFromConnectorAndRef(t *testing.T) {
    // unit: a helper reviewURL(repo, pr, ref) → "https://github.com/owner/r/pull/5#pullrequestreview-42"
}
```
(For the DB-capturing fake, extend `fakeServiceDB` to invoke `fn` with a fake `pgx.Tx` only if feasible; otherwise keep `Enqueue`'s pre-insert validation tests at unit level and cover the insert in the integration test in Task 7. Prefer a small pure `reviewURL` helper that is unit-tested directly.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agents/coding/ -run 'TestEnqueue|TestReviewURL'`
Expected: FAIL (undefined `Enqueue`/`reviewURL`).

- [ ] **Step 3: Implement the split + read-model extensions**

In `service.go`: rename `Trigger`→`Enqueue`; keep steps 1–3 (resolve connector, build client is NOT needed at enqueue — drop it here; resolve cred; `cred.Host()==""` check; egress pre-flight). Replace steps 4+ with a single pending insert that also stores `principal_id`, `agent_id`, then return `CodeReview{ID, Status:"pending", PRNumber}`. Move steps 5–10 (FetchPR→clone→sandbox→parse→post→finalize) into `runJob(ctx, job ClaimedReview)`, re-resolving connector + cred via `s.Repos.Resolve(ctx, job.PrincipalID, job.BusinessID, job.RepoConnectorID)` and `s.Creds.Resolve(ctx, job.PrincipalID, job.BusinessID, job.AgentID)`, building the github client there, and using `WithPrincipal(job.PrincipalID)` for DB writes. Add `List`. Add a pure helper:
```go
func reviewURL(repo string, pr int, externalRef string) string {
    if repo == "" || externalRef == "" {
        return ""
    }
    return fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-%s", repo, pr, externalRef)
}
```
Populate `ReviewURL` in `Get`/`List` (the row has `repo_connector_id`; the connector repo can be resolved, or store `repo` denormalized — simplest: in `Get`, also load the connector's `repo` via `Repos.Resolve` to build the URL; in `List`, batch is acceptable to skip and only fill URL when `external_review_ref != ""` by resolving per row, or leave `ReviewURL` empty in list rows and only populate in `Get`). Decide and document inline; the UI list only needs the GitHub link on terminal rows — populating in `Get` (detail) is sufficient, list rows can link via the detail page.

- [ ] **Step 4: Run unit tests**

Run: `go test ./internal/agents/coding/ -run 'TestEnqueue|TestReviewURL|TestTrigger'`
Expected: PASS. Then `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_test.go
git commit -m "feat(007): split code-review into Enqueue + runJob; add List + findings/review_url read model"
```

---

### Task 5: CodeReviewWorker (poll → claim → run → finalize, bounded retry)

**Files:**
- Create: `internal/agents/coding/worker.go`
- Test: `internal/agents/coding/worker_test.go` (unit, fake DB + fake runner)

**Interfaces:**
- Consumes: `dbgen.ClaimCodeReviews`, `dbgen.RequeueCodeReview`, `dbgen.FailCodeReview`; `CodeReviewService.runJob` (Task 4).
- Produces:
  - `type CodeReviewWorker struct { DB systemDB; Svc *CodeReviewService; Logger *slog.Logger; Poll time.Duration; LeaseSeconds int; MaxAttempts int; Batch int }`
  - `func (w *CodeReviewWorker) Run(ctx context.Context)` — loops until ctx done; each tick claims a batch (system path) and processes each row; on `runJob` error, `RequeueCodeReview` if `attempts < MaxAttempts` else `FailCodeReview`.
  - `systemDB` interface: the system/cross-tenant DB accessor the outbox worker uses (read `internal/platform/events/worker.go` to copy the exact pattern for non-RLS/system queries).

- [ ] **Step 1: Read the outbox worker for the system-DB pattern**

Read `internal/platform/events/worker.go` (`events.Worker`) — copy how it gets a system/super DB handle for cross-tenant polling and how it loops/sleeps with ctx cancellation. The claim query runs on that path; per-row `runJob` runs under `WithPrincipal`.

- [ ] **Step 2: Write failing unit tests**

Create `internal/agents/coding/worker_test.go`:
```go
// fake systemDB returns a scripted batch from ClaimCodeReviews, records Requeue/Fail calls.
func TestWorkerSuccessMarksDone(t *testing.T) { /* runJob ok → no requeue/fail */ }
func TestWorkerRetriesUntilMaxThenFails(t *testing.T) {
    // runJob always errors; attempts=1 → Requeue; attempts=MaxAttempts → Fail.
}
```
Inject a `runJob` seam (e.g. `w.runJob func(ctx, ClaimedReview) error` defaulting to `w.Svc.runJob`) so the unit test drives success/failure without a sandbox.

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/agents/coding/ -run TestWorker`
Expected: FAIL (undefined `CodeReviewWorker`).

- [ ] **Step 4: Implement the worker**

Write `internal/agents/coding/worker.go` with the loop, claim, per-row processing, bounded retry, and structured logging on each transition. Defaults: `Poll=3s`, `LeaseSeconds=900` (>10m sandbox cap), `MaxAttempts=3`, `Batch=2`. Use a `select { case <-ctx.Done(): return; case <-ticker.C: ... }` loop. On panic in `runJob`, recover → treat as failure (don't kill the worker).

- [ ] **Step 5: Run unit tests + build**

Run: `go test ./internal/agents/coding/ -run TestWorker` then `go build ./...`
Expected: PASS / clean.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/worker.go internal/agents/coding/worker_test.go
git commit -m "feat(007): CodeReviewWorker — durable queue poll/claim/run with bounded retry"
```

---

### Task 6: Handlers + routes + main wiring (async trigger, list/delete/get)

**Files:**
- Modify: `internal/agents/coding/handler.go`
- Modify: `cmd/manyforge/main.go`
- Test: `internal/agents/coding/handler_test.go` (extend; unit)

**Interfaces:**
- Consumes: `CodeReviewService.{Enqueue,List,Get}`, `RepoConnectorService.{List,Delete}`.
- Produces routes: `GET /businesses/{id}/repo-connectors`, `DELETE /businesses/{id}/repo-connectors/{rcID}` (gate `connectorsManage`); `GET /businesses/{id}/code-reviews` (gate `agentsRun`). `POST /code-reviews` now returns `202 {id,status:"pending"}` via `Enqueue`.

- [ ] **Step 1: Write failing handler unit tests**

Extend `internal/agents/coding/handler_test.go` (mirror existing `nilHandler`/`serveCoding` helpers and the bad-input tests): list returns `{items:[...]}`, delete bad id → 404, list/delete missing principal → 404, trigger returns 202. Use fakes for the services.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agents/coding/ -run TestHandler`
Expected: FAIL.

- [ ] **Step 3: Implement handlers + route registration**

Add `listRepoConnectors`, `deleteRepoConnector`, `listReviews` handlers (thin: principal+business parse → service → `httpx.WriteJSON`/`WriteError`). Register in `RepoConnectorRoutes` (`r.Get("/")`, `r.Delete("/{rcID}")`) and `CodeReviewRoutes` (`r.Get("/")`). Keep `triggerReview` but call `Enqueue`; response stays `202 {id,status,review_url}` (review_url empty for pending).

- [ ] **Step 4: Wire main.go — mount + start worker + boot reconcile**

In `cmd/manyforge/main.go`: the new GET/DELETE routes are already covered by the existing `connectorsManage`/`agentsRun` groups (verify `RepoConnectorRoutes`/`CodeReviewRoutes` now register them). Construct and start the worker after the service:
```go
crWorker := &coding.CodeReviewWorker{DB: database, Svc: codingSvc, Logger: logger}
go crWorker.Run(ctx)
```
(Use the same `ctx` that other background workers use so shutdown cancels it.) The lease-expiry reclaim in `ClaimCodeReviews` IS the boot reconcile — no separate sweep needed; note this in a comment.

- [ ] **Step 5: Run + build**

Run: `go test ./internal/agents/coding/ -run TestHandler` then `go build ./...`
Expected: PASS / clean.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/handler.go cmd/manyforge/main.go internal/agents/coding/handler_test.go
git commit -m "feat(007): async trigger + connector list/delete + review list routes; start worker"
```

---

### Task 7: OpenAPI + contract drift + async integration test + security pin

**Files:**
- Modify: `specs/007-coding-review-agents/contracts/openapi.yaml`
- Modify: `cmd/manyforge/drift_007_test.go`
- Modify: `internal/agents/coding/service_integration_test.go`
- Modify: `internal/security_regression/coding_review_pins_test.go`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Add the 3 ops to `inScope007Ops` (failing contract test)**

Edit `cmd/manyforge/drift_007_test.go` `inScope007Ops` to add:
```go
"GET /businesses/{}/repo-connectors",
"DELETE /businesses/{}/repo-connectors/{}",
"GET /businesses/{}/code-reviews",
```

- [ ] **Step 2: Run contract test to verify it fails**

Run: `go test -tags contract ./cmd/manyforge/ -run TestOpenAPIDrift007`
Expected: FAIL — ops in router but not in openapi.yaml (and/or vice versa).

- [ ] **Step 3: Document the ops in openapi.yaml + fix the schema bug**

Add the three paths/operations to `specs/007-coding-review-agents/contracts/openapi.yaml` (responses per spec §5: list `{items:[...]}`, delete 204, list reviews `{items:[...]}`). While there, fix `CreateRepoConnectorInput` to match the handler (`type`, `display_name`, `base_url`, `repo`, `api_token`, `allow_private_base_url`) and `CodeReview` to include `findings`.

- [ ] **Step 4: Contract test passes**

Run: `go test -tags contract ./cmd/manyforge/ -run TestOpenAPIDrift007`
Expected: PASS.

- [ ] **Step 5: Async e2e + RLS integration test**

Extend `internal/agents/coding/service_integration_test.go`: a test that calls `Enqueue` (→ pending row), runs ONE worker tick (call the claim+runJob path directly with a `validFakeRunner`), and asserts the review reaches `succeeded` with findings + exactly one GitHub POST. Add an RLS test: tenant B `List`/`Get`/`Delete` of tenant A's review/connector → empty/`ErrNotFound`. Add a crash-recovery test: a `running` row with `lease_expires_at` in the past is re-claimed.

- [ ] **Step 6: Security pin — no secret projection**

Add to `internal/security_regression/coding_review_pins_test.go` (`MF007-PIN-8`): source-level assert that `ListRepoConnectors`/`GetRepoConnector` SQL does not `SELECT secret_ref`, and that `RepoConnectorSummary`/the list-review response struct have no `Token`/`APIToken`/`SecretRef` field (reflection).

- [ ] **Step 7: Full gate + commit**

Run: `make test && make lint && go test -tags contract ./cmd/... && make sec-test && go test -tags integration -p 1 ./internal/agents/coding/... ./internal/connectors/...`
Expected: all green.
```bash
git add specs/007-coding-review-agents/contracts/openapi.yaml cmd/manyforge/drift_007_test.go internal/agents/coding/service_integration_test.go internal/security_regression/coding_review_pins_test.go
git commit -m "test(007): contract ops + async/RLS integration + no-secret pin (MF007-PIN-8)"
```

---

## Self-Review

- **Spec coverage:** schema (§4.1)→T1; sqlc (§4.2)→T2; services List/Delete/Enqueue/runJob/List + read model (§4.3, §5)→T3,T4; worker (§3, §4.3)→T5; routes/gates/async (§4.4)→T6; OpenAPI+drift+tests+pins (§4.5, §8)→T7. Security (§7): RLS via WithPrincipal (T3,T4), no-secret projection (T2 SQL, T7 pin), egress pre-flight at enqueue (T4). All covered.
- **Types:** `ClaimedReview` (T4) ↔ `dbgen.ClaimCodeReviewsRow` (T2) — verify field names after sqlc gen. `RepoConnectorSummary` (T3) consumed by handler (T6). `CodeReview` extended fields (T4) consumed by list/get handlers (T6) + UI (Plan B).
- **Open verification during impl:** exact sqlc-generated field names (`BaseUrl` vs `BaseURL`), the system-DB accessor signature (copy from `events.Worker`), and whether `Get` resolves the connector for `review_url` or denormalizes — decided inline in T4.

## Execution Handoff

See end of Plan B for the combined execution-choice prompt (backend plan executes first).
