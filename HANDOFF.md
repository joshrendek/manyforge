# Handoff — manyforge @ 008-cloud-stream — 2026-07-03 ~14:30 UTC

## ⚠️ Before you clear
- **Unpushed:** none — on branch **008-cloud-stream**, pushed. `master` @ `8ffb570` (has all of Spec 008 via #8).
- **Uncommitted:** none code (only stray untracked `*.png`/`.pair/`/scattered `CLAUDE.md`s + two untracked `docs/superpowers/plans/*.md` predating this work).
- **PR #8 MERGED** ✅ (Spec 008 core + the cloud bug fixes + a11y + tests). **PR #9 OPEN** (008-cloud-stream → master): cloud review streaming, **live-verified**, ready to merge.
- **Still running:** air backend **:8081** (`/tmp/mf-air.log`), ng frontend **:4300** (`/tmp/mf-web.log`), Docker `mf-dev` :55432 + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt (streaming entrypoint: opencode stderr → container stderr).

## State
Spec 008 core is on master. This session also shipped **#2 cloud streaming** (PR #9) — cloud reviews now stream opencode's live tool-call narration into the UI heartbeat like the local path (LIVE-VERIFIED: `progress.preview` streamed 0→1091 bytes during a real review, then succeeded). **Resume: merge PR #9, then pick next from Other open work.**

## Resume here → merge PR #9, then next
PR #9 (008-cloud-stream) is live-verified and green. Merge it (base master, delete branch), then the single branch is master again. Next candidates (Other open work below): none is started.
#2 streaming implementation (all in PR #9, for reference):
- `entrypoint.sh`: opencode stderr left on container stderr (dropped `2> /out/stderr.log`); stdout still → review.json.
- `sandbox`: `SandboxSpec.StreamStderr io.Writer` + `docker.go` `io.MultiWriter` tee. `+TestRunStreamsStderr`.
- `service.go`: `progressStreamWriter` → `prog.UpdateStream`; worker heartbeat persists it. `sandboxStderrTail` reads `res.Stderr`. `+TestProgressStreamWriter`.

## Other open work
- **`manyforge-ubk`** (P3): full per-dimension provider support (credential resolve via `credResolver.Resolve(businessID, provider)` + egress allowlist per provider). Currently mismatched-provider lanes are SKIPPED with a reason (partitionByProvider).
- **`manyforge-1s9`** (P2): opencode does ~no prompt caching for glm-5.2/OpenRouter (`cache_read`≈0) → the deeper driver behind heavy/slow lanes + timeouts. Likely opencode/provider-side.
- **`manyforge-vay`** (P3, partially done): remaining SQL-query/CRUD unit tests are integration-covered; verify-config validation deferred to Slice 3.
- **`manyforge-8qs`** (P3): Slice 3 — verify pass + rule citations + cost estimate (also owns the verify_provider/verify_model validation).

## Run & verify
- Backend: `go build ./...`; `make lint`; `go test ./internal/agents/coding/ ./internal/connectors/`; `go test -tags contract ./cmd/...`; `make sec-test`; integration e.g. `go test -tags integration -run 'TestRunReapsContainerOnTimeout' ./internal/agents/coding/sandbox/`.
- Frontend (`web/`): `npx ng test --no-watch` (277) — vitest, NOT `--browsers=`; `npx playwright test` (71, needs ng :4300).
- Live cloud repro: login `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` (token TTL 900s); `POST /api/v1/businesses/7bbeb32e-…/code-reviews {"agent_id":"6c252395-… glm-5.2","repo_connector_id":"eb68939b-…","pr_number":8}`. Cheap single lane: `UPDATE review_dimension SET enabled=false … WHERE dimension<>'security'` (re-enable after). mf-dev: `PGPASSWORD=devpassword psql -h localhost -p55432 -U manyforge -d manyforge`.
- **NO Co-Authored-By** on commits (user rule). Branch off master (one at a time); `fix(008): …` style.

## Gotchas (don't relearn)
- opencode stderr → `/out/stderr.log` (file), so it never reaches the host `cmd.Stderr` — the crux of the #2 redesign above.
- Sandbox timeout on macOS orphaned the container (attached-run gotcha) — FIXED (reap by name in docker.go).
- A partial-success lane's error is now in `dimension_runs.last_error` (client-safe reason) + server log `"code review cloud lane failed"`.
- Non-deterministic lane failure: model verbosity varies run-to-run; the failing lane moved correctness→ui between identical triggers.
- gopls phantom `dbgen` field errors (DimensionRuns/Progress) after editing service.go — stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]
- vitest for frontend unit tests (`ng test`), NOT Karma; `--browsers=` is invalid.
- zsh `noclobber`: use `>|` for redirects. [[user-zsh-noclobber-bg-logs]]

## Pointers
- **bd:** epic `manyforge-t2s` (008); CLOSED this session: `6h1`, `2s1`, `0h0`. Open: `1s9`, `ubk`, `vay`, `8qs`, `e54`.
- **This session's commits (all on master via #8):** `c0e969b` (6h1/2s1), `ecac038` (provider/empty-review blockers), `3d89728` (a11y), `dbf7fd0` (vay tests), `399ddc4` (theme de-flake), `8ffb570` (merge).
- **Key files for #2:** `internal/agents/coding/{service.go,sandbox/docker.go,sandbox/runner.go,progress.go,localreview.go}`; `deploy/sandbox/entrypoint.sh`.
