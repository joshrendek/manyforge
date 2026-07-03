# Handoff — manyforge @ 008-review-dimensions — 2026-07-03 ~02:20 UTC

## ⚠️ Before you clear
- **Unpushed:** none — `HEAD == origin/008-review-dimensions` (`c941b88`). All work pushed.
- **Uncommitted:** none code (working tree clean apart from bd's export churn + stray untracked `*.png`/`.pair/`/scattered `CLAUDE.md`s + two untracked `docs/superpowers/plans/*.md` that predate this session).
- **PR #8 OPEN:** `008-review-dimensions` → `master`. Now carries Slice 1 + Slice 2 + cost fix + **the two P2 cloud-path bug fixes (6h1, 2s1)**. MERGEABLE.
- **Still running:** air backend **:8081** (`/tmp/mf-air.log`), ng frontend **:4300** (`/tmp/mf-web.log`), Docker `mf-dev` :55432 + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt clean (final entrypoint, max_tokens=32000, **no debug instrumentation**).

## State
Spec **008 — Multi-dimension Code Review**. Slice 1 (`v9c`) + Slice 2 (`puh`) + cost fix (`d2bf8a2`) complete. **This session fixed + LIVE-verified both remaining P2 cloud-path bugs (`manyforge-6h1`, `manyforge-2s1`) — both CLOSED.** Commits `c0e969b` (code) + `c941b88` (bd).

## 6h1 + 2s1 — DONE + verified live (`c0e969b`)
Root-caused via a live instrumented repro on Acme's 6-lane glm-5.2 panel (PR #8). Both are distinct failures on the same pathologically-heavy `correctness`/`ui` lanes (300k+ input tokens, `cache_read`≈0):
- **2s1 (failed lane loses cost):** a heavy lane hits the **5-min sandbox timeout** → killed before opencode writes `usage.json` → `readSandboxUsage` empty → lane records all-zero cost AND tokens. **Compound leak:** `docker.go` `exec.CommandContext` killed the docker CLI but NOT the daemon container → orphan seen "Up 10m" past a 5m cap, still calling OpenRouter. Fixes: `docker.go` names + reaps the container on ctx timeout/cancel (`+TestRunReapsContainerOnTimeout`); `main.go` timeout 5m→**8m**; `service.go`/`dimensions.go` persist a client-safe `FailReason` → `dimension_runs.last_error` + log full err server-side (closes the observability gap — a partial-success lane's error was silently dropped).
- **6h1 (truncated JSON):** custom OpenRouter slug has no catalog output limit → opencode's small default cap → glm-5.2's ~9k reasoning tokens exhaust the shared completion budget → JSON truncated. **Proven live:** `max_tokens=50` → `reasoning=50/output=1` (empty); `32000` → complete. Fixes: `entrypoint.sh` sets `provider.<p>.models.<slug>.options.max_tokens=32000`; `service.go` retries a clean-exit parse/empty failure ONCE (mirrors local path), summing usage across attempts.

## Resume here → #2 streaming (design ready, decided Option B) OR Slice 3
**#2 (`manyforge`… streaming):** make cloud reviews stream live progress like the local path. Design (unchanged, still valid): add `StreamStderr io.Writer` to `SandboxSpec`; in `sandbox/docker.go` `Run`, `cmd.Stderr = io.MultiWriter(&stderr, spec.StreamStderr)` when set; `reviewLane` passes a secret-scrubbing writer pushing opencode's live stderr (tool-call narration) into `prog.UpdateStream` — the heartbeat the UI already polls. Token counts still finalize at end. No frontend change. NOTE: cloud progress currently shows `tokens:0, preview:""` the whole run (this is exactly #2).

## Also open
- **`manyforge-1s9` (P2, NEW):** opencode does ~no prompt caching for glm-5.2/OpenRouter (`cache_read`≈0) → lanes 5-10× heavier/slower → the deeper driver behind the timeouts. Intermittent (one earlier run showed 205k cache_read). Likely opencode/provider-side; mitigated (not root-fixed) by the 8m timeout + retry.
- **`manyforge-8qs` (P3):** Slice 3 — verify pass + rule citations + cost estimate.

## Run & verify
- Backend: `go build ./...`; `make lint`; `go test ./internal/agents/coding/ ./internal/connectors/`; `go test -tags contract ./cmd/...`; `make sec-test`. NEW integration tests: `go test -tags integration -run 'TestRunReapsContainerOnTimeout' ./internal/agents/coding/sandbox/` and `-run 'TestCodeReviewTrigger/cloud_lane_retries' ./internal/agents/coding/`. (All GREEN this session.)
- **Live cloud-review repro:** login `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` (token TTL 900s — re-login before reading results); `POST /api/v1/businesses/7bbeb32e-…/code-reviews {"agent_id":"6c252395-… (glm-5.2)","repo_connector_id":"eb68939b-…","pr_number":8}`. Acme = 6-lane panel (~15min, ~$0.75). For a CHEAP single lane: temporarily `UPDATE review_dimension SET enabled=false … WHERE dimension<>'security'` (re-enable after). mf-dev DB: `PGPASSWORD=devpassword psql -h localhost -p55432 -U manyforge -d manyforge`.
- **NO Co-Authored-By** on commits (user rule). Commit style `fix(008): …`.

## Gotchas (don't relearn)
- **A lane fan-out failure is non-deterministic** (model verbosity varies run-to-run) — the failing lane moved correctness→ui between two identical triggers. Don't rely on one run; the timeout edge is ~real for any 300k-token lane.
- Sandbox timeout on macOS **orphaned the container** (docker.go attached-run gotcha) — FIXED now (reap by name), but the pattern is: `exec.CommandContext` kills the CLI, not the daemon container.
- A **partial-success** lane's error text used to be dropped (only `status` in `dimension_runs`) — now in `dimension_runs.last_error` + server log `"code review cloud lane failed"`.
- Instrumenting: comment the cleanup `defer` at `service.go` ~line 320 to preserve `~/.cache/manyforge/sandbox/<crID>/out`; it collides with the worker's job-retry re-clone ("destination path already exists") — restore it when done.
- gopls shows phantom `dbgen` field errors (DimensionRuns/Progress) after editing service.go — stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]
- opencode config knobs: `provider.<p>.models.<slug>.options` → provider SDK (`max_tokens`); `.limit.output` needs `.limit.context` too (avoided). [[manyforge-opencode-sandbox-cost-and-usage]]
- zsh `noclobber`: use `>|` for redirects. [[user-zsh-noclobber-bg-logs]]

## Pointers
- **bd:** epic `manyforge-t2s`; `v9c`+`puh`+**`6h1`+`2s1` CLOSED**; open: `1s9` (caching), `8qs` (Slice 3), `e54` (Slice 4). **PR #8** open.
- **This session's commits:** `c0e969b` (6h1/2s1 fix), `c941b88` (bd close).
- **Key files:** `internal/agents/coding/{service.go,dimensions.go,sandbox/docker.go}`; `cmd/manyforge/main.go`; `deploy/sandbox/entrypoint.sh`.
