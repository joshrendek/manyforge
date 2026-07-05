# Design: Slice 2 — `pull_request` → auto-triggered code review (GitHub App) — v2

**Status:** design (pre-plan, fable-reviewed) · **Date:** 2026-07-05 · **Builds on:** Slice 1 (spec 009)

## 1. Goal & context

Auto-enqueue a manyforge code review when a pull request is opened/pushed/reopened on a linked GitHub App installation, reusing the existing async pipeline. Slice 1 (merged, live) built App identity/config, the signature-verified `installation` webhook, install→business+agent mapping, and OAuth-verified linking. Slice 2 adds the **`pull_request` trigger + App-token auth**.

**Verified seams (fable-confirmed against the code):**
- `runJob` (`service.go:250-618`) uses `rc.Credential.APIToken` for both clone (`BasicAuthHeader` `x-access-token:` basic, L349) and post (factory `Bearer`, L618); the factory never inspects `rc.Type`/`secret_ref`. A `ghs_…` installation token plugs in identically. **The one change in `runJob`:** after `Repos.Resolve`, for a `type='github_app'` connector, **mint** the per-repo token (outside the DB tx) and set `rc.Credential.APIToken` — minting is NOT done inside `Resolve` (that would make `Get`/`Enqueue`'s ownership pre-flights do a GitHub network call inside a `WithPrincipal` transaction, M2).
- Machine identity: each agent already owns a `kind='agent'` principal with a `membership` (agent.go:213-231), so `authorized_businesses(agent_principal)` includes its business — enqueue/resolve under `agent.principal_id` works with no new grant (M1-verified). `model=''` at enqueue is execution-safe (`claim_code_reviews` claims on `(status,run_after)`; `runJob` re-resolves the model via `Creds.Resolve`).
- `secret_ref` nullable vs the composite FK is sound (`MATCH SIMPLE` skips FK on NULL); the two-arm CHECK + partial unique index + `ON CONFLICT … WHERE type='github_app'` are valid.
- App JWT (RS256, iat−60s, exp+9m) with `ParseRSAPrivateKeyFromPEM(cfg.PrivateKeyPEM)`; per-repo `{"repositories":["<name>"]}`; Slice-1's manifest already subscribes `pull_request` with `contents:read`+`pull_requests:write` — no App reconfig.

## 2. Locked decisions
| # | Decision | Choice |
|---|----------|--------|
| S2-A | App-triggered auth | Per-repo, short-TTL **installation token** minted in `runJob` (outside the DB tx), **no cache** (mint fresh 60-min token per review; only caller is `runJob`, ~once/review) — M1 |
| S2-B | Review→pipeline attach | **App-backed `repo_connector`** (`type='github_app'`, auto-created per repo; nullable `secret_ref`) |
| S2-C | Machine identity | The review **agent's own principal** (`agent.principal_id`) |
| S2-D | Filtering | Skip drafts, **bot-authored** PRs (`pull_request.user.type=='Bot'` — author-only), and fork PRs (`head.repo.id != base.repo.id`); actions `opened`/`synchronize`/`reopened`/`ready_for_review` |
| S2-E | Dedup + supersede | `X-GitHub-Delivery` replay dedup; **pending-supersede** (a new push marks same-PR pending reviews `superseded`) + **claim-time same-head re-check** (skip posting if the current head already has a succeeded review) |
| S2-F | Rate cap | **Crude per-installation hourly cap** in the enqueue DEFINER (skip+log over N/hour) |
| S2-G | Auto-connector UX | Badge `type='github_app'` connectors as auto-managed; **block manual delete** |
| S2-H | Deferred to Slice 3 | Full budget/cost accounting, `no-manyforge-review` opt-out label, per-installation filter config, fork-PR review, token pagination >100 |

## 3. Architecture flow
```
GitHub pull_request ──▶ POST /api/v1/github/webhook  (Slice-1 route: verify sig + target-id)
  │ parse PR (action, installation.id, repo, number, head.sha, draft, user.type[bot], head.repo.id vs base.repo.id[fork])
  │ action ∉ {opened,synchronize,reopened,ready_for_review} → 202
  │ FILTER: draft | bot-author | fork → 202 (log reason; delivery id NOT consumed)
  │ github_installation_context(installation.id) [DEFINER] → business, tenant_root, agent_id, agent_principal, agent_enabled, enabled, suspended
  │     unlinked (business NULL) | !enabled | suspended | agent NULL | !agent_enabled → 202 (log; delivery id NOT consumed → redeliverable)
  │ github_pr_review_ingest(installation.id, X-GitHub-Delivery, business, tenant_root, agent_id, agent_principal, repo, number, head.sha) [DEFINER, ONE atomic tx]:
  │     ① delivery dedup (ON CONFLICT DO NOTHING) → replay → return NULL(replay)
  │     ② hourly rate cap: count code_review for this connector in last 1h ≥ CAP → return NULL(rate)
  │     ③ ensure app-backed repo_connector (business,repo) [ON CONFLICT … WHERE type='github_app'; SELECT … FOR UPDATE]
  │     ④ same-head skip: succeeded/pending review exists for (conn,pr,head_sha) → return NULL(dup)
  │     ⑤ pending-supersede: UPDATE code_review SET status='superseded' WHERE conn,pr, status='pending'
  │     ⑥ INSERT code_review(pending, principal_id=agent_principal, agent_id, conn, pr, head_sha, model='')  RETURNING id
  ▼  (unchanged pipeline)
claim_code_reviews → runJob:
     Repos.Resolve(conn) → metadata (no mint)  ·  MINT per-repo installation token (outside tx) → rc.Credential.APIToken
     EGRESS pre-flight (isLocalProvider || EgressAllow.Allows(cred.Host()))  [M5 — also fixes manual path]
     FetchPR(current head) → CLAIM-TIME re-check: succeeded review exists for (conn,pr,current head) → skip post
     clone (KubeRunner) → opencode/GLM → PostReview (same token)
```

## 4. Components & data model

### 4.1 App JWT + installation-token minting (new `internal/githubapp/apptoken.go` + `client.go` method)
- `AppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error)` — RS256, `iss=appID`, `iat=now-60s`, `exp=now+9m`.
- `Client.MintInstallationToken(ctx, installationID int64, appJWT, repoFullName string) (token string, expiresAt time.Time, err error)` — `POST /app/installations/{id}/access_tokens`, `Authorization: Bearer <appJWT>`, body `{"repositories":["<name>"]}` (bare name from `owner/name`). Reuses `Client.do` (strips upstream bodies). A `401/403/404` from mint is a **terminal** (non-retried) failure class (m5).
- `type InstallationTokenSource struct { Store *ConfigStore; API tokenMinter; Now func() time.Time }` with `Token(ctx, installationID int64, repoFullName string) (string, error)` — builds the App JWT from `Store.Get()`, mints a fresh 60-min token (**no cache**, M1). `tokenMinter` interface (`MintInstallationToken`) is faked in tests.

### 4.2 App-backed `repo_connector` (migration + `repo_service.go`)
- Migration: `repo_connector_type_chk` → `type IN ('github','github_app')`; `ALTER COLUMN secret_ref DROP NOT NULL` + CHECK `(type='github' AND secret_ref IS NOT NULL) OR (type='github_app' AND secret_ref IS NULL AND config ? 'installation_id')`; `CREATE UNIQUE INDEX … ON repo_connector (business_id, repo) WHERE type='github_app'`. **sqlc ripple (m2):** `secret_ref` becomes `pgtype.UUID` in dbgen → mechanical edits in `Create`/`Resolve`; regenerate with the pinned sqlc v1.27.0; update any `security_regression` source pins touching those lines.
- `RepoConnectorService.Resolve`: for `type='github_app'`, return metadata (`Config` carries `installation_id`) with `Credential.APIToken=""` — **does NOT mint** (M2). The existing `github` path (`Vault.Open`) is unchanged; NULL `secret_ref` never reaches `Vault.Open`.
- `List`: badge `type='github_app'` rows as auto-managed (a flag in the summary DTO). `Delete`: reject `type='github_app'` with `ErrValidation` ("auto-managed by the GitHub App install") — never manual-delete (S2-G); also fixes the FK-500/resurrect bug. `installation_id` decoded from `config` as a typed int64 (json.Number/explicit cast, not bare `float64`) (m8).

### 4.3 `github_installation_context(p_installation_id bigint)` DEFINER (migration)
Mirrors `connector_webhook_context` (0043). `LEFT JOIN agent a ON a.id = gi.agent_id` (agents are hard-deleted, no FK — m3); returns `(business_id, tenant_root_id, agent_id, agent_principal_id, agent_enabled, enabled, suspended, config)`, `WHERE installation_id=$1 AND deleted_at IS NULL`. `REVOKE ALL FROM PUBLIC` + `GRANT EXECUTE TO manyforge_app`.

### 4.4 `github_webhook_delivery` + `github_pr_review_ingest` DEFINER (migration)
- `github_webhook_delivery(id, installation_id bigint, external_delivery_id text, received_at, UNIQUE(installation_id, external_delivery_id))`; empty delivery id → the handler synthesizes one / skips dedup (m6).
- **`github_pr_review_ingest(p_installation_id bigint, p_delivery_id text, p_business_id uuid, p_tenant_root uuid, p_agent_id uuid, p_agent_principal uuid, p_repo text, p_pr_number int, p_head_sha text) RETURNS uuid`** — ONE atomic DEFINER (fixes C2 — dedup+enqueue in one tx like `ingest_connector_webhook`):
  1. **Delivery dedup:** `INSERT INTO github_webhook_delivery … ON CONFLICT DO NOTHING`; `ROW_COUNT=0` → `RETURN NULL` (replay).
  2. **Rate cap (S2-F):** `IF (SELECT count(*) FROM code_review cr JOIN repo_connector rc ON rc.id=cr.repo_connector_id WHERE rc.type='github_app' AND (rc.config->>'installation_id')::bigint = p_installation_id AND cr.created_at > now()-interval '1 hour') >= <CAP> THEN RETURN NULL; END IF;`
  3. **Ensure connector (C1 — all NOT NULL cols):** `INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url, repo, allow_private_base_url, config, secret_ref, status, created_at, updated_at) VALUES (gen_random_uuid(), p_business_id, p_tenant_root, 'github_app', p_repo, 'https://api.github.com', p_repo, false, jsonb_build_object('installation_id', p_installation_id), NULL, 'enabled', now(), now()) ON CONFLICT (business_id, repo) WHERE type='github_app' DO NOTHING;` then `SELECT id INTO v_conn FROM repo_connector WHERE business_id=p_business_id AND repo=p_repo AND type='github_app' FOR UPDATE;` (the `FOR UPDATE` serializes concurrent same-PR events → race-free, M3).
  4. **Same-head skip:** `IF EXISTS (SELECT 1 FROM code_review WHERE repo_connector_id=v_conn AND pr_number=p_pr_number AND head_sha=p_head_sha AND status IN ('pending','running','succeeded')) THEN RETURN NULL; END IF;`
  5. **Pending-supersede (S2-E):** `UPDATE code_review SET status='superseded', updated_at=now() WHERE repo_connector_id=v_conn AND pr_number=p_pr_number AND status='pending';`
  6. **Insert:** `INSERT INTO code_review (…, status, principal_id, agent_id, model, …) VALUES (…, 'pending', p_agent_principal, p_agent_id, '', …) RETURNING id`.
  `REVOKE ALL FROM PUBLIC` + `GRANT EXECUTE TO manyforge_app`. `code_review_status_chk` extended to include `'superseded'`. `requeue_code_review`/`fail_code_review` gain a `WHERE status='running'` guard (so a superseded row can't be resurrected — Slice-1 §9).

### 4.5 `webhook.go` `pull_request` handler
`handleWebhook`: `else if event=="pull_request" { h.handlePullRequestEvent(r, body) }` (after sig + target-id). Handler:
1. Parse `pullRequestEvent`.
2. `action ∉ trigger set` → 202.
3. **Filters** (before any DB write, no delivery consumption): `draft` | `user.type=='Bot'` (author-only, S2-D) | `head.repo==null || head.repo.id != base.repo.id` (fork) → 202 (log reason).
4. `github_installation_context(installation.id)` → unlinked/disabled/suspended/no-agent/disabled-agent → 202 (log; **delivery id not consumed** → redeliverable, C2).
5. `github_pr_review_ingest(installation.id, X-GitHub-Delivery, …, head.sha)` → review id or NULL (replay/rate/dup) → 202.
Uniform 202 (no oracle). One injected dep: a `prReviewEnqueuer` (raw-pgx `WithTx` wrapper over the two DEFINERs — DEFINERs bypass RLS, no `WithPrincipal`).

### 4.6 `runJob` changes (`service.go`) — M2 + M5 + claim-time re-check
After `Repos.Resolve` (L250): `if rc.Type=='github_app' { tok, err := s.Tokens.Token(ctx, installationID(rc), rc.Repo); … rc.Credential.APIToken = tok }` — minted **outside** the tx. Then the **egress pre-flight** (M5): `if !isLocalProvider(cred) && !s.EgressAllow.Allows(cred.Host()) { terminal-fail }` (also fixes the manual path). After `FetchPR` (current head): **claim-time re-check** — `if a succeeded code_review exists for (rc.ID, prNumber, pr.HeadSHA) other than this row → finalize as skipped (don't PostReview)`. `s.Tokens *githubapp.InstallationTokenSource` + `s.EgressAllow` injected into `CodeReviewService`.

### 4.7 Model + finalize (m1)
`UpdateCodeReviewResult` (`code_review.sql`) gains a `model = $n` column write so the resolved model is stamped at finalize (app reviews enqueue with `model=''`). Add a claim-time-skip terminal status write path (the re-check finalizes the row as `succeeded` with an empty/"superseded by newer head" note, or `superseded`).

## 5. Filters (Slice 2)
Non-configurable this slice: skip `draft`; skip bot-**authored** (`pull_request.user.type=='Bot'` — dependabot/renovate; does NOT skip human PRs a merge-bot pushed to, S2-D); skip fork (`head.repo.id != base.repo.id`, null-safe). Actions: `opened`/`synchronize`/`reopened`/`ready_for_review`.

## 6. Security
- Per-repo, short-TTL installation tokens minted at run time (outside any DB tx), never stored (app-backed `secret_ref` is NULL).
- App JWT signed with the sealed private key (`ConfigStore`), 9-min exp, 60-sec backdated iat; mint failures on dead/suspended installs are terminal (no retry storm, m5).
- Fork PRs skipped (untrusted code never enters the sandbox this slice).
- Machine identity = the agent's existing least-priv principal; enqueue is a principal-less DEFINER (business/agent from the signature-verified installation mapping, never the payload).
- Delivery-id + same-head dedup + pending-supersede + hourly rate cap bound replay/duplicate/runaway billing.
- `runJob` egress pre-flight (M5) fails fast instead of launching doomed sandboxes.
- KubeRunner sandbox isolation + egress allowlist unchanged.

## 7. Testing
- **Unit:** `AppJWT` (iat/exp/RS256 parse); `MintInstallationToken` + `InstallationTokenSource` (fresh-mint, no-cache, `{"repositories"}` scope, 401→terminal) against `httptest`; `pullRequestEvent` parse; filter matrix (draft/bot-author/fork/action); `Resolve` github_app returns metadata + empty token (no mint); `runJob` mint+egress+claim-time-recheck branches (fake token source + FakeRunner); `Delete` rejects github_app.
- **Integration (real Postgres):** signed `pull_request` → app-backed connector auto-created (all NOT NULL cols) → pending `code_review` under agent principal → review via `FakeRunner` → `PostReview`; delivery-id replay → single enqueue (atomic); same-`(repo,pr,head_sha)` → no second review; pending-supersede (2nd push supersedes 1st pending); claim-time re-check (no double-post of same head); hourly rate cap; `github_installation_context` resolves (LEFT JOIN, disabled-agent skip); machine review runs under `agent.principal_id`.
- **Security-regression pins:** mint request carries `"repositories"`; fork/bot/draft skip; new DEFINERs `REVOKE ALL FROM PUBLIC`+`GRANT EXECUTE`; app-backed `secret_ref IS NULL` invariant; `Delete` blocks github_app; egress pre-flight in `runJob`.

## 8. Out of scope (Slice 3)
Full budget/cost accounting (beyond the crude hourly cap); `no-manyforge-review` opt-out label + `labeled`/`unlabeled` events; per-installation filter config; fork-PR review; installation-token pagination >100; delivery/nonce pruning sweeps; re-mint-before-PostReview for pathological multi-lane local reviews.

## 9. Implementation slices (for the plan)
1. **App-token auth:** `AppJWT` + `MintInstallationToken` + `InstallationTokenSource` (fresh mint, terminal-on-401) + unit tests.
2. **App-backed connector:** migration (type_chk + nullable secret_ref + CHECK + partial unique index) + `Resolve` metadata-only github_app branch + `List` badge + `Delete` block + sqlc `pgtype.UUID` ripple + tests.
3. **DEFINERs:** `github_installation_context` (LEFT JOIN, agent_enabled), `github_webhook_delivery`, `github_pr_review_ingest` (atomic dedup + rate cap + ensure-connector + same-head + pending-supersede + insert), `code_review_status_chk` += 'superseded' + `requeue/fail` status guards + a thin `prReviewEnqueuer` + integration tests.
4. **Webhook handler + runJob:** `handlePullRequestEvent` (parse + filter + context + ingest) + `runJob` (mint outside tx + egress pre-flight + claim-time re-check + finalize model stamp) + `main.go` wiring; unit + integration (webhook→pending→FakeRunner review) + security pins.
