# Support-Desk Usability — Design

**Date:** 2026-06-02
**Author:** Josh Rendek (with Claude)
**Status:** Approved (pending spec review)
**Builds on:** spec-002 (Native Support Desk), spec-001 (Tenant Foundation)

## Problem

Spec-002 shipped with all tasks ticked and the merge gate green (unit + integration +
contract + lint + 30/30 Playwright). Despite that, the running app is **disjointed and
appears non-functional** when actually used. Driving the real browser produced a precise,
evidence-backed diagnosis that the test suite and a code audit both missed.

### What is NOT broken (verified, do not "fix")

The auth/session/token-refresh layer works correctly on every path. Network trace from the
live app:

```
GET  /api/v1/me            -> 401   (original request, expired access token)
GET  /api/v1/businesses    -> 401   (original request, expired access token)
POST /api/v1/auth/refresh  -> 200   (single-flight refresh; ONE call for both 401s)
GET  /api/v1/me            -> 200   (retried with fresh token)
GET  /api/v1/businesses    -> 200   (retried with fresh token)
```

Paths verified in the browser:
- Valid access token → works.
- Expired access + valid refresh → transparent refresh + retry (above).
- Invalid/expired refresh → `clearSession()` + redirect to `/login` (no stale dashboard).
- No token → `authGuard` redirects to `/login`.

Consequence: **`manyforge-bhg` ("SPA renders stale dashboard on 401") does not reproduce.**
It is reconciled as not-reproducing, not as fixed. The two `401` console entries seen on a
normal load are the browser logging the original pre-retry requests — cosmetic, not a fault.

### What IS broken

1. **Disjointed — no app shell.** `web/src/app/app.html` is a banner + `<router-outlet>`.
   The only navigation is a single "Support" link; every feature (dashboard, ticket list,
   thread view, inbox settings) is a standalone full-width island stitched together with
   per-page "Back to dashboard" links. There is no persistent navigation and no sense of one
   product.

2. **Appears non-functional — the desk is empty.** All four demo businesses
   (Acme Holdings, Engineering, Platform Team, Sales) return `200` with **zero tickets**.
   Logging in and opening Support shows nothing, everywhere. The headline flow
   (inbound email → ticket → thread → reply) has no data to render, so the feature looks
   dead. There is **no seed mechanism in the repo at all** — the demo user/businesses were
   hand-created in local Postgres; `live-demo`/`DevPassw0rd`/`Acme Holdings` appear nowhere
   in source.

3. **Untested interactive flows.** Because the desk is empty, reply / internal note / triage
   (status, priority, assignee, tags) / inbox-settings could not be exercised in the browser.
   Real bugs may hide here; they are the most likely true source of "doesn't function" and
   must be driven and fixed once data exists.

### Why the guardrails missed this

The Playwright e2e suite logs in **fresh** each run (never reaches the 15-minute access-token
expiry) and **seeds its own ticket per test** (never sees the empty desk). A code-reading
audit sees wired components (true) but cannot see session lifecycle, navigation cohesion, or
empty runtime data. Green tasks ≠ usable product.

## Goals

- The app feels like one product: persistent navigation tying the features together.
- The support desk is alive on login: realistic tickets and threads, produced through the
  real ingestion pipeline, repeatably and idempotently.
- Every interactive support flow is driven in a real browser, fixed where broken, and pinned
  with a regression test.

## Non-goals (YAGNI)

- No change to the auth/session layer (it works).
- No in-UI "simulate inbound email" button (user chose seed-only).
- No real email/domain wiring for demo data.
- No bulk actions, saved views, attachment-upload UI, or CRM/requester surfaces (future specs).

## Deliverable 1 — App shell

A persistent left **sidebar** that wraps the routed content, replacing the banner-only shell.

- **Layout:** sidebar (~220px) + main content region holding `<router-outlet>`. The current
  top banner (brand + Sign out) is absorbed into the sidebar.
- **Contents:** brand at top; primary nav **Dashboard** and **Support** with active-route
  highlighting (`--accent-soft` / `--accent`); account identity + **Sign out** pinned to the
  bottom.
- **Styling:** reuse existing design tokens in `web/src/styles.css` (`--panel`, `--border`,
  `--accent #6d6afe`, Inter, `--radius`). No new styling system (no Tailwind).
- **Page cleanup:** remove now-redundant per-page "Back to dashboard" links and duplicated
  account/header chrome so navigation lives in the shell. Keep contextually useful in-page
  links (e.g. "Inbox settings" from a ticket list) where they aid flow.
- **Test-id preservation:** keep existing `data-testid`s that e2e relies on (e.g.
  `nav-support`); update e2e specs where a control physically moved into the sidebar.
- **Responsive:** below a narrow breakpoint the sidebar collapses to a top bar (simple; no
  hamburger drawer required for v1).

Boundaries: the shell is a single presentational component that owns navigation + active
state and depends only on `Router`/`AuthService`. Pages render inside it and remain
independently testable.

## Deliverable 2 — Dev seed through the real ingestion pipeline

A new, committed, idempotent dev seed.

- **Entry point:** `make seed-demo` invoking a small Go tool under `cmd/seeddemo/`.
- **Steps (idempotent):**
  1. Resolve the demo businesses by name (Acme Holdings, Engineering, Platform Team, Sales),
     creating them if absent so the seed is self-contained on a fresh DB.
  2. **Ensure each business has an inbound address** (ingestion routes recipient →
     `inbound_address` row → business; the demo businesses currently have none).
  3. Inject ~3–4 realistic conversations per business **through the real ingestion path**
     (signature/auth → decode → route → ingest → thread → outbox), spanning a mix of
     statuses and priorities. At least one conversation includes a threaded customer reply
     (`in_reply_to` / `references`) so threading and RFC822 Message-ID idempotency are
     genuinely exercised.
  4. Re-running creates nothing new (Message-ID idempotency + presence checks).
- **Pipeline fidelity:** the seed exercises the real ingestion code (not raw row inserts).
  Whether it posts HMAC-signed envelopes to the public `POST /inbound/email/webhook` endpoint
  (requires the running server + `MANYFORGE_INBOUND_WEBHOOK_SECRET`) or drives the same
  in-process `Ingester` directly is settled in the plan; both satisfy "real pipeline." The
  HTTP path is preferred when it does not add fragility, as it covers signature + routing +
  rate-limit + handler end to end.
- **Result:** login → Support → live tickets with real multi-message threads across the demo
  businesses.

## Deliverable 3 — Drive every flow and fix what is real

With seeded data present, drive the real browser through each interactive flow and fix any
genuine defect found:

- US2: reply, internal note, delivery-failed surface, reply/note toggle.
- US3: status change, priority change, assign-to-me / assign-by-picker / unassign, tag add &
  remove.
- US4: add email domain, verify (DNS challenge surface), add inbound address.

Each genuine fix is paired with a Playwright regression under `web/e2e/`. If no defect is
found in a flow, add/confirm a regression that drives it against seeded data so the empty-desk
blind spot is closed.

## Error handling

- Seed is defensive and idempotent: safe to re-run; clear log of what it created vs skipped;
  non-zero exit on real failure (no silent partial seeds).
- App shell makes no new network calls; it reflects `AuthService` state and never blocks
  routing.

## Testing

- **App shell:** component/e2e coverage asserting nav items render, active state tracks the
  route, and Sign out works.
- **Seed:** idempotency assertion (second run adds no tickets) plus a smoke check that each
  demo business has ≥1 ticket with a multi-message thread after seeding.
- **Flows:** Playwright regressions under `web/e2e/` for each Deliverable-3 flow, run against
  seeded data.
- **Gate:** the full existing merge gate stays green — `make test && make int-test &&
  make contract-test && make lint` + `cd web && npm run e2e`.

## Tracking

- New `bd` issues: app-shell, dev-seed, flow-verification (children of the support-desk area).
- `manyforge-bhg`: reconcile as not-reproducing with the network-trace evidence above.

## Rollout / "watch it being built"

Build in visible browser increments, Deliverable 1 → 2 → 3, so progress is observable in the
running app at `localhost:4300` after each step.
