# Handoff — manyforge @ master — 2026-06-11 UTC

## ⚠️ Before you clear
- **Everything is on `master`.** The entire stack (specs 001–004 + UI Streams 1–2) was fast-forwarded onto `master` (`master` @ `363c318`, pushed). All stacked branches (`001-tenant-foundation`, `002-support-desk`, `003-agent-runtime`, `004-external-connectors`, `ui-redesign`) are **deleted** (local + remote). **Only `master` remains. No open PRs.** Nothing unpushed.
- **NEW workflow rule (now in `CLAUDE.md` → "Branching & Git Workflow" + `bd remember`):** at most **ONE** branch off `master` at a time — **never stack branches**. Branch from master → PR into master → merge → delete → next work branches fresh from master.
- **Uncommitted:** none of consequence (only pre-existing untracked harness artifacts: `.claude/scheduled_tasks.lock`, a stray `docs/superpowers/plans/2026-06-01-us2-reply-threading.md`, and claude-mem `CLAUDE.md` files — leave them).
- **Still running:** dev **Postgres :55432**, **backend `air` :8081**, **frontend `ng serve` :4300**. Stop with `pkill -f bin/air` + `pkill -f 'ng serve --port 4300'` (leave Postgres).

## State (≤3 sentences)
**All delivered work now lives on `master`:** tenant foundation (001), support desk (002), agent runtime (003), external connectors (004), and the UI program's Stream 1 (design system + page migration) and Stream 2 (approvals queue UI + safe action-summary, bd `manyforge-4zs.2`, closed). The previously-stacked per-spec/per-stream branches were collapsed onto `master` and deleted; going forward the repo uses a single-branch-off-master workflow. Latest gate was green end-to-end (backend `make test`+`make sec-test`; frontend 140 Vitest + 42 Playwright) and CI passed on the Stream-2 PR before merge.

## Resume here
No work in flight. Pick the next unit of work, **branch once off `master`**, PR back into `master`. Candidates: `bd ready` for the next issue; the UI program's `manyforge-4zs.3` (connectors UI — full-stack, needs its own brainstorm/spec/plan); or Spec-004 deferred follow-ups under epic `manyforge-a7j` (`a7j.7`–`a7j.12`) if prioritized.

## Run & verify
- **Backend:** `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test && make sec-test` (Docker up; testcontainers may need one retry).
- **Frontend:** `cd web && npm run build && npm test` (Vitest) + `npx playwright test` (needs dev server on :4300).
- **Dev login (real app):** `live-demo@manyforge.test` / `DevPassw0rd!` (owner of Acme Holdings). Migrate as SUPERUSER if the backend refuses to serve (schema < code): `/opt/homebrew/bin/migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`.

## Gotchas (don't relearn these)
- **`master` is unprotected** — direct pushes work, but per the new rule, route real work through a single branch + PR.
- **bd journal auto-rides commits:** `.beads/issues.jsonl` gets auto-staged by bd tooling, so it lands in whatever commit you make after a `bd` mutation even with `git add <specific files>` + `--no-verify`. Either commit bd changes deliberately as a `chore(bd)` (or accept it riding a coherent docs commit), and never `git add -A`.
- **Undefined CSS classes/vars render SILENTLY** (no console error) and the green unit/e2e gate doesn't catch them — always eyes-on both light + dark themes for UI work. Reuse only existing `.mf-*` classes + `--mf-*` tokens (`web/src/styles.css` + `web/src/app/ui/`).
- **Frontend test cmd is `cd web && npm test`** (Vitest via `@angular/build:unit-test`), NOT bare `npx vitest`. `.mf-table`/`.mf-tr` are DIV-flex, not real `<table>`; `mf-select` is a native `<select class="mf-select">` + `[ngModel]` (needs FormsModule).

## Open follow-ups (bd)
- `manyforge-crm` (P4) — Support page should seed `CurrentBusinessService` so the approvals nav badge tracks the viewed business everywhere.
- Epic `manyforge-a7j` deferred items (`a7j.7`–`a7j.12`, + `a7j.4.9`) — Spec-004 connector polish; none blocking.

## Pointers
- **bd:** UI program epic `manyforge-4zs` — `.1` ✓ Stream1, `.2` ✓ Stream2, `.3` ○ connectors. `bd prime` / `bd ready` to resume.
- **Stream 2 artifacts:** `docs/superpowers/specs/2026-06-11-approvals-queue-ui-design.md` + `docs/superpowers/plans/2026-06-11-approvals-queue-ui.md`. Key code: `internal/agents/approval_summary.go`, `web/src/app/pages/approvals/queue.ts`, `web/src/app/core/{approvals,current-business}.service.ts`.
- Resume: `/handoff resume`.
