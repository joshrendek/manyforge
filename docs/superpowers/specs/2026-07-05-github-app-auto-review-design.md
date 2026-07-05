# Design: org-wide code review via a GitHub App

**Status:** design (pre-plan) · **Date:** 2026-07-05 · **Supersedes the "webhook auto-trigger" fast-follow deferred in spec 007**

## 1. Goal & context

Auto-trigger a manyforge code review when a GitHub pull request is opened or
pushed, across **whole GitHub organizations**, via a **GitHub App**. Today the
review pipeline is manual/API-only, per-repo, PAT-authenticated, and requires a
logged-in user. Spec 007 explicitly deferred both webhook auto-trigger and the
GitHub App; this design builds them.

**Reused unchanged** (verified against the code): the async review pipeline
(`CodeReviewService.Enqueue` → pending `code_review` row → `claim_code_reviews`
worker → `runJob` → clone → KubeRunner sandbox → opencode/GLM → `PostReview`),
the GitHub REST client (`internal/connectors/github/client.go`: `FetchPR`,
`ChangedFiles`, `PostReview` with its hidden `<!-- manyforge-review-id -->`
marker + `findReviewByMarker` idempotency), repo-connector storage + vault +
RLS, and the signed-webhook template shape in `internal/connectors/webhook.go`
(principal-less public route → SECURITY DEFINER lookup → unseal → constant-time
HMAC verify → dedupe → uniform 202).

## 2. Locked decisions

| # | Decision | Choice |
|---|----------|--------|
| D1 | Integration/auth | **GitHub App** (one App per instance; installations mapped to a business) |
| D2 | Which PRs | **Skip drafts, bots, and forks**; per-PR opt-out via a `no-manyforge-review` label |
| D3 | On new push (`synchronize`) | **Supersede** — cancel the pending review for the old commit, review the latest |
| D4 | Guardrails | **Per-installation monthly token/cost budget + max-concurrent / per-hour caps** |
| D5 | App-cred provisioning | **App-manifest one-click**, hardened: instance-operator gate that refuses to overwrite an existing App, and a signed/single-use/session-bound callback |
| D6 | App-triggered review → pipeline | **App-backed `repo_connector`** (auto-created `type=github_app` row per repo; token minted at `Resolve` time) — downstream pipeline unchanged |
| D7 | App visibility | **One public App + OAuth-verified linking** (`GET /user/installations` proves control before linking; unlinked installs quarantined) |

## 3. Architecture overview

**Setup (once):** the instance operator runs the in-app manifest flow → GitHub
creates a **public** App → the callback persists `app_id` + private key +
webhook secret to `github_app_config`. The operator installs the App on each
org; each install fires an `installation` webhook that records an **unlinked**
installation. In manyforge, a logged-in business member links each installation
to their business + picks a review **agent**; linking is proven via GitHub OAuth
(`GET /user/installations`).

**Steady state (per PR event):** GitHub POSTs `pull_request` →
`POST /api/v1/github/webhook` → verify `X-Hub-Signature-256` → dedupe on
`X-GitHub-Delivery` → resolve installation via a SECURITY DEFINER context
function → apply filters (draft/bot/fork/label) and budget/rate caps →
**supersede** any pending review for `(installation, repo, pr)` → ensure an
app-backed `repo_connector` for the repo → `Enqueue` under the **agent's own
principal**. The existing worker claims the row; `runJob` resolves the
connector, which mints a fresh **per-repo installation token** to clone and
`PostReview`.

```
GitHub ──pull_request──▶ /api/v1/github/webhook
                          │ verify HMAC · dedupe delivery-id
                          │ DEFINER: installation → (business, agent, agent_principal, config, budget)
                          │ filters + budget · supersede pending · ensure app-backed connector
                          ▼
                    Enqueue(agent_principal, business, agent, repo_connector, prNumber)
                          ▼  (unchanged)
        claim_code_reviews → runJob → Resolve(connector) mints per-repo token
                          → clone (KubeRunner) → opencode/GLM → PostReview
```

## 4. Components & data model

### 4.1 `github_app_config` — instance singleton (new table)
- Columns: `id` (fixed singleton), `app_id`, `slug`, `client_id`,
  encrypted `private_key_pem`, encrypted `client_secret`, encrypted
  `webhook_secret`, `created_at`.
- **No RLS** (tenantless): follow the `principal`-table precedent —
  `GRANT SELECT` to `manyforge_app`, never exposed via any tenant API.
- Sealed with a **dedicated master key** `MANYFORGE_GITHUB_APP_MASTER_KEY`
  (matching the existing per-domain key pattern in `config.go`); the feature
  fails **closed** if the key is unset.
- Written **once** by the manifest callback; the callback **refuses to
  overwrite** a non-empty row (rotation is an explicit ops action: delete + rerun).

### 4.2 `github_app_installation` — install → business mapping (new table)
- Columns: `id`, `installation_id` (bigint, unique), `account_login`,
  `account_type`, `business_id` (nullable until linked), `tenant_root_id`,
  `agent_id` (the review agent; its `principal_id` is the machine identity),
  `enabled`, `config` jsonb (filter overrides + budget), `suspended_at`,
  `deleted_at`, timestamps.
- **RLS-scoped** to `business_id` once linked. **Unlinked** rows
  (`business_id IS NULL`) are visible only via a DEFINER path used by the
  linking flow — never cross-tenant.
- Lifecycle from `installation` webhooks: `created` → upsert unlinked;
  `deleted` → `deleted_at`; `suspend`/`unsuspend` → `suspended_at`. A
  suspended/deleted installation stops triggering reviews.

### 4.3 App-backed `repo_connector` (extend existing table) — D6
- New allowed `type='github_app'` (relax `repo_connector_type_chk`); a new
  unique index `(business_id, repo)` enforces one connector per repo per business.
- An app-backed row carries `repo='owner/name'`, `config.installation_id`, and
  **no `secret_ref`** (nullable, since there is no stored PAT).
- **Ensure-connector** step (DEFINER, idempotent): on a qualifying PR event, look
  up `(business_id, repo)`; if a connector already exists (manual `github` **or**
  app-backed), reuse it; otherwise auto-create the app-backed row. This lets a
  pre-existing manual PAT connector for the same repo coexist and take precedence.
- `RepoConnectorService.Resolve` gains a branch: for `type='github_app'`, mint a
  **fresh per-repo installation token** and return it as the `Credential.APIToken`
  — so `runJob`, the claim scan, `Get`/`ReviewURL`, clone auth, and `PostReview`
  are **all unchanged**, and the invariant "every review has a connector" holds.
- Token freshness is identical to a stored PAT (minted at resolve/claim time,
  never stored in the queue row).

### 4.4 `github_installation_context(installation_id)` — DEFINER (new)
Mirrors `connector_webhook_context` (migration 0043): resolves an incoming
`installation_id` to `(business_id, tenant_root_id, agent_id, agent_principal_id,
enabled, suspended, config)` bypassing RLS, so the principal-less webhook handler
can act. Returns "unmapped/disabled" without leaking existence.

### 4.5 `github_webhook_delivery` — replay dedupe (new) — M3
`delivery_id` (GitHub `X-GitHub-Delivery` GUID) PK, `received_at`. DEFINER
insert `ON CONFLICT DO NOTHING` **inside the same tx as enqueue**; a re-delivered
GUID is a no-op (uniform 202). TTL-pruned. Plus a `(repo, pr, head_sha)`
already-succeeded check so `reopened` / out-of-order `synchronize` don't re-bill
an already-reviewed commit.

### 4.6 Machine identity — no new principal (M1)
`Enqueue` runs under the **review agent's own `principal_id`** (every agent
already owns a `kind='agent'` principal with an `agent_runtime` membership —
migrations 0001/0004/0029). `code_review.principal_id` = that agent principal.
No bot principal, no new role: the webhook enqueues at the service layer (no
route middleware), and `authorized_businesses(current_principal())` already
grants the agent principal full RLS visibility of its home business.

## 5. GitHub App authentication (new `githubapp` package)

- **App JWT:** RS256, `iss=app_id`, `iat = now − 60s` (clock-skew guard),
  `exp ≤ now + 10m`, signed with the private key. (M5)
- **Installation token:** `POST /app/installations/{id}/access_tokens` with the
  App JWT, body `{"repositories":["<name>"]}` to scope the token to the **single
  repo** under review (C4 — a default token spans every repo in the install).
  Optionally downscope `permissions` to `contents:read, pull_requests:write`.
- **Cache** per `(installation_id, repo)` keyed on `expires_at − 5m`;
  **retry-once** on a 401 with a freshly minted token (covers rotation).
- **Terminal failures:** a suspended/deleted/permission-revoked installation
  maps to a non-requeued terminal failure class (don't burn 3 worker retries on
  a dead install).
- Manifest declares permissions `contents:read`, `pull_requests:write`,
  `metadata:read`; subscribed events `pull_request` (installation lifecycle
  events are delivered to Apps automatically and are **not** listed in
  `default_events`, m3). `PATCH /app/hook/config` updates the webhook URL if the
  instance host changes (M4).

## 6. Webhook route `POST /api/v1/github/webhook` (principal-less, public)

1. Read raw body; verify `X-Hub-Signature-256` HMAC-SHA256 against the App
   webhook secret, **constant-time**. Missing/unconfigured → 202; bad sig → 401;
   valid → continue. Uniform 202 for everything accepted (no existence oracle).
2. Defense-in-depth: verify `X-GitHub-Hook-Installation-Target-ID == app_id` (m7).
3. Dedupe `X-GitHub-Delivery` (§4.5).
4. Route on `X-GitHub-Event`:
   - `installation` → upsert/soft-delete the installation row (§4.2).
   - `pull_request`, actions `opened` / `reopened` / `synchronize` /
     `ready_for_review` → the trigger (§7–9).
5. Respond within GitHub's 10s deadline — all work is the single enqueue tx.

Note (m2): GitHub does **not** auto-retry failed deliveries; a restart during a
PR event drops it. v1 accepts this; a later reconciliation sweep
(`GET /app/hook/deliveries` + redeliver) is out of scope.

## 7. Setup & linking UX

### 7.1 Manifest flow (hardened) — D5
- `GET /api/v1/github/app/manifest` (gated: authenticated principal whose email
  matches `MANYFORGE_INSTANCE_OPERATOR_EMAIL`, else 404) renders an
  auto-submitting form POSTing the App manifest (name, **public**, webhook URL,
  permissions, events, `redirect_url`, server-generated single-use `state`) to
  `https://github.com/settings/apps/new` (user-owned; org-owned variant
  documented for operators who prefer it).
- GitHub redirects to the callback with `?code=...&state=...`. Callback
  (same operator gate) validates the single-use, session-bound `state`, exchanges
  the `code` via `POST /app-manifests/{code}/conversions` (1h expiry — handle
  failure), and stores `id`/`slug`/`client_id`/`client_secret`/`pem`/
  `webhook_secret` in `github_app_config` — **refusing to overwrite** an existing
  config.

### 7.2 OAuth-verified linking — C1 / D7
- A logged-in business member with the connector-manage permission starts linking
  → redirect to `https://github.com/apps/{slug}/installations/new?state=<signed,
  single-use, bound to (business_id, principal_id)>`.
- The App has **"Request user authorization (OAuth) during installation"** on;
  GitHub returns to the App's setup redirect with an OAuth `code` +
  `installation_id`. manyforge exchanges the `code` for a user token and calls
  `GET /user/installations`, **requiring** the claimed `installation_id` to be
  present — proof the user actually holds the installation. Only then is
  `github_app_installation.business_id`/`agent_id` set. This closes the
  cross-tenant install-hijack (C1).
- Unlinked / unproven installations trigger no reviews.

## 8. Filtering & budget — D2 / D4

- **Filters** (from the `pull_request` payload; per-installation overridable via
  `config`): skip `draft==true`; skip bot authors (`user.type=='Bot'`); skip
  forks via **`head.repo == null || head.repo.id != base.repo.id`** (repo-id
  comparison + null-safe, M7); skip if the `no-manyforge-review` label is
  present. The label is re-checked at claim time in `runJob` since labels are
  usually applied after `opened` and we don't subscribe to `labeled` (m1).
- **Budget** (per installation): month-to-date `SUM(code_review.cost_cents)` vs a
  cap (index `(installation_id, created_at)`); a max in-flight count and a
  per-hour count. Over any cap → skip + structured log; **no** PR comment from
  the webhook path (m5). Because `cost_cents` lands at finalize, the
  concurrent/hourly caps bound the month-cap overshoot — documented, not exact.

## 9. Supersede & dedup semantics — D3 / C3

- **Pending supersede (race-free):** DEFINER `supersede_pending_reviews(
  installation, repo, pr)` does `UPDATE code_review SET status='superseded'
  WHERE ... AND status='pending'` — safe against the claim lease, which flips
  `pending→running` under `FOR UPDATE SKIP LOCKED`.
- **Status guards:** add `WHERE status='running'` to `requeue_code_review` and
  `fail_code_review` so a superseded row can't be resurrected by a transient
  sandbox failure. Extend `code_review_status_chk` to include `'superseded'`.
- **Running rows (best-effort):** set a `supersede_requested` flag; `runJob`
  re-reads it immediately before `PostReview` and skips posting if set, and
  `UpdateCodeReviewResult` is conditional on `status='running'`. Residual
  seconds-wide race worst case = one extra advisory review (noise, not
  corruption) — documented.
- **True k8s-Job cancellation is out of scope for v1** (needs a
  `code_review.id → Job-name` registry); a superseded running review finishes its
  sandbox work but does not post.
- Cross-commit dedup: the `(repo, pr, head_sha)` succeeded-check (§4.5) prevents
  re-billing an already-reviewed head on `reopened` / duplicate deliveries.

## 10. Security posture

- Webhook: raw-body constant-time HMAC; uniform 202 / 401-only-after-known-config;
  delivery-id replay dedupe; installation-target-id cross-check.
- Secrets: App private key + client secret + webhook secret encrypted at rest
  under a dedicated master key; `github_app_config` never exposed via any API;
  fails closed when the key is unset. Zero secrets in git.
- Least privilege: per-repo, short-TTL installation tokens (not a broad PAT);
  minted at claim time, never stored in the queue row.
- Untrusted code: fork PRs skipped by default (the sandbox holds both the LLM key
  and a repo-scoped token; hostile-diff prompt-injection exfil is unmodeled, so
  forks stay skip-only until threat-modeled).
- Tenancy: OAuth-verified linking (C1); operator-gated + non-overwriting manifest
  config (C2); machine identity is the agent's existing principal, no privilege
  invention (M1).
- The KubeRunner sandbox isolation and egress allowlist are unchanged.

## 11. Testing

- **Unit:** App JWT construction (iat backdating, exp bound) + per-repo token
  minting/caching/401-retry against a fake GitHub; webhook signature verify
  (good/bad/missing) + delivery-id dedupe; event parsing; the filter matrix
  (draft/bot/fork-by-id/null-head/label); supersede state machine + status
  guards; budget/rate enforcement; OAuth-verified-linking accept/reject.
- **Integration:** signed `pull_request` delivery → app-backed connector
  auto-created → pending row → review via `FakeRunner` → `PostReview` marker;
  manifest callback → `github_app_config` populated (+ overwrite refusal);
  supersede on a second `synchronize`.
- **Security-regression pins** (dedicated file per finding, source-level pins):
  signature verification present; fork/bot/draft skip; per-repo token scoping
  (`"repositories"` in the mint request); operator gate on the manifest routes;
  OAuth `GET /user/installations` check on linking; status guards on
  requeue/fail; `github_app_config` not exposed via any handler.

## 12. Observability — m6

Structured events: `webhook.received`, `webhook.skipped{reason}` (the #1 support
question), `review.enqueued`, `review.superseded`, `review.budget_blocked`,
`installation.linked/suspended/deleted`, and a counter on token-mint failures.

## 13. Out of scope (v1)

Fork-PR review; true in-flight k8s-Job cancellation; webhook delivery
reconciliation/redelivery sweep; GitLab; a budget-exhaustion PR comment;
per-repo agent overrides (installation-level agent only).

## 14. Implementation slices (for the plan) — re-cut per review

1. **App identity & setup:** `githubapp` package (JWT + per-repo token mint/cache/
   retry), `github_app_config` + dedicated master key, hardened manifest flow,
   `github_app_installation` + `installation` lifecycle, **OAuth-verified linking
   (C1 belongs here, not later)**.
2. **Webhook trigger → machine review:** `/api/v1/github/webhook` (verify +
   delivery-id + head-sha dedupe, **in this slice**), `github_installation_context`
   DEFINER, app-backed `repo_connector` (`type='github_app'` + `Resolve` token
   mint), enqueue under the agent principal.
3. **Policy & lifecycle:** filter matrix + `no-manyforge-review` (webhook +
   claim-time recheck), per-installation budget + concurrency/rate caps, supersede
   (pending DEFINER + status guards + `supersede_requested` best-effort skip-post),
   observability events.
