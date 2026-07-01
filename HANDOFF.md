# Handoff — manyforge @ fix/code-review-fallback-model — 2026-07-01 ~16:30 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of this session's code — commit `c2602f1` is pushed (HEAD == origin). Only stray untracked docs remain (scattered `CLAUDE.md`s, `.pair/`, screenshots — not this session's, not code). **Unpushed:** none.
- **Still running (survive the clear):** air **:8081** (`/tmp/mf-air.log`), ng serve **:4300** (`/tmp/mf-web.log`), **Ollama :11434**, **LM Studio at 192.168.2.241:1234** (external box; OpenAI-compatible, 142k ctx, no key), Docker `mf-dev` Postgres **:55432** + `mf-egress-proxy`.

## State (≤3 sentences)
The local self-host reviewer now **works end-to-end**: commit `c2602f1` (PR #7) makes local reviews reach a **private-LAN** model (LM Studio), handles reasoning models + LM Studio's json_schema quirk, bounds runaway output, and **keeps a failed review's captured output visible** (the user's explicit ask). **Verified live:** a `vllm` agent pointed at LM Studio ornith-1.0-9b reviewed PR #7 to **`succeeded` with 7 real findings about the actual code** (localreview.go / entrypoint.sh / code_review.sql — no hallucinated paths); the failed-output UI was browser-checked. All gates green; nothing in flight.

## Resume here — this unit of work is DONE
No next step required for manyforge-5ai (closed). If continuing Spec 007, pick a follow-up (see Pointers). If merging: PR #7 is large but complete — `gh pr view 7`.

## Run & verify
- **Stack is up.** Restart if needed: air `set -a; . ./.air.env; set +a; nohup air >| /tmp/mf-air.log 2>&1 & disown` (`curl :8081/healthz`); ng `cd web && nohup npx ng serve --proxy-config proxy.conf.json --port 4300 --host 127.0.0.1 >| /tmp/mf-web.log 2>&1 & disown`.
- Login (fresh JWT each air restart): `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` → `access_token`. Trigger: `POST :8081/api/v1/businesses/7bbeb32e-7c98-4c8f-966b-70acdb440dce/code-reviews {agent_id,repo_connector_id,pr_number}`; poll `GET …/code-reviews/{id}`.
- Gates (all green as of `c2602f1`): `go build ./...`; `make lint`; `go test ./internal/agents/coding/ ./internal/platform/netsafe/ ./internal/security_regression/`; `go test -tags contract ./cmd/...`; `make sec-test`; `cd web && npx ng test --include='**/code-review/**/*.spec.ts' --watch=false`. **NO Co-Authored-By trailer** (user rule).
- Browser-verify UI: inject JWT into `localStorage['mf_access']`, then `/code-review/{bizId}/{reviewId}` (Playwright MCP or gstack).

## Gotchas (don't relearn these)
- **ornith-1.0-9b is a REASONING model** → streams chain-of-thought in `delta.reasoning_content`, final answer in `delta.content`. localReview shows reasoning in the preview but only parses `content`. [[manyforge-sandbox-dev-gotchas]]
- **LM Studio returns EMPTY under `response_format=json_schema`** (Ollama NEEDS it) → localReview falls back to plain. LM Studio's **plain** path is non-deterministic and sometimes emits malformed JSON (unescaped quotes from a code snippet) → localReview now retries plain IN-LINE with a temperature bump (`manyforge-87a`, done: PR#7 succeeds on worker attempt 1). Structured output (json_schema/json_object) is unusable — the reasoning model emits reasoning but empty content under a schema.
- **Credentials resolve per-provider.** The `ollama` cred is `localhost`; the LM Studio agent uses the **`vllm`** provider slot (its own cred `ca7b0b97`, `allow_private_base_url=true`). Both route local via `isLocalProvider`.
- **Local-review SSRF guard is the INVERSE of netsafe**: netsafe permits public IPs; local review must BLOCK public (it bypasses the egress proxy). Guard allows loopback always + private only with `AllowPrivateBaseURL`; metadata/link-local/public blocked. MF007-PIN-14 pins it.
- **zsh `noclobber`** → bg log redirects use `>|` not `>`. [[user-zsh-noclobber-bg-logs]]
- **Editing a `.go` file restarts air mid-review** and orphans the in-flight job → park it (`UPDATE code_review SET status='failed',lease_expires_at=NULL WHERE id=…`). Large PRs on a 9B reasoning model can run minutes; `max_tokens=8192` now bounds it.
- **Stale gopls** after edits — `go build`/`test` is truth. [[gopls-stale-dbgen-diagnostics]] · **sqlc PINNED v1.27.0** [[sqlc-version-pin-v127]]

## Decisions & rationale
- **Reuse the existing `AllowPrivateBaseURL` trust flag + netsafe** rather than hardcoding RFC1918 in `isLoopbackHost` — consistent with `validateBaseURL` (create-time) + `clone.checkCloneURL`, DNS-rebind-aware for literal IPs, backward-compatible (existing `localhost` cred already had the flag).
- **Failed reviews retain `progress`** (already-redacted preview) and the UI shows it — the raw `last_error` stays server-side (can carry provider/sandbox internals). This narrows `manyforge-byz` ("clear progress on terminal") to **succeeded only**.
- **`max_tokens=8192`** bounds a reasoning model's output so an attempt fails fast+visibly instead of pinning the worker/GPU for minutes.

## Pointers
- **PR:** #7 OPEN → master. **Commits:** `c2602f1` (local self-host reviewer) + `a5d2c44` (manyforge-87a in-line retry). **bd:** epic `manyforge-7ml` (reopened); done: `manyforge-5ai`, `manyforge-87a`. Open follow-ups: `manyforge-byz` (P3 clear-progress = succeeded-only now), `manyforge-5tr` (P2 Ollama num_ctx), `manyforge-lyv`/`bbi` (P3 polish).
- **Key files:** `internal/agents/coding/{localreview.go, credresolver.go, worker.go, findings.go, progress.go, service.go}`; `web/src/app/pages/code-review/detail.ts`; `internal/security_regression/coding_review_pins_test.go` (MF007 pins incl. PIN-14).
- **Dev entities (business Acme `7bbeb32e-7c98-4c8f-966b-70acdb440dce`):** agents — LM Studio ornith(vllm) `2571c371`, ornith:9b(ollama) `4232e921`, qwen2.5-coder:14b(ollama) `6aeb7a46`, ReviewBot(openrouter z-ai/glm-5.2) `6c252395`; creds — vllm(LM Studio) `ca7b0b97`, ollama(localhost) `4431d2f2`, openrouter `fb0993e2`; connectors — joshrendek/manyforge `eb68939b`, bluescripts-net/threat.gg `3d944fdc` (PR #22 is CLOSED). Superuser DB: `docker exec mf-dev psql -U manyforge -d manyforge`.
- **Screenshot:** `code-review-failed-output-retained.png` (failed-review output-retention, browser-verified).
