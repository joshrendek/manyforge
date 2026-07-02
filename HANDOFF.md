# Handoff — manyforge @ 008-review-dimensions — 2026-07-02 ~17:15 UTC

## ⚠️ Before you clear
- **Unpushed:** none — `HEAD == origin/008-review-dimensions` (`d2bf8a2`). All work pushed.
- **Uncommitted:** none code (working tree clean apart from bd's `.beads/issues.jsonl` + stray untracked `*.png`/`.pair/`/scattered `CLAUDE.md`s).
- **PR #8 OPEN:** `008-review-dimensions` → `master` (Slice 1 + Slice 2 + cost fix). MERGEABLE.
- **Still running:** air **:8081** (backend log `/tmp/mf-air.log`), ng serve **:4300** (`/tmp/mf-web.log`), Docker `mf-dev` :55432 + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt (final entrypoint).

## State
Spec **008 — Multi-dimension Code Review**. Slice 1 (`v9c`) + Slice 2 (`puh`) COMPLETE. **This session also fixed + verified the cloud-review cost undercount (`d2bf8a2`).** Then diagnosed (not yet fixed) two more cloud-path bugs and designed the sandbox-streaming feature.

## Cost fix — DONE + verified live (`d2bf8a2`)
Cloud reviews under-billed ~3–4× because the entrypoint captured only input/output/reasoning tokens and the host re-priced them — ignoring **`tokens_cache_read`**, which dominate the agentic loop (opencode re-reads the cached context every tool turn: one lane had 205,696 cache-read vs 9,886 fresh input). sst/opencode already computes the right cost in `session.cost`; the "cost=0 for custom slug" assumption was stale. Fix: `deploy/sandbox/entrypoint.sh` usage.json now SUMs `session.cost` + full token breakdown; `readSandboxUsage`/`costCentsFromUsage` bill from `cost` when >0, catalog fallback otherwise; `TokensIn` includes cache reads. **Verified live:** 5/6 lanes matched opencode's cost to the cent. See [[manyforge-opencode-sandbox-cost-and-usage]].

## Resume here → fix two cloud-path bugs (both need ONE instrumented repro FIRST)
**`manyforge-6h1` (P2) — lanes fail on truncated output.** Confirmed class via review `5c471422`: model emitted a ` ```json ` fence around JSON **truncated mid-object** → `ParseFindings` "empty findings output". Root cause = output TRUNCATION (verbose/reasoning models exceed opencode's output cap). Fix direction: raise opencode's max output tokens, and/or terser JSON-only prompt, and/or a plain-retry like the local path. NOT just fence-stripping (JSON is incomplete).

**`manyforge-2s1` (P2) — a failed lane loses its cost.** correctness lane recorded 0¢ though opencode billed 19¢. Puzzle: `reviewLane` reads usage BEFORE the parse-error return, so a parse failure should PRESERVE cost — yet it was 0, meaning `readSandboxUsage` returned empty at read time despite usage.json having real data post-hoc. Mechanism unconfirmed (bind-mount flush lag? session-write ordering?). **Do NOT guess-fix** (systematic-debugging).

**Repro recipe (both):** the runDir (`$HOME/.cache/manyforge/sandbox/<crID>`) is `os.RemoveAll`'d by a `defer` at service.go:320, and the opencode DB is in the container's ephemeral `/tmp`. To inspect: temporarily (a) instrument `entrypoint.sh` to `cp` the DB + `review.json` to `/out`, (b) comment that cleanup defer, then trigger ONE review and read `$HOME/.cache/manyforge/sandbox/<crID>/out/lane-*/`. Rebuild image: `DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .` (buildx missing → BuildKit off). Trigger via API: login `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` → `POST /api/v1/businesses/7bbeb32e-…/code-reviews {"agent_id":"6c252395-… (openrouter glm-5.2)","repo_connector_id":"eb68939b-…","pr_number":8}`. NOTE: Acme has a 6-lane panel so every review fans out (~15min, ~$0.30-0.70). **Access token expires mid-poll — re-login before reading results.** Clean up after: restore cleanup defer + entrypoint, `rm -rf` the runDir, `docker kill` orphan opencode containers.

## Then → #2 streaming (design ready, decided: Option B)
Make OpenRouter/cloud reviews stream live progress like the local path. Design: add `StreamStderr io.Writer` to `SandboxSpec`; in `sandbox/docker.go` `Run`, `cmd.Stderr = io.MultiWriter(&stderr, spec.StreamStderr)` when set (currently buffers + blocks). `reviewLane` passes a secret-scrubbing writer that pushes opencode's live stderr (tool-call narration: "Read…/Grep…") into `prog.UpdateStream` — the same heartbeat the UI already polls. Token counts still finalize at end (usage.json); only `prog.preview` streams. No frontend change. (#3 — sandbox the local Ollama/vLLM path — remains a later spike.)

## Run & verify
- Backend: `go build ./...`; `make lint`; `go test ./internal/agents/coding/ ./internal/connectors/`; `go test -tags contract ./cmd/...`; `make sec-test`; multidim: `go test -tags integration -run TestCodeReviewMultiDimensionFanout ./internal/agents/coding/`.
- Frontend (`web/`): `npx ng test --no-watch` (277); `npx playwright test` (69, needs ng :4300).
- **NO Co-Authored-By** on commits (user rule). Commit style `fix(008): …`.

## Gotchas (don't relearn)
- opencode is **agentic** (many LLM calls/lane, cache reads dominate) — bill from `session.cost`, not a token subset. [[manyforge-opencode-sandbox-cost-and-usage]]
- `docker.go` `Run` uses `exec.CommandContext` → on timeout it kills the docker CLI but the **container orphans** (attached-run gotcha). Relevant if you touch the sandbox lifecycle.
- gopls shows phantom dbgen errors after editing service.go — stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]
- e2e specs need the shell nav-badge calls mocked or they 401→logout. [[manyforge-e2e-shell-nav-badge-401-logout]]
- zsh `noclobber`: use `>|` for redirects. [[user-zsh-noclobber-bg-logs]]

## Pointers
- **bd:** epic `manyforge-t2s`; `v9c`+`puh` closed; **`6h1`+`2s1` (P2 bugs — NEXT)**; `8qs` (Slice 3); `e54` (Slice 4). **PR #8** open.
- **This session's commits:** `1a652c1` (Phase C), `b59dae1` (Phase D), `ed65aa5` (e2e fix), `9ea76cf` (handoff), `d2bf8a2` (cost fix).
- **Key files:** `internal/agents/coding/{service.go,dimensions.go,panel.go,localreview.go,findings.go,sandbox/docker.go}`; `deploy/sandbox/entrypoint.sh`.
