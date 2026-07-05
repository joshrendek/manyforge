# Handoff — manyforge @ master — 2026-07-05 ~22:15 UTC

## ⚠️ Before you clear
- Uncommitted: none (only untracked noise: screenshots, `.pair/`, stray `CLAUDE.md`s). Unpushed: none — master in sync with origin.
- Still running: nothing I started. (A pre-existing `ng serve` on :4300 from a prior session may linger — not mine, leave it.)
- This `HANDOFF.md` can be `.gitignore`d if you want it local-only.

## State (≤3 sentences)
The **GitHub App auto-review feature is fully built and deployed live** to https://hub.bluescripts.net: Slice 1 (App identity/webhook/OAuth-linking, spec 009), the setup UI (010), and Slice 2 (`pull_request` → auto-review trigger + App-token auth, 011) are all merged to master, deployed (image `main-a0542b9`, pod 1/1), and DB-migrated (schema version 84, clean). The pipeline is complete but **dormant until the operator registers + installs + links the App** (a human step in the browser). Slice 3 is the remaining work.

## Resume here
Two independent paths (pick per user):
1. **Exercise the live feature:** josh logs into https://hub.bluescripts.net/settings/github → "Create GitHub App" (operator gate = his principal `cd28c757-57c6-4674-af89-4623d4fecea4`) → install on `bluescripts-net`/`sysward` → "Connect an organization" (pick a review agent). Then open a PR → it auto-reviews. Nothing in code needs touching for this.
2. **Build Slice 3:** brainstorm → spec → (fable design review) → plan → (fable plan review) → subagent-driven-development → merge → deploy — same pattern as Slices 1/2. Scope: full budget/cost caps, `no-manyforge-review` opt-out label (+ `labeled`/`unlabeled` events), per-install filter config (`github_app_installation.config` jsonb overrides), fork-PR review (threat-model prompt-injection exfil first).

## Run & verify
- Backend tests: `make test`; security pins: `make sec-test`; integration (needs Docker/colima): `go test -tags integration ./...`; contract: `go test -tags contract ./cmd/...`; lint: `golangci-lint run ./...`. `go build ./...` is the source of truth for compile errors.
- sqlc regen: **use v1.27.0** — `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate` (or `make generate` if local sqlc is pinned); a newer version re-churns `dbgen/models.go`. Always mirror new tables/constraints into `db/schema.sql`.
- Deploy path: merge to master → GitHub Actions `images` workflow builds `ghcr.io/joshrendek/manyforge:main-<sha>-<ts>` → Flux image-automation auto-commits the tag to `infra-k8s/flux/clusters/proxmox-talos/manyforge.yaml` → HelmRelease redeploys (~10-15 min). Force: `KUBECONFIG=~/.kube/talos flux reconcile image repository manyforge -n flux-system`.
- Prod DB (external PG 192.168.2.243, non-RLS `account`/`principal` readable): `psql "$(KUBECONFIG=~/.kube/talos kubectl get secret manyforge-database -n manyforge -o jsonpath='{.data.url}' | base64 -d)"`.
- Verify feature mounted: `curl -X POST https://hub.bluescripts.net/api/v1/github/webhook -d '{}'` → 202 (would be 404 if disabled).

## Gotchas (don't relearn these)
- **Stale gopls is constant here:** after edits/regen the editor flags "undefined dbgen method"/type-mismatch/"undefined field" — almost always stale. `go build ./...` is truth; don't chase them.
- **`code_review` finalize NOT-NULL trap:** `UpdateCodeReviewResult` MUST always pass `Findings` AND `DimensionRuns` as `[]byte("[]")` — pgx encodes a nil `[]byte` as SQL NULL → 23502 → the whole tx silently aborts. `fail()` had this latent bug (no-op since 0079); fixed this session. Any new finalize path must set both.
- **Migration downs are never tested** (`testdb` only runs Up) — the 0083 down FK-violated on rollback (fixed; `manyforge-yvy` tracks a CI up→down→up smoke test). Check downs by hand.
- GitHub App is **inert until `MANYFORGE_GITHUB_APP_MASTER_KEY` is set** (it IS in prod, as the `github-app` key in the `manyforge-masterkeys` k8s secret). Webhooks 202 but nothing enqueues until an installation is **linked** (unlinked installs are quarantined by design).
- Installation tokens are minted **fresh per review, no cache** (a cached token can expire mid-job → PostReview 401 → whole run re-bills).
- `zsh noclobber`: `cmd > existing.log` fails; use `>|`. Avoid destructive `rm`.

## Decisions & rationale (not visible in code)
- **App-backed `repo_connector`** (`type='github_app'`, nullable `secret_ref`, `installation_id` in config): keeps the entire `runJob`→clone→PostReview pipeline unchanged; the token is minted in `runJob` (outside any tx) and never stored.
- **Machine identity = the review agent's own `kind='agent'` principal** (it already has a `membership`) — no invented bot principal.
- **Authenticated-completion linking** (in-handler `connectors-manage` check on the signed-state business + GitHub OAuth `GET /user/installations` proof) closes the cross-tenant leaked-`state` hole (fable found it).
- Slice 2 folded basic filters (draft/bot-author/fork) in; budget/label/per-install-config/fork-review deferred to Slice 3 (user decisions).

## Next steps
1. (User) register the App to make auto-reviews actually fire — see Resume #1.
2. Slice 3 — see Resume #2.
3. Follow-ups (all open bd): `manyforge-0w9` (rate-cap config + terminal-on-config-error + Create 400-not-500), `manyforge-yvy` (CI down-migration smoke test), `manyforge-87l` (surface "skipped: already reviewed this head" in review history), `manyforge-3b1` (constrain `github_app_installation.agent_id` on raw updates), `manyforge-g4d` (Slice-1 client OAuth/webhook-lifecycle test coverage).

## Pointers
- Specs: `docs/superpowers/specs/2026-07-05-github-app-auto-review-design.md` (Slice 1), `…-github-app-slice2-pr-trigger.md` (Slice 2 v2). Plans: `docs/superpowers/plans/2026-07-05-github-app-review-slice1.md`, `…-github-app-slice2-plan.md`.
- Feature code: `internal/githubapp/*` (apptoken, client, config_store, prreview, pullrequest, webhook, handler, installations, state, nonce, manifest, link, perms); `internal/connectors/repo_service.go` (github_app branch); `internal/agents/coding/service.go` (`runJob` mint/egress/claim-time-recheck, `fail()`/`finalizeSkipped`); migrations `0080`–`0084`; SPA `web/src/app/pages/settings/github-*.ts` + `core/github-app.service.ts`; `charts/manyforge/{templates/deployment.yaml,templates/configmap.yaml,values.yaml}`; `infra-k8s/flux/clusters/proxmox-talos/manyforge.yaml` (HelmRelease values: `instanceOperatorPrincipal`, `publicBaseURL`).
- Closed (done+deployed): `manyforge-q4h` (Slice 1), `manyforge-doh` (setup UI), `manyforge-qpc` (Slice 2). SDD ledger: `.superpowers/sdd/progress.md`.
- Operator principal: `cd28c757-57c6-4674-af89-4623d4fecea4` · public base URL: `https://hub.bluescripts.net`.
