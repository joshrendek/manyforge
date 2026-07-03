# Handoff — manyforge @ master — 2026-07-03 ~18:30 UTC

## ⚠️ Before you clear
- **Unpushed:** none. On **master**, up to date with origin. Single local branch (rule: one branch off master at a time).
- **Uncommitted:** none code (only long-standing untracked `*.png` / `.pair/` worktree / scattered `CLAUDE.md`s that predate this work).
- **PR #9 MERGED** ✅ (squash `6fb7eba` — "feat(008): stream cloud review progress live"). Spec 008 core (#8) + cloud streaming (#9) are both on master.
- **Still running:** air backend on **:8080** (`main` pid listens :8080), Docker `mf-dev` Postgres :55432. (Prior handoff said :8081 — that was stale; it's :8080 now.)

## State
`/handoff resume` this session found PR #9's CI was **red**, not "green" as the prior note claimed: `make test` failed on security pin **MF007-PIN-11** because the #2 streaming commit refactored `sandboxStderrTail(outDir string,…)` → `sandboxStderrTail(stderr []byte,…)` and the source pin literal still matched the old signature. Verified redaction is intact on both client-visible paths (last_error via `redactSecrets`; live `progress.preview` via `Progress.Snapshot`→`redactSecrets` with `SetSecrets` at service.go:272). Updated the pin literal (commit squashed into `6fb7eba`), full local gate green, merged #9 squash + deleted branch.

## Resume here → pick next open work (nothing started)
No work is in flight. Master is the single branch. Choose from `bd ready`. Top candidates:
- **`manyforge-1s9`** (P2, bug): opencode does ~no prompt caching for glm-5.2/OpenRouter (`cache_read`≈0) → lanes 5–10× heavier/slower + timeouts. Likely opencode/provider-side; the deepest driver of slow cloud reviews.
- **`manyforge-ubk`** (P3): wire per-dimension provider (credential resolve + egress allowlist). **Has a security note** (see below) that MUST be honored when implemented.
- **`manyforge-8qs`** (P3): 008 Slice 3 — verify pass + rule citations + cost estimate (owns verify_provider/verify_model validation).
- **`manyforge-byz`** (P3): clear `code_review.progress` on terminal states (succeeded/failed).
- **`manyforge-5tr`** (P2, bug): local Ollama OpenAI-compat ignores `options.num_ctx` → huge-context models unusable.

## Loose ends from this session (not blocking)
- **Stale remote branch `origin/005-crm-contacts-timeline`** is FULLY merged into master (0 ahead). Safe to delete (`git push origin --delete 005-crm-contacts-timeline`); left in place because it wasn't created this session.
- **Latent security gap on `manyforge-ubk`** (noted in its bd `--notes`): `SetSecrets` registers only the PRIMARY cred key. Identical to lane keys today (mismatched lanes are skipped), but when per-dimension providers land, each lane key must be `SetSecrets`-registered before it streams, or it can leak into `progress.preview`. Add an MF007 pin for the per-lane call in the same change.

## Run & verify
- CI gate (`.github/workflows/ci.yml`, one `build-test` job, sequential): `make build` → `make lint` → `make test` → `make contract-test` → `make int-test` (testcontainers, needs Docker). **`make test` includes `internal/security_regression` — run it locally; per-package `go test` misses it.** [[backend-verification-gates-easy-to-miss]]
- Security pins live in `internal/security_regression/mf00*_*_test.go` as source-literal `strings.Contains` checks — a Go signature/literal refactor breaks them; update the pin in the SAME change. [[security-regression-pins-grep-source-literals]]
- Frontend (`web/`): `npx ng test --no-watch` (vitest, NOT `--browsers=`); `npx playwright test` (needs ng dev server).
- Live cloud repro: login `POST :8080/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` (TTL 900s); `POST /api/v1/businesses/<id>/code-reviews {...}`. mf-dev: `PGPASSWORD=devpassword psql -h localhost -p55432 -U manyforge -d manyforge`.
- **NO Co-Authored-By** on commits (user rule). Branch off master (one at a time); `fix(008): …` style.

## Gotchas (don't relearn)
- Prior handoff's "green, ready to merge" was WRONG — always re-check `gh pr checks` on resume; don't trust a note's CI claim.
- opencode stderr now stays on container stderr → `SandboxResult.Stderr` (the #2 redesign); `sandboxStderrTail` reads that `[]byte`, no longer `/out/stderr.log`.
- gopls phantom `dbgen` field errors after editing service.go are stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]
- zsh `noclobber`: use `>|` for bg-log redirects. [[user-zsh-noclobber-bg-logs]]

## Pointers
- **bd:** epics `manyforge-t2s` (008), `manyforge-7ml` (007). This session touched no issue status; added a security note to `manyforge-ubk`.
- **This session's change:** pin fix, squashed into master `6fb7eba` (#9).
- **Key files (#2 streaming):** `internal/agents/coding/{service.go,progress.go,sandbox/docker.go,sandbox/runner.go}`; `deploy/sandbox/entrypoint.sh`; pins in `internal/security_regression/mf007_review_redaction_test.go`.
