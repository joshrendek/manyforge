# Handoff — manyforge @ master — 2026-06-20 ~21:00 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence. Untracked noise only (`.pair/`, `*.png` screenshots, scattered `CLAUDE.md` files) — pre-existing, ignore.
- **Unpushed:** none. `master == origin/master == 2ddd75b`.
- **Still running:** DB **:55432** (ssh tunnel, dev DB **@ migration 71**) · Go backend `manyforge` **:8081** (air) · Angular `ng serve` **:4300**. (Bring the sandbox feature up: build the two images — see below — before triggering a real review.)

## State (≤3 sentences)
Shipped **Spec 007 slice 1 — read-only code-review agent (opencode)**: a GitHub `repo_connector` (vault-sealed PAT), and a `CodeReviewService` that clones a PR head **on the host**, runs **opencode read-only in an ephemeral credential-free Docker sandbox** (only the LLM key inside; egress forced through an allowlisting CONNECT proxy on an `--internal` docker network), validates structured findings, and posts **one PR review automatically** (advisory → ungated). Built subagent-driven over 16 TDD tasks (Task 11 skipped by design — no `agent_run` row; `code_review` is its own lifecycle envelope), all gates green (`make test`/`make lint`/contract/`sec-test` + sandbox-isolation & e2e integration), merged to master and pushed. bd **`zcq` closed**; 3 follow-ups filed.

## Resume here
No half-done work on 007 slice 1. The highest-value next step is **validating the REAL opencode invocation end-to-end** (bd follow-up): automated tests use a busybox **stub** sandbox image; the real opencode contract (its `opencode.json` permission keys, `{env:LLM_MODEL}` interpolation, the v0.0.55 pinned binary) is **unverified**. Run `specs/007-coding-review-agents/quickstart.md` against a throwaway repo + real GitHub PAT + real LLM key, then fix `deploy/sandbox/{Dockerfile,opencode.json,entrypoint.sh}` + the service `Env` mapping to opencode's actual contract. Otherwise pick from `bd ready` (epic `7ml` next slices: PR authoring, GitLab, webhook auto-trigger, k8s sandbox backend).

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `make test` · `make lint` · `go test -tags contract ./cmd/...` · `make sec-test` · integration `go test -tags integration -p 1 ./internal/<pkg>/...` (Docker). sqlc = the **v1.27.0 bottle** `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (global v1.31.1 re-churns).
- **Sandbox images** (needed for a real review, not for tests): `docker build -f deploy/egress-proxy/Dockerfile -t manyforge/egress-proxy:dev .` and `docker build -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
- **Dev DB** DSN `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable` (migration **71**).

## Gotchas (don't relearn these)
- **Colima sandbox mounts:** only `/Users/...` is mirrored into the Docker VM. `CodeReviewService.WorkRoot` defaults to `$HOME/.cache/manyforge/sandbox` (NOT `/tmp`) for exactly this reason; integration tests use `$HOME/.cache/...` temp dirs, not `t.TempDir()` (=/tmp).
- **Egress allowlist is BOOT-STATIC** (`MANYFORGE_SANDBOX_EGRESS_ALLOW`, default `api.anthropic.com,openrouter.ai,api.openai.com`). Per-run `spec.EgressAllow` is currently ignored → a review agent on a *custom* provider host is **silently egress-blocked**. Tracked as a bd bug follow-up; fix before supporting custom providers.
- **Sandbox isolation test** (`internal/agents/coding/sandbox/`, `-tags integration`) builds the proxy+stub images itself and takes ~50s; it's the security gate (no-ambient-creds / read-only `/work` / egress-blocked / `/out` writable) — don't weaken its assertions.
- **Host-side clone is hardened** (`internal/agents/coding/clone.go`): token via `-c http.extraHeader` with `http.followRedirects=false` + `credential.helper=` (no redirect token-leak), a `netsafe.IsBlocked` pre-clone DNS check (SSRF), and a minimal `Cmd.Env` (no inherited git config/helpers). Pinned by `TestCloneHardeningPinned`. Don't regress these.
- **Subagent worktree footgun:** one implementer subagent committed into a `.claude/worktrees/agent-*` branch instead of the working branch; caught via the post-task `git worktree list` check and `--ff-only` merged back. Tell implementers to commit on the current branch and verify after each task.
- **gopls "undefined dbgen method"** after sqlc regen is stale tooling — `go build` is truth.

## Decisions & rationale
- **Direct orchestration, NOT the LLM `Engine.Run` loop** (user-approved): opencode is the only reviewing LLM; wrapping it in a second manyforge LLM is redundant. `code_review.status` is the lifecycle; `agent_run_id` is a nullable forward-seam (no `agent_run` row this slice).
- **Review posting is ungated** (advisory; changes no code) — pinned; the autonomy gate is about *mutating the codebase*, not external-vs-internal.
- **Sandbox holds no repo credential**: ManyForge clones on the host; the sandbox gets only the LLM key + egress to the LLM host. Makes the isolation pin trivially true.
- **Permissions reuse existing keys**: repo-connector create → `connectors.manage`; code-review trigger/get → `agents.run` (no new perm/migration this slice).

## Next steps
1. `bd ready` / the 3 new follow-ups: **validate real opencode e2e** (P2), **fix boot-static egress vs per-run allow** (P2 bug), **slice-1 polish** (P3). 2. Then next 007 slice (PR authoring on the proven sandbox, or GitLab / webhook trigger). 3. SDD scratch + reviews are under `.superpowers/sdd/` (git-ignored).

## Pointers
- **Spec/plan:** `specs/007-coding-review-agents/{spec.md,quickstart.md,contracts/openapi.yaml}` · `docs/superpowers/plans/2026-06-20-007-code-review-agent.md`.
- **bd:** `zcq` closed; epic `7ml`; 3 open follow-ups (egress bug, opencode-e2e, polish).
- **Key files:** `internal/connectors/{repo_connector.go,repo_service.go,github/}` · `internal/agents/coding/{service.go,clone.go,findings.go,credresolver.go,handler.go,sandbox/}` · `cmd/mf-egress-proxy/` · `deploy/{egress-proxy,sandbox,sandbox-stub}/` · migrations `0070`/`0071` · pins `internal/security_regression/coding_review_pins_test.go` · drift `cmd/manyforge/drift_007_test.go`.
- Resume: `/handoff resume`.
