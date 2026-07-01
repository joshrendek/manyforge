# Handoff — manyforge @ 008-review-dimensions — 2026-07-01 ~19:15 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of this session's code — all pushed. Working tree has only stray untracked docs (scattered `CLAUDE.md`s, `.pair/`, screenshots). **Unpushed:** none (`008-review-dimensions` tracks origin).
- **PR #7 is MERGED** to master (squash `8da6007`); the old `fix/code-review-fallback-model` branch is deleted. We are on a fresh **`008-review-dimensions`** branch off master (one branch off master — compliant).
- **Still running:** air **:8081**, ng **:4300**, Ollama **:11434**, LM Studio **192.168.2.241:1234** (external), Docker `mf-dev` :55432 + `mf-egress-proxy`.

## State (≤3 sentences)
Spec **008 — Multi-dimension Code Review** is greenlit and in flight. It turns a review from *one agent, one prompt* into a **panel of per-dimension reviewers** (security/correctness/performance/UI/docs/tests), each with its own model + prompt + file-scope + severity; a review fans out across the in-scope dimensions, dedupes, optionally verifies, and posts one review tagged by dimension. **Slice 1 part 1 is done** (commit `5a2c34e`): the fully-unit-tested **dimension engine** — nothing wires it yet, so zero behavior change.

## Resume here — finish Slice 1 (`manyforge-v9c`), backend core
The engine (`internal/agents/coding/dimensions.go`, 6 tests green) exists but is unwired. Do these in order, each with tests, keeping the single-agent path working:
1. **Prompt plumbing** — parameterize the review prompt: `localReview`/`streamLocalReview` take a `prompt` arg (used instead of the `reviewInstructions` const); the sandbox path writes the *dimension's* prompt to `/out/review_instructions.txt` (already the runtime seam, MF007-PIN-13). Default = `reviewInstructions`. Update the existing localreview tests to pass it.
2. **`defaultPanel()`** — returns a SINGLE "general" dimension using `reviewInstructions` (all files, min info). **This is the zero-config default → NO cost/latency regression** (do NOT auto-enable all 6 specialists by default; that would 5× everyone's review). The `dimensionCatalog()` specialists are opt-in via Slice 2 presets.
3. **Fan-out in `runJob`** (`service.go` ~348–433, the delicate part I hardened all session — go carefully): resolve the panel → `activeDimensions(panel, changedFilePaths)` → for each active dim run the local/cloud pass with its prompt+model (empty ⇒ the resolved `cred`) against the scope-filtered payload → tag findings with `dim.Key` → `applySeverityFloor` → collect. Then `dedupeFindings` → post ONE review → finalize with **summed** tokens/cost + a `dimension_runs` record (model/tokens/cost/status/skipped). Partial success: one lane failing ≠ whole review fails (FR-013); all fail ⇒ fail.
4. **Migrations 0077–0079** + sqlc: `review_dimension`, `review_config` (both RLS by business_id/tenant_root_id — mirror `ai_provider_credential`/migration 0025), `code_review.dimension_runs jsonb`. A resolver returns configured rows or `defaultPanel()` when none.
5. **Tests**: fan-out integration (fake connector + fake local endpoint returning per-dim findings → one aggregated review; partial-success; none-in-scope) + **MF008-PIN-1** (RLS cross-tenant) / **PIN-2** (per-dim SSRF guard not bypassed) / **PIN-3** (prompt-per-dimension plumbed) in `internal/security_regression/`.

## Run & verify
- Gates: `go build ./...`; `make lint` (= `go vet ./...`, NOT staticcheck); `go test ./internal/agents/coding/ ./internal/connectors/ ./internal/security_regression/`; `go test -tags contract ./cmd/...`; `make sec-test`. **NO Co-Authored-By** (user rule).
- Live review (still works): login `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` → trigger `POST :8081/api/v1/businesses/7bbeb32e-.../code-reviews {agent_id,repo_connector_id,pr_number}`. Only OPEN PR is **joshrendek/manyforge #7** (now merged — reviewing a merged PR fails "not open"; open a new PR or reopen one to dogfood).

## Gotchas (don't relearn these)
- **Don't 5× review cost by default.** Zero-config panel = one general lane (`defaultPanel`), not all specialists. Specialists are opt-in (Slice 2 presets/UI). Local providers are single-GPU → fan-out is sequential and each lane is minutes.
- **Scope globs use doublestar** (`matchGlob`): `**/*.go` = any-depth; bare `*.go` = top-level only (correct glob semantics). Defaults use `**/`.
- **The runJob local/cloud paths** are freshly hardened (spec 007 + manyforge-87a): reasoning models stream `reasoning_content` (preview-only), LM Studio empty-under-json_schema → plain + in-line retry, `max_tokens=8192` bound, per-dimension prompt must flow through BOTH paths. [[manyforge-sandbox-dev-gotchas]]
- **zsh `noclobber`** → bg redirects use `>|`. [[user-zsh-noclobber-bg-logs]] · **sqlc PINNED v1.27.0** (`/opt/homebrew/bin/sqlc`) [[sqlc-version-pin-v127]] · editing `.go` restarts air mid-review → park orphans.
- **Next migration is 0077.** RLS pattern: mirror `migrations/0025_ai_provider_credential.up.sql`.

## Decisions & rationale
- **Inline per-dimension config** (not agent-bound): each dimension holds its own provider+model+prompt+scope+severity, resolving the credential by provider — no forcing agent pre-creation. (User pick.)
- **Deterministic glob routing** (not an LLM router) for v1; risk-tier triage deferred to Slice 4.
- **Backward-compatible default** (one general lane) so Slice 1 ships the fan-out machinery with no cost/behavior regression; value lands when users configure the panel (Slice 2).

## Pointers
- **Spec/plan:** `specs/008-review-dimensions/{spec.md,plan.md}` (greenlit, commit `95f4bf4`). **Foundation:** spec 007 (`internal/agents/coding`).
- **bd:** epic `manyforge-t2s`; slices `manyforge-v9c` (Slice 1, in progress — see its notes for the remaining checklist), `puh` (Slice 2 config UI, blocked by v9c), `8qs` (Slice 3 quality), `e54` (Slice 4 later). Also open: `manyforge-byz` (clear-progress = succeeded-only now), `manyforge-5tr` (Ollama num_ctx).
- **Key new files:** `internal/agents/coding/dimensions.go` (+ `_test.go`). **Key edit targets next:** `localreview.go`, `service.go` (runJob), `migrations/0077+`, `internal/security_regression/`.
- **Dev entities (business Acme `7bbeb32e-7c98-4c8f-966b-70acdb440dce`):** LM Studio agent(vllm) `2571c371` + cred `ca7b0b97` (192.168.2.241:1234, allow_private=true); connectors joshrendek/manyforge `eb68939b`.
