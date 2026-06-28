# Code Review UI + Durable Async Reviews — Design

> **Date:** 2026-06-21 · **Epic:** `manyforge-7ml` (Spec 007 — Coding & Review Agents) ·
> **Parent spec:** [`specs/007-coding-review-agents/spec.md`](../../../specs/007-coding-review-agents/spec.md) ·
> **Builds on:** slice 1 (read-only code-review agent, API-only) and the egress-allowlist fix (`manyforge-0qj`, PR #5).

## 1. Problem & goal

Spec 007 slice 1 shipped a working code-review agent but **no UI** — connectors are created and
reviews triggered only via `curl` (`specs/007-coding-review-agents/quickstart.md`). A user who opens
the app "sees nothing" for code review.

**Goal:** a first-class **Code Review** page in the web app where a user can manage GitHub repo
connectors, point a code-review agent at a PR, and watch the review run to completion with its
findings — without touching the API.

A second goal falls out of the first: `POST /code-reviews` currently **blocks** the HTTP request
until the sandbox exits (up to the 10-minute cap), which is unworkable from a browser. So this slice
also makes review execution **durable and asynchronous**.

## 2. Scope

**In scope**
- Repo-connector management UI: list, create, delete.
- "Review a PR" form: pick agent + connector, enter PR number, trigger.
- Review **history** list (live status) and a **detail** view with the full findings table.
- Backend: durable async execution (queue + worker), new read/list/delete endpoints, read-model
  extension (findings + GitHub review URL), OpenAPI + contract-drift updates, a security pin.

**Out of scope (non-goals)**
- Webhook auto-trigger, GitLab support, PR authoring (later 7ml slices).
- Per-run egress narrowing (tracked separately; a run may still reach any boot-allowlisted host).
- Pagination / search on history (cap to recent N).
- Retry backoff tuning beyond a small bounded retry.
- Editing a connector (delete + recreate is enough for slice 1).

## 3. Architecture — durable async work queue (approach C, table-as-queue)

Triggering is split into a fast **enqueue** (synchronous, request-scoped) and a durable **run**
(asynchronous, worker-scoped). The `code_review` row **is** the queue item — claim/lease columns live
on it, so there is one lifecycle source of truth and no second table.

```
POST /code-reviews ──► enqueue (sync):
                         1. resolve connector (RLS ownership)         → 404 if not yours
                         2. resolve AI credential + egress pre-flight → 400 ErrValidation
                         3. insert code_review (status='pending', principal_id, agent_id,
                            repo_connector_id, pr_number, run_after=now())
                         4. return 202 { id, status:'pending' }

worker loop (every ~3s, started in main.go like outboxWorker):
   claim N rows:  UPDATE code_review SET status='running',
                  lease_expires_at = now()+15m, attempts = attempts+1
                  WHERE id IN (SELECT id FROM code_review
                               WHERE (status='pending' AND run_after <= now())
                                  OR (status='running' AND lease_expires_at < now())  -- crash recovery
                               ORDER BY created_at
                               FOR UPDATE SKIP LOCKED LIMIT N) RETURNING ...
   per row (under WithPrincipal(row.principal_id)):
     - run pipeline: FetchPR → clone → sandbox(opencode) → parse → PostReview → finalize
     - success → status='succeeded' (+ findings, summary, external_review_ref, posted_at)
     - failure → if attempts < max(3): status='pending', run_after=now()+30s, lease_expires_at=NULL (retry)
                 else:                 status='failed', last_error set
```

**Why a worker queue (C) over an in-process goroutine (A):** survives server restart (queued/leased
jobs are reclaimed from the table, not lost with the process), gives bounded retry for free, and
decouples the long run from request lifecycle. **Cost (be honest):** it adds a migration, a job
table, sqlc queries, and a worker loop — materially more machinery than A for a low-frequency job.
See §9 for the trade-off and the simpler alternative.

**Secrets never enter the queue.** The job row stores only ids
(`code_review_id`, `principal_id`, `business_id`, `agent_id`, `repo_connector_id`, `pr_number`). The
worker **re-resolves** the GitHub PAT and the LLM key fresh per run via the existing
vault/`Resolve` path — preserving the "no ambient creds / resolve-per-run" property from slice 1.

**Two DB contexts, on purpose.** *Claiming* jobs is a system-level operation (the worker polls
across all tenants, so the claim runs on the system/super path the outbox worker already uses).
*Processing* a claimed job runs under `WithPrincipal(job.principal_id)`, so the same row-level-security
policies apply to every connector/credential/review read and write outside an HTTP request (RLS is
driven by the passed principal, not request-bound auth). Confirm the system-path mechanism against
`events.Worker` during implementation.

### Lifecycle (single source of truth)

`code_review.status` is the only lifecycle field — the row IS the queue item.

| status | meaning | claimable by worker? |
|---|---|---|
| `pending`   | enqueued, or requeued after a retriable failure once `run_after` passes | yes, when `run_after <= now()` |
| `running`   | leased by a worker; `lease_expires_at` set | only if the lease has expired (crash recovery) |
| `succeeded` | terminal — review posted | no |
| `failed`    | terminal — max attempts exhausted; `last_error` set | no |

No second table, no dual state. The worker is the only writer of the claim/lease columns.

## 4. Backend changes

### 4.1 Schema (new migration `0072_code_review_queue`)
Extend `code_review` itself into the work queue (single source of truth) rather than adding a
separate job table:
```sql
ALTER TABLE code_review
  ADD COLUMN principal_id     uuid,                  -- actor whose RLS context the worker assumes
  ADD COLUMN agent_id         uuid,                  -- which agent's AI credential to resolve
  ADD COLUMN attempts         integer NOT NULL DEFAULT 0,
  ADD COLUMN run_after        timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN lease_expires_at timestamptz,
  ADD COLUMN last_error       text NOT NULL DEFAULT '';
CREATE INDEX code_review_claim_idx ON code_review (status, run_after);
```
`principal_id`/`agent_id` are added **nullable** so the migration applies cleanly over existing
slice-1 rows (all terminal — never claimed). The service sets them on every new row; a later
migration can tighten to `NOT NULL` once no NULLs remain. The worker's cross-tenant claim runs on the
system/super path the outbox worker already uses; per-row processing is `WithPrincipal(principal_id)`
(see §3). RLS on `code_review` is unchanged.

### 4.2 sqlc queries
- `repo_connector.sql`: **`ListRepoConnectors :many`** (RLS-scoped, no secret_ref in projection),
  **`DeleteRepoConnector :exec`** (by id; RLS-scoped).
- `code_review.sql`: **`ListCodeReviews :many`** (id, pr_number, status, summary, findings-count or
  findings, external_review_ref, repo_connector_id, created_at, posted_at), and ensure
  **`GetCodeReview`** projects `findings` (it already SELECTs the column).
- `code_review.sql` (queue): **`ClaimCodeReviews`** (the UPDATE…RETURNING above, system path),
  **`RequeueCodeReview`** (retriable failure → status='pending', bump `run_after`, clear lease),
  **`FailCodeReview`** (terminal → status='failed', set `last_error`). `InsertCodeReview` gains
  `principal_id`, `agent_id`, `run_after`; `UpdateCodeReviewResult` (success) must also clear
  `lease_expires_at`.

> sqlc generate uses the pinned **v1.27.0** bottle (`/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`);
> the global v1.31.1 re-churns generated code.

### 4.3 Services
- `RepoConnectorService.List(ctx, principalID, businessID)` and `.Delete(ctx, principalID, businessID, id)`.
- `CodeReviewService`: split `Trigger` →
  - **`Enqueue(ctx, principal, business, agent, connector, pr) (CodeReview, error)`** — cheap sync
    validation (connector resolve, cred resolve, **egress pre-flight**), insert `pending` review +
    `queued` job, return.
  - **`runJob(ctx, job)`** — the existing heavy pipeline (FetchPR → clone → sandbox → parse → post →
    finalize), refactored to take a claimed job and re-resolve secrets.
  - **`List(ctx, principal, business)`** and extend `Get` to return `findings` + a constructed
    `review_url` (`https://github.com/{repo}/pull/{pr}#pullrequestreview-{external_review_ref}` for
    `github.com`; derive host from connector base_url for GHE).
- **`CodeReviewWorker`** — a `Run(ctx)` loop (poll → claim → process), started in `main.go` next to
  `outboxWorker`. Bounded concurrency (start with 1–2). Lease (15m) > sandbox timeout (10m) so a
  live run is never reclaimed mid-flight.
- **Startup reconciliation:** on boot, requeue `leased` jobs whose `lease_expires_at` is past (the
  poll already does this; an explicit boot sweep makes recovery immediate).

### 4.4 HTTP routes (same permission gates as slice 1)
| Method & path | Gate | Handler |
|---|---|---|
| `GET /businesses/{id}/repo-connectors` | `connectors.manage` | list |
| `DELETE /businesses/{id}/repo-connectors/{rcID}` | `connectors.manage` | delete |
| `GET /businesses/{id}/code-reviews` | `agents.run` | list |
| `POST /businesses/{id}/code-reviews` | `agents.run` | enqueue (now truly 202 async) |
| `GET /businesses/{id}/code-reviews/{reviewID}` | `agents.run` | get (now returns findings + url) |

`POST /repo-connectors` and the create flow are unchanged.

### 4.5 OpenAPI + contract drift
Add the three new operations to `specs/007-coding-review-agents/contracts/openapi.yaml` **and** to
`inScope007Ops` in `cmd/manyforge/drift_007_test.go` (the drift test asserts both directions for any
route containing `/repo-connectors` or `/code-reviews`). While editing the spec, fix the existing
`CreateRepoConnectorInput` schema bug (`repo_url`/`token` → `type`/`display_name`/`base_url`/`repo`/`api_token`).

## 5. API contract (request/response shapes)

```jsonc
// GET /repo-connectors → 200
{ "items": [ { "id": "...", "type": "github", "display_name": "...", "repo": "owner/name",
               "base_url": "https://api.github.com", "created_at": "..." } ] }   // no secret_ref

// DELETE /repo-connectors/{rcID} → 204

// POST /code-reviews  { "agent_id", "repo_connector_id", "pr_number" } → 202
{ "id": "...", "status": "pending" }

// GET /code-reviews → 200
{ "items": [ { "id", "pr_number", "status", "summary", "findings_count",
               "repo_connector_id", "review_url", "created_at", "posted_at" } ] }

// GET /code-reviews/{id} → 200
{ "id", "pr_number", "status", "summary", "review_url", "posted_at", "created_at",
  "findings": [ { "file", "line", "severity", "title", "detail" } ] }
```

## 6. Frontend (web/, Angular)

Mirrors the established `/credentials/ai` + `/agents` conventions: **standalone components, signals,
`CurrentBusinessService`** for the active business, `HttpClient`, the `mf-*` CSS classes, and a
`data-testid` on every interactive element.

- **Route & nav:** add `{ path: 'code-review', canActivate: [authGuard], loadComponent: … }` to
  `app.routes.ts` and a `{ label: 'Code Review', route: '/code-review', testid: 'nav-code-review' }`
  NavItem.
- **Service:** `CodeReviewService` (`core/code-review.service.ts`) with
  `listConnectors/createConnector/deleteConnector` and `listReviews/getReview/triggerReview`, base
  `/api/v1/businesses/${bid}/{repo-connectors|code-reviews}`. Reuses `AgentsService.list` for the
  agent dropdown.
- **Page** `pages/code-review/` — one page, three sections (matching the approved mock):
  1. **Connectors** — table (repo, type, created) + Add form (display_name, repo, base_url default
     `https://api.github.com`, api_token, allow_private_base_url) + delete-with-confirm.
  2. **Review a PR** — agent `<select>`, connector `<select>`, PR number input, **Review PR** button →
     `triggerReview` → optimistic row in History.
  3. **History** — table of reviews (PR #, status badge, findings count, time, GitHub link); a row
     links to the **detail** view (summary + findings table + "View on GitHub").
- **Polling:** while any visible review is `pending`/`running` (or a review just triggered), poll
  `listReviews` (or `getReview` on the detail page) every ~3s; stop when all visible rows are terminal.
  Use a signal-driven `setInterval`/`timer` cleaned up on destroy.
- **Validation surfacing:** a `400` from the egress pre-flight (provider host not allowlisted) or a
  `404` (connector not yours) renders inline near the form via the existing `mf-err` pattern.

## 7. Security

- New endpoints reuse the **existing permission gates** (`connectors.manage`, `agents.run`) and the
  RLS-bound, 404-on-missing-or-foreign semantics (no 403/404 existence oracle).
- **List/Get never project `secret_ref` or any credential.** The PAT stays sealed; the LLM key is
  resolved only inside the worker and only passed to the sandbox.
- **Egress pre-flight** (shipped in PR #5) runs at enqueue → a misconfigured provider host fails fast
  with `ErrValidation` instead of a doomed background run.
- The new queue columns hold only ids (`principal_id`, `agent_id`) — **no secrets**; the worker
  re-resolves the PAT and LLM key fresh per run.
- `DELETE` is RLS-scoped; deleting a connector cascades its `code_review` rows per the existing FK
  (call this out in the delete-confirm copy).

## 8. Test plan

**Backend — unit (no infra):**
- `RepoConnectorService.List/Delete`: returns own rows, foreign id → not-found, delete is scoped.
- `CodeReviewService.Enqueue`: inserts pending + queued job, returns 202 shape; **disallowed egress
  host → ErrValidation, no job enqueued** (extends the `manyforge-0qj` unit tests).
- Worker claim/transition logic with a fake DB + fake runner: queued→leased→done on success;
  failure path requeues until max then dead/failed; lease-expiry reclaim.

**Backend — integration (`-tags integration`, testdb + stub sandbox):**
- End-to-end async: enqueue → worker claims → stub sandbox writes `review.json` → review reaches
  `succeeded`, findings + `review_url` populated, exactly one GitHub `POST /reviews` (extend the
  existing `service_integration_test.go` GitHub stub).
- **RLS isolation:** tenant B cannot list/get/delete tenant A's connectors or reviews (404).
- Crash recovery: a `leased` job with an expired lease is reclaimed and completes.

**Backend — contract & security pins:**
- `go test -tags contract ./cmd/...` green after adding the 3 ops to openapi + `inScope007Ops`.
- Extend `coding_review_pins_test.go`: a pin that list/get responses don't contain `secret_ref`/token
  fields, and that the `code_review` row/struct carries no API-key/secret field (source-level + reflection).

**Frontend — unit (`*.spec.ts`, `HttpTestingController`):**
- Service builds correct URLs/bodies; list/detail components render rows/findings from mocked responses;
  delete-confirm calls DELETE; trigger posts the right body.

**Frontend — e2e (Playwright, `web/e2e/code-review.spec.ts`):**
- Mock the API: list connectors → fill "Review a PR" → trigger (202 pending) → poll returns
  `running` then `succeeded` with findings → History row shows succeeded, detail shows the findings
  table and the GitHub link. Assert the polling stops.

**Real-browser verification (required for UI before "done"):**
- Against the running stack (`:8081` API, `:4300` web), drive the page in a real browser (gstack
  `$B` / Playwright MCP): create a connector, trigger a review on a PR, watch it transition, open the
  detail. Capture before/after screenshots. (This doubles as the `manyforge-2nd` real-opencode e2e if
  a real LLM key + PAT are supplied; otherwise verify the UX with the stub image.)

**Gate:** `make test` + `make lint` + `go test -tags contract ./cmd/...` + `make sec-test` +
the coding integration tests + frontend unit + the new Playwright spec all green.

## 9. Trade-offs & alternatives considered

- **Async approach A (in-process goroutine)** — simplest; no migration/worker. Loses in-flight runs
  on restart (mitigated by a startup sweep). **Rejected per user choice of C**, but it is the clean
  off-ramp if the queue machinery feels too heavy at review.
- **Async approach B (outbox/event worker)** — reuses `events.Worker`, but that path is built for
  short transactional handlers, not multi-minute compute. Rejected.
- **Queue realization:** **table-as-queue (chosen)** — claim/lease columns on `code_review` itself,
  one lifecycle source of truth, no second table. The rejected alternative was a dedicated
  `code_review_job` table; it isolates retry/lease bookkeeping but duplicates lifecycle state across
  two tables. Approach C's durability (survives restart, bounded retry, lease-based crash recovery) is
  fully preserved in the table-as-queue form.
- **Detail depth:** full findings table in-app (chosen) duplicates GitHub but is self-contained;
  requires `Get` to project findings.

## 10. Rollout / branch hygiene

- **Before any code:** merge PR #5 (egress fix) and branch fresh from `master` — the repo rule is
  one branch off `master` at a time.
- Tracked as `manyforge-elo` (child of epic `manyforge-7ml`).
- Ship behind the same boot tolerance as slice 1: if Docker/egress infra is absent the page still
  loads and a triggered review fails cleanly (no crash).
