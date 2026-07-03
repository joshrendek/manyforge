# Handoff — manyforge @ master — 2026-07-03 ~19:45 UTC

## ⚠️ Before you clear
- **Unpushed:** none (after the pending commit below lands). On **master**, single branch.
- **Uncommitted:** only long-standing untracked `*.png` / `.pair/` worktree / scattered `CLAUDE.md`s (predate this work). No code.
- **Merged this session:** **PR #9** (cloud streaming, squash `6fb7eba`) and **PR #10** (`manyforge-1s9` GLM cache pin, squash `3dbe6e1`). Both CI-green on master.
- **Still running:** manyforge air backend on **:8081** (`tmp/manyforge`, logs → `/tmp/mf-air.log`), ng frontend, Docker `mf-dev` Postgres :55432. NOTE: garden.gg's server is on :8080 — manyforge is :8081. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt with the z-ai pin.

## State
Two things shipped: (1) PR #9's CI was red on security pin MF007-PIN-11 (stale `sandboxStderrTail` signature literal) — fixed + merged. (2) `manyforge-1s9` (GLM/OpenRouter no prompt caching) — root-caused, fixed with a z.ai routing pin, **live-measured positive** (3/3 runs cached 39–89% vs ~0 baseline), merged (#10), issue CLOSED.

## Resume here → pick next open work (nothing started)
Master is the single branch; nothing in flight. Choose from `bd ready`. Candidates:
- **`manyforge-5tr`** (P2 bug): local Ollama OpenAI-compat ignores `options.num_ctx` → huge-context models unusable.
- **`manyforge-ubk`** (P3): wire per-dimension provider (credential + egress). **Has a security note in its bd `--notes`:** `SetSecrets` (service.go:272) registers only the PRIMARY cred key; when per-dimension providers land, each lane key must be `SetSecrets`-registered before it streams or it can leak into `progress.preview`. Add an MF007 pin then.
- **`manyforge-8qs`** (P3): 008 Slice 3 — verify pass + rule citations + cost estimate.
- **`manyforge-byz`** (P3): clear `code_review.progress` on terminal states.

## 1s9 deploy note (don't miss)
The fix is in `deploy/sandbox/entrypoint.sh` (baked into the image). **Prod/other envs need a `manyforge/opencode-sandbox:dev` image rebuild** to pick it up: `DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .` (arm64 dev). Pin is scoped to `z-ai/*`/`*glm*` slugs only.

## How to run a live cloud review (learned this session — the repro that works)
- manyforge API is **:8081** (NOT :8080 = garden.gg). Login: `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` → `access_token` (TTL 900s — re-login right before use).
- Trigger: `POST :8081/api/v1/businesses/7bbeb32e-7c98-4c8f-966b-70acdb440dce/code-reviews {"agent_id":"6c252395-c8a3-4e2b-bc23-12de92308f47","repo_connector_id":"eb68939b-ae44-4276-8340-003a10070a36","pr_number":<N>}` (ReviewBot=GLM/OpenRouter).
- **The PR must be OPEN** — the worker rejects merged/closed PRs ("pull request not open"). Push a branch + open a (draft) PR to get a target.
- **Cheap single lane:** `UPDATE review_dimension SET enabled=(dimension='correctness') WHERE business_id='7bbeb32e-…';` then restore `enabled=true` after.
- Server logs (incl. any temp `slog`) go to `/tmp/mf-air.log`. DB folds `cache_read` into `tokens_in`, so granular cache metrics need a temp `slog` of the `sandboxUsage` fields (Input/CacheRead/CacheWrite) in service.go's lane loop (~line 500). mf-dev: `PGPASSWORD=devpassword psql -h localhost -p55432 -U manyforge -d manyforge`.

## Run & verify
- CI gate (`.github/workflows/ci.yml`, one `build-test` job): `make build`→`make lint`→`make test`→`make contract-test`→`make int-test`. **`make test` includes `internal/security_regression`** — run it locally. [[backend-verification-gates-easy-to-miss]]
- Security pins are source-literal `strings.Contains` checks in `internal/security_regression/mf00*_*_test.go`; a Go signature/literal refactor breaks them — update the pin in the SAME change. [[security-regression-pins-grep-source-literals]]
- **NO Co-Authored-By** on commits. Branch off master (one at a time).

## Gotchas (don't relearn)
- Don't trust a handoff's "CI green" claim — re-check `gh pr checks` on resume (PR #9's note was wrong).
- manyforge = :8081, garden.gg = :8080. Two air processes run; check `cwd`.
- air rebuilds `tmp/manyforge` on `.go` save (delay 500ms); it restarts the process. Verify a rebuild with `strings tmp/manyforge | grep <marker>` — `grep -c` on the binary is unreliable.
- zsh `noclobber`: use `>|` to overwrite files (e.g. offset markers). [[user-zsh-noclobber-bg-logs]]
- gopls phantom `dbgen` field errors (DimensionRuns/Progress) after editing service.go are stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]

## Pointers
- **bd:** epics `manyforge-t2s` (008), `manyforge-7ml` (007). CLOSED this session: `manyforge-1s9`. Security note added to `manyforge-ubk`.
- **This session's master commits:** `6fb7eba` (#9 streaming), the MF007 pin fix (in #9), `f64644a` (handoff), `3dbe6e1` (#10 GLM cache pin).
- **Key files:** `deploy/sandbox/entrypoint.sh` (GLM z-ai pin); `internal/agents/coding/{service.go,progress.go}`; pins in `internal/security_regression/`.
