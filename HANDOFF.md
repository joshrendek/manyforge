# Handoff — manyforge @ master — 2026-07-03 ~14:00 UTC

## ⚠️ Before you clear
- **Unpushed:** none — on **master**, `HEAD == origin/master` (`8ffb570`). All work pushed.
- **Uncommitted:** none code (only stray untracked `*.png`/`.pair/`/scattered `CLAUDE.md`s + two untracked `docs/superpowers/plans/*.md` predating this work).
- **PR #8 MERGED** ✅ (merge commit `8ffb570`) — Spec 008 multi-dimension code review (Slice 1+2), cost fix, the two P2 cloud-path bug fixes, the PR-review blockers, a11y, and test-coverage. 008 branch deleted. **Single branch = master.**
- **Still running:** air backend **:8081** (`/tmp/mf-air.log`), ng frontend **:4300** (`/tmp/mf-web.log`), Docker `mf-dev` :55432 + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` = clean final (max_tokens=32000, no debug).

## State
Spec 008 is fully landed on master. This session: root-caused + fixed & LIVE-verified the two P2 cloud bugs (6h1, 2s1), then triaged PR #8's 56 review threads — fixed the real blockers (per-dimension provider misroute, empty-review-on-all-skipped), did the frontend a11y pass (0h0, browser-verified), added test coverage (vay), de-flaked a theme e2e — then merged. **Next: #2 cloud-review streaming (NOT started).**

## Resume here → #2 cloud streaming (design CORRECTED — see wrinkle)
Goal: cloud/OpenRouter reviews should stream live tool-call narration into the UI heartbeat like the local path (currently cloud shows `progress {tokens:0, preview:""}` the whole run).
**⚠️ Design wrinkle (invalidates the old handoff's plan):** the entrypoint redirects opencode stderr to a FILE — `deploy/sandbox/entrypoint.sh:153` `... 2> /out/stderr.log`. So `cmd.Stderr` on the host (docker CLI stderr) receives NOTHING from opencode. The old "MultiWriter on cmd.Stderr" alone streams nothing. **Corrected design:**
1. `entrypoint.sh`: drop `2> /out/stderr.log` so opencode stderr flows to the container's stderr (→ docker → host `cmd.Stderr`). (stdout still → `/out/review.json`; the two fds are independent, review.json stays clean.) Needs image rebuild.
2. `sandbox/docker.go`: add `StreamStderr io.Writer` to `SandboxSpec`; `cmd.Stderr = io.MultiWriter(&stderr, spec.StreamStderr)` when set. `res.Stderr` already buffers it.
3. `service.go` `reviewLane`: pass a secret-scrubbing writer (scrub `laneCred.APIKey`, `rc.Credential.APIToken`) as `spec.StreamStderr` that pushes lines to `prog.UpdateStream(counter, preview)` — the heartbeat the UI already polls (see `progress.go` `UpdateStream`, and `localreview.go:443` for the local pattern). Token counts still finalize at end from usage.json.
4. `sandboxStderrTail` (service.go): change to read from `res.Stderr` instead of `/out/stderr.log` (the file no longer exists). Pass `res.Stderr` into it.
5. Tests: unit — a fake StreamStderr receives bytes; docker.go MultiWriter wiring. Verify: needs ONE paid live cloud review (~$0.75, Acme 6-lane) to SEE streaming in the UI (browser). Cheaper: single-lane (narrow Acme panel to 1 dim, re-enable after — see below).
No frontend change (UI already renders `progress.preview`).

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
