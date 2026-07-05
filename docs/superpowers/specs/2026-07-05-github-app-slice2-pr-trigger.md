# Design: Slice 2 — `pull_request` → auto-triggered code review (GitHub App)

**Status:** design (pre-plan) · **Date:** 2026-07-05 · **Builds on:** Slice 1 (`2026-07-05-github-app-auto-review-design.md`, merged as spec 009)

## 1. Goal & context

When a GitHub pull request is opened/pushed/reopened on a linked installation, automatically enqueue a manyforge code review — reusing the entire existing async pipeline. Slice 1 delivered App identity, config, the signature-verified webhook (installation events only), install→business+agent mapping, and OAuth-verified linking. Slice 2 adds the **`pull_request` trigger + App-token auth**.

**Verified seams (grounding investigation):**
- `runJob` (`internal/agents/coding/service.go:250-618`) resolves the connector, then uses `rc.Credential.APIToken` for **both** clone (`github.BasicAuthHeader`, L349) and post (factory `client.token` → `PostReview`, L618). **If `RepoConnectorService.Resolve` returns a minted installation token as `Credential.APIToken` for a `type='github_app'` connector, `runJob` needs ZERO changes** — the entire app-token seam is inside `Resolve`.
- GitHub App installation tokens (`ghs_…`) use the identical `x-access-token:<token>` basic-auth for clone and `Bearer <token>` for REST — no `github/client.go`/`factory.go` change.
- `code_review.repo_connector_id` is `NOT NULL` (FK). D6 (Slice-1) holds: reuse the schema unchanged **iff** a `repo_connector` row always exists for the review. So Slice 2 auto-creates an **app-backed** `repo_connector`.
- Machine identity: each agent already owns a `kind='agent'` principal with a `membership` (agent.go), so `authorized_businesses(agentPrincipal)` grants its business — enqueuing/resolving under `agent.principal_id` works with no new grant (Slice-1 M1).
- `golang-jwt/v5` supports RS256; the App private key is already unsealed by `ConfigStore.Get` (`config_store.go`).
- Delivery-dedup template: `connector_webhook_delivery` + `ingest_connector_webhook` (migrations 0041/0042) — an atomic DEFINER that dedups a delivery id and enqueues in one shot, principal-less.

## 2. Locked decisions
| # | Decision | Choice |
|---|----------|--------|
| S2-A | App-triggered auth | Per-repo, short-TTL **installation token** (App JWT RS256 → `/access_tokens` with `{"repositories":[name]}`), minted in `Resolve`, never stored |
| S2-B | Review→pipeline attach | **App-backed `repo_connector`** (`type='github_app'`, auto-created per repo; D6) — pipeline unchanged |
| S2-C | Machine identity | The review **agent's own principal** (`agent.principal_id`) |
| S2-D | Filtering (this slice) | **Skip drafts, bot authors, and fork PRs**; trigger actions `opened`/`synchronize`/`reopened`/`ready_for_review` |
| S2-E | Dedup | `X-GitHub-Delivery` replay dedup + same-`(repo,pr,head_sha)` already-reviewed skip |
| S2-F | Deferred to Slice 3 | Budget/rate caps, `no-manyforge-review` opt-out label, **supersede** (cancel in-flight on new push), fork-PR review |

## 3. Architecture flow

```
GitHub pull_request ──▶ POST /api/v1/github/webhook (Slice-1 route)
   │ verify X-Hub-Signature-256 (Slice 1) · target-id check
   │ NEW: X-GitHub-Delivery dedup (github_webhook_delivery)
   │ NEW: parse PR (repo, number, head.sha, draft, sender/user bot, head.repo.id vs base.repo.id)
   │ NEW: filter — skip draft / bot / fork; action ∈ {opened,synchronize,reopened,ready_for_review}
   │ NEW: github_installation_context(installation_id)  [DEFINER] → business, tenant_root, agent_id, agent_principal_id, enabled, suspended, config
   │ NEW: github_enqueue_pr_review(...)  [DEFINER, atomic, principal-less]:
   │        ensure app-backed repo_connector for (business, repo)  ·  same-(repo,pr,head_sha) skip  ·  INSERT code_review(pending, principal_id=agent_principal, repo_connector_id, agent_id, pr_number, head_sha)
   ▼  (unchanged)
claim_code_reviews → runJob → Repos.Resolve(github_app connector) MINTS per-repo installation token
   → clone (KubeRunner) → opencode/GLM → PostReview (same token)
```

## 4. Components & data model

### 4.1 App JWT + installation-token minting (new `internal/githubapp/apptoken.go` + `client.go` method)
- `AppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error)`: RS256, `iss=appID`, `iat=now-60s` (clock-skew), `exp=now+9m` (≤10m); `jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))`, `jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)`.
- `Client.MintInstallationToken(ctx, installationID int64, appJWT, repo string) (token string, expiresAt time.Time, err error)`: `POST {APIBase}/app/installations/{id}/access_tokens` with `Authorization: Bearer <appJWT>`, body `{"repositories":["<name>"]}` (`repo` is `owner/name`; send only `name`). Reuses `Client.do` (strips upstream bodies).
- `type InstallationTokenSource struct { Store *ConfigStore; API tokenMinter; Now func() time.Time; cache … }` with `Token(ctx, installationID int64, repo string) (string, error)`: builds the App JWT from `Store.Get()`, mints, **caches per `(installationID, repo)` keyed on `expiresAt-5m`**, retry-once on a 401. `MintInstallationToken` + `AppJWT` are the `tokenMinter` interface so tests fake them.

### 4.2 App-backed `repo_connector` (migration + `repo_service.go`)
- Migration: relax `repo_connector_type_chk` to `type IN ('github','github_app')`; make `secret_ref` **nullable** with a CHECK: `(type='github' AND secret_ref IS NOT NULL) OR (type='github_app' AND secret_ref IS NULL AND config ? 'installation_id')` (avoids a placeholder secret); add `CREATE UNIQUE INDEX … ON repo_connector (business_id, repo) WHERE type='github_app'`.
- `RepoConnectorService.Resolve` branch (after loading `row`, before `Vault.Open`): for `row.Type=='github_app'`, read `installation_id` from `row.Config`, mint via the injected `InstallationTokenSource.Token(installationID, row.Repo)`, return `ResolvedRepoConnector{…, Credential: {APIToken: token}}`. Add `"github_app": true` to `knownRepoConnectorTypes`. `RepoConnectorService` gains an injected `Tokens *githubapp.InstallationTokenSource`.
- Ensure-connector is done inside the enqueue DEFINER (§4.4), not `Create` (which requires a PAT).

### 4.3 `github_installation_context(p_installation_id bigint)` DEFINER (migration)
Mirrors `connector_webhook_context` (0043). Returns `(business_id, tenant_root_id, agent_id, agent_principal_id, enabled, suspended, config)` by joining `github_app_installation` → `agent` (`a.principal_id`), `WHERE installation_id=$1 AND deleted_at IS NULL`. `REVOKE ALL FROM PUBLIC` + `GRANT EXECUTE TO manyforge_app`. Lets the principal-less webhook resolve a linked (or as-yet-unlinked→null business) installation.

### 4.4 `github_webhook_delivery` + `github_enqueue_pr_review` DEFINER (migration)
- `github_webhook_delivery(id, installation_id bigint, external_delivery_id text, received_at, UNIQUE(installation_id, external_delivery_id))` — tenantless (installation is the key pre-link), TTL-pruned later.
- `github_ingest_delivery(p_installation_id bigint, p_delivery_id text) RETURNS boolean` DEFINER: `INSERT … ON CONFLICT DO NOTHING`; rows-affected=0 → replay (false). Called first in the PR handler.
- `github_enqueue_pr_review(p_installation_id bigint, p_business_id uuid, p_tenant_root uuid, p_agent_id uuid, p_agent_principal uuid, p_repo text, p_pr_number int, p_head_sha text) RETURNS uuid` DEFINER, atomic, principal-less:
  1. **Ensure connector:** `INSERT INTO repo_connector (id, business_id, tenant_root_id, type, repo, config, secret_ref, status) VALUES (gen_random_uuid(), …, 'github_app', p_repo, jsonb_build_object('installation_id', p_installation_id), NULL, 'enabled') ON CONFLICT (business_id, repo) WHERE type='github_app' DO NOTHING`; then `SELECT id INTO v_conn FROM repo_connector WHERE business_id=p_business_id AND repo=p_repo AND type='github_app'`.
  2. **Same-head skip:** `IF EXISTS (SELECT 1 FROM code_review WHERE repo_connector_id=v_conn AND pr_number=p_pr_number AND head_sha=p_head_sha AND status <> 'failed') THEN RETURN NULL; END IF;`
  3. **Insert review:** `INSERT INTO code_review (id, business_id, tenant_root_id, repo_connector_id, pr_number, head_sha, status, principal_id, agent_id, model, created_at, updated_at) VALUES (gen_random_uuid(), p_business_id, p_tenant_root, v_conn, p_pr_number, p_head_sha, 'pending', p_agent_principal, p_agent_id, '', now(), now()) RETURNING id`.
  - `head_sha` is stamped at enqueue here (Slice 1 wrote it at finalize; the column exists). `model` is left `''` at enqueue — `runJob` re-resolves the model via `Creds.Resolve(job.PrincipalID, …, job.AgentID)` under the agent principal (as it already does), so no app-side model lookup is needed on the principal-less webhook path. `REVOKE/GRANT` as usual.
- This mirrors `ingest_connector_webhook`: one atomic DEFINER does dedup + enqueue with no `WithPrincipal` round-trip (there is no logged-in caller).

### 4.5 `webhook.go` `pull_request` handler
In `handleWebhook`, add `else if event=="pull_request" { h.handlePullRequestEvent(r, body) }` (after sig + target-id). `handlePullRequestEvent`:
1. Parse `pullRequestEvent` (Action, Installation.ID, Number, PullRequest.{Draft, User.Type, Head.SHA, Head.Repo.ID}, Base.Repo.ID via PullRequest.Base.Repo.ID, Repository.FullName, Sender.Type).
2. `if action ∉ {opened,synchronize,reopened,ready_for_review}` → 202 (ignore).
3. `github_ingest_delivery(installation_id, X-GitHub-Delivery)` → replay → 202.
4. **Filters:** skip if `PullRequest.Draft`; skip if `Sender.Type=='Bot'` or `PullRequest.User.Type=='Bot'`; skip if `PullRequest.Head.Repo.ID != PullRequest.Base.Repo.ID` (fork) or head repo null. Each skip logs a structured reason → 202.
5. `github_installation_context(installation_id)` → if no row / `business_id IS NULL` (unlinked) / `agent_id IS NULL` / `!enabled` / `suspended` → 202 (log). 
6. `github_enqueue_pr_review(installation_id, business_id, tenant_root, agent_id, agent_principal_id, repo, number, head_sha)` → returns review id (or null on same-head skip). 202.
Uniform 202 throughout (no oracle). The webhook handler gains one injected dep — a `prReviewEnqueuer` wrapping the `github_installation_context` + `github_ingest_delivery` + `github_enqueue_pr_review` DEFINER calls (raw pgx via `WithTx`, no `WithPrincipal` — the DEFINERs bypass RLS).

### 4.6 Wiring
`InstallationTokenSource` (Store + client + cache) built in `main.go` under the App master key; injected into `RepoConnectorService.Tokens` (for `Resolve`) and into the webhook handler's `prReviewEnqueuer` is not needed for tokens (minting happens later in `Resolve`). The webhook handler gets a `prReviewEnqueuer` (raw-pgx wrapper over `github_installation_context` + `github_ingest_delivery` + `github_enqueue_pr_review`). No model resolution on the webhook path — `runJob` handles it under the agent principal.

## 5. Filters (Slice 2)
From the `pull_request` payload, non-configurable this slice (per-installation `config` overrides are Slice 3): skip `draft`; skip bot (`sender.type=='Bot'` || `pull_request.user.type=='Bot'`); skip fork (`head.repo.id != base.repo.id`). Actions handled: `opened`, `synchronize`, `reopened`, `ready_for_review`. Everything else → 202 ignore.

## 6. Security
- Per-repo, short-TTL installation tokens minted at resolve time, never stored in the queue row or connector (secret_ref is NULL for app-backed).
- App JWT signed with the sealed App private key (`ConfigStore`), 9-min exp, 60-sec backdated iat.
- Fork PRs skipped (untrusted code never reaches the sandbox this slice).
- Machine identity is the agent's existing least-privilege principal; the enqueue path is a principal-less DEFINER exactly like `ingest_connector_webhook`, never bypassing tenancy (business/agent come from the signature-verified installation mapping, not the payload).
- Delivery-id + same-head dedup prevent replay/duplicate billing.
- KubeRunner sandbox isolation + egress allowlist unchanged.

## 7. Testing
- **Unit:** `AppJWT` (iat backdate, exp bound, RS256 parses a fake key); `MintInstallationToken` + `InstallationTokenSource` cache/TTL/401-retry against an `httptest` fake; `pullRequestEvent` parse; the filter matrix (draft/bot/fork/action) using table tests; `Resolve` github_app branch returns the minted token as `Credential.APIToken` (fake token source).
- **Integration (real Postgres):** signed `pull_request` delivery → app-backed `repo_connector` auto-created → pending `code_review` under the agent principal → review runs via `FakeRunner` + a fake token source → `PostReview`; delivery-id replay → single enqueue; same-`(repo,pr,head_sha)` → no second review; `github_installation_context` resolves business/agent/principal; the machine review resolves + runs under `agent.principal_id` (RLS).
- **Security-regression pins:** installation-token request carries `"repositories"` (per-repo scope); fork/bot/draft skip; the new DEFINERs carry `REVOKE ALL FROM PUBLIC` + `GRANT EXECUTE TO manyforge_app`; app-backed connector `secret_ref IS NULL` invariant.

## 8. Out of scope (Slice 3)
Budget/rate caps; `no-manyforge-review` opt-out label + `labeled`/`unlabeled` events; **supersede** (cancel a pending/running review for the same PR on a new push — needs the pending-only DEFINER + status guards from the Slice-1 design §9); fork-PR review; installation-token pagination past 100; per-installation filter config; nonce/delivery pruning sweeps.

## 9. Implementation slices (for the plan)
1. **App-token auth:** `AppJWT` + `MintInstallationToken` + `InstallationTokenSource` (mint/cache/retry) + unit tests.
2. **App-backed connector:** migration (type_chk + nullable secret_ref + unique index) + `Resolve` github_app branch (mint via token source) + tests (incl. `runJob` unchanged).
3. **Installation-context + enqueue DEFINERs:** `github_installation_context`, `github_webhook_delivery` + `github_ingest_delivery`, `github_enqueue_pr_review` (ensure-connector + same-head skip + insert) migration + a thin service wrapper + integration tests.
4. **`pull_request` webhook handler:** parse + delivery-dedup + filter matrix + context-resolve + enqueue + wiring in `main.go`; unit + integration (webhook→pending row→FakeRunner review) + security pins.
