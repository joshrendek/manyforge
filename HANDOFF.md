# Handoff — manyforge @ feat/code-review-ui — 2026-06-28 ~20:00 UTC

## ⚠️ Before you clear
- **Uncommitted:** none code — only pre-existing untracked files (`.beads/CLAUDE.md`,
  `.pair/`, screenshots, stray `CLAUDE.md`). (`.beads/issues.jsonl` is committed below.)
- **Unpushed:** none — `feat/code-review-ui` is up to date with origin (PR #6 updated).
- **Still running (survive this session):** air dev server on **:8081** (`/tmp/mf-air.log`);
  **Ollama** on **:11434** (qwen2.5-coder 7b/14b/32b, gemma3:12b); Docker (Colima):
  `mf-dev` + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt this session.

## State (≤3 sentences)
**`manyforge-fqo` is COMPLETE and CLOSED** — all of the code-review-quality follow-ups shipped on
`feat/code-review-ui` (PR #6): **#1 diff-based review** (annotated hunks on local + cloud,
`268fe0d..165cfba`), **#2 secret redaction** (`redactSecrets`/`redactDoc` at the stderr-tail +
model-doc trust boundaries, `MF007-PIN-11`), and **#3 provider generality** (cloud path now
`openrouter|anthropic|openai` via `LLM_PROVIDER` + a generalized entrypoint), `dd69291..c1156e1`.
Both efforts passed an opus whole-branch review = **SHIP**; the full gate (unit, vet, lint,
contract, sec-test, coding integration) is green; the sandbox image is rebuilt.

## Resume here
PR #6 is ready to **merge** at your call (squash recommended — it now carries slice-2 + #1 + #2/#3).
The only thing NOT done is the **GitHub-posting live dogfood**: the dev DB was reseeded (the old
agent/connector are gone; `agent`/`repo_connector`/`business` tables are EMPTY), so posting a real
review to PR #6 needs a re-seed (`cmd/seeddemo`) + an ollama agent + a repo connector with a real
GitHub PAT, then `POST /api/v1/businesses/{id}/code-reviews {agent_id,repo_connector_id,pr_number:6}`.
(The #1 path was already dogfooded locally against PR #6's real diff — see git history.)

## Run & verify
- Tests (all green): `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`;
  `make lint`; `make sec-test`; integration `go test -tags integration -p 1 ./internal/agents/coding/`;
  drift `go test -tags contract ./cmd/...`.
- **Rebuild sandbox image (after any `entrypoint.sh` change):**
  `DOCKER_DEFAULT_PLATFORM=linux/arm64 DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
  — the `DOCKER_DEFAULT_PLATFORM` is REQUIRED on this host (see gotcha).
- SDD ledgers: `.superpowers/sdd/progress.md` (this = redaction/provider) + `progress-dbr-archive.md` (#1).

## Gotchas (don't relearn these)
- **Sandbox image rebuild needs `DOCKER_DEFAULT_PLATFORM=linux/arm64`.** The cached `alpine:3.20`
  is **amd64**, so without it the base resolves amd64, `TARGETARCH` defaults wrong, and the build
  fails at `RUN opencode --version` (`opencode: not found` — wrong-arch binary). With it, it builds.
- **gopls diagnostics lie after edits** — phantom `undefined …` / `dbgen.* undefined` /
  `too many arguments` are STALE. `go build`/`go test`/`go vet` is the only truth.
- **`make lint` tail can hide the golangci result** — `golangci-lint run ./...` directly to confirm.
- **3 review-prompt copies stay in sync:** `localreview.go`, `deploy/sandbox/entrypoint.sh`,
  `tools/local-model-eval/run.sh`.
- **`CreateCodeReviewAgentRun` needs a REAL agent** (INSERT…SELECT FROM agent → 0 rows for a
  foreign agent). Integration tests must seed via `seed.agentID`, not `uuid.New()` (see `f821a9a`).
- **GitHub token is base64-wrapped** in the clone BasicAuthHeader → outside `redactSecrets`'s exact
  match, but clone errors never echo it (documented in `redact.go`).

## Decisions & rationale
- **Redaction at two trust boundaries** (sandbox stderr tail → `last_error`/audit; model doc →
  posted body + stored row), exact known-value + specific key-pattern regex. Opus verified every
  OTHER error path can't carry the raw secret.
- **Provider generality** is openrouter/anthropic/openai only (cloud); ollama/vllm stay host-side.
  No `credresolver` default for openai — it already requires a user-supplied base_url (`ai/factory.go`).

## Next steps
1. Merge PR #6 (user's call).
2. (Optional) GitHub-posting dogfood — needs re-seed + a connector PAT (see Resume here).
3. `manyforge-206`: gemini-2.5-pro 504 on large diffs (now mitigated by smaller hunk payloads; chunk if still needed).

## Pointers
- **Specs/plans:** `docs/superpowers/specs/2026-06-28-{diff-based-code-review,review-redaction-provider-generality}-design.md`
  and the matching `docs/superpowers/plans/2026-06-28-*`.
- **Key files (#2/#3):** `internal/agents/coding/{redact.go,service.go}` (redaction + `sandboxEnv`),
  `deploy/sandbox/entrypoint.sh` (provider allowlist), `internal/security_regression/mf007_review_redaction_test.go`.
- **bd:** `manyforge-fqo` CLOSED; epic `manyforge-7ml`. `manyforge-206` open.
- **PR #6** (slice-2 + #1 + #2/#3) → master.
