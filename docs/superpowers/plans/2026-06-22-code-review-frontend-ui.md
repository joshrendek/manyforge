# Code Review Frontend — Angular Page (connectors, trigger, history+detail) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Depends on Plan A** (`2026-06-22-code-review-backend-async-api.md`) — the endpoints it consumes must be merged first.

**Goal:** A `/code-review` page where a user manages GitHub repo connectors, triggers a PR review, and watches it run to completion with its findings — no curl.

**Architecture:** One Angular standalone page with three sections (Connectors, Review a PR, History) plus a detail view; a `CodeReviewService` wraps the REST API; the history/detail poll while a review is non-terminal. Mirrors the existing `/credentials/ai` and `/agents` patterns (standalone components, signals, `CurrentBusinessService`, `mf-*` classes, `data-testid` on every interactive element).

**Tech Stack:** Angular (standalone components, signals), `HttpClient`, Playwright e2e, Karma/Jasmine + `HttpTestingController` unit tests.

**Spec:** `docs/superpowers/specs/2026-06-21-code-review-ui-design.md` (§5 API, §6 frontend). **Issue:** `manyforge-elo`.

## Global Constraints

- Standalone components only; state via signals; get the active business from `CurrentBusinessService` (`core/current-business.service.ts`) — `current.businessId()`.
- API base: `/api/v1/businesses/${businessId}/{repo-connectors|code-reviews}`.
- `data-testid` on every interactive element (the e2e + unit tests select by it). Follow the naming already used in `pages/credentials/ai/list.ts` and `pages/agents/list.ts`.
- Reuse `mf-*` CSS classes (`mf-card`, `mf-field`, `mf-select`, `mf-input`, `mf-table`, `mf-btn`/`mf-btn-primary`/`mf-btn-danger`/`mf-btn-sm`, `mf-err`, `mf-hint`). No new global CSS unless a pattern is missing.
- All protected routes use `canActivate: [authGuard]` (see `app.routes.ts`).
- **Real-browser verification is required before "done"** (project rule): drive the page against the running stack (`:4300` web → `:8081` API) and capture before/after screenshots; then keep the Playwright spec as the regression.
- Run frontend commands from `web/`: `npm test` (unit), `npx playwright test` (e2e), `npm run build`.

## File Structure

- Create: `web/src/app/core/code-review.service.ts` (+ `.spec.ts`) — API client + types.
- Create: `web/src/app/pages/code-review/list.ts` (+ `.spec.ts`) — the page (3 sections).
- Create: `web/src/app/pages/code-review/detail.ts` (+ `.spec.ts`) — single-review detail (findings table).
- Modify: `web/src/app/app.routes.ts` — add `/code-review` and `/code-review/:businessId/:id`.
- Modify: `web/src/app/ui/nav.ts` — add the nav item.
- Create: `web/e2e/code-review.spec.ts` — Playwright flow.

---

### Task 1: CodeReviewService (API client + types)

**Files:**
- Create: `web/src/app/core/code-review.service.ts`
- Test: `web/src/app/core/code-review.service.spec.ts`

**Interfaces:**
- Produces:
  - Types `RepoConnector {id;type;display_name;base_url;repo;allow_private_base_url;created_at}`, `CreateRepoConnectorBody {type:'github';display_name;base_url;repo;api_token;allow_private_base_url}`, `Finding {file;line:number|null;severity;title;detail}`, `CodeReview {id;pr_number;status;summary;findings_count?;review_url?;created_at;posted_at?;findings?:Finding[]}`, `TriggerBody {agent_id;repo_connector_id;pr_number}`.
  - `CodeReviewService` methods: `listConnectors(bid)`, `createConnector(bid,body)`, `deleteConnector(bid,id)`, `listReviews(bid)`, `getReview(bid,id)`, `trigger(bid,body)`.

- [ ] **Step 1: Write failing service spec**

Read `web/src/app/core/ai-credentials.service.ts` + its spec for the exact conventions. Create `code-review.service.spec.ts` (TestBed + `HttpTestingController`): assert each method hits the right URL/verb/body and parses the response. Example:
```ts
it('lists connectors', () => {
  let res: RepoConnector[] | undefined;
  svc.listConnectors('b1').subscribe(r => res = r.items);
  const req = http.expectOne('/api/v1/businesses/b1/repo-connectors');
  expect(req.request.method).toBe('GET');
  req.flush({ items: [{ id: 'c1', repo: 'o/r', type: 'github', display_name: 'X', base_url: '', allow_private_base_url: true, created_at: '' }] });
  expect(res?.[0].repo).toBe('o/r');
});
it('triggers a review (202 pending)', () => {
  svc.trigger('b1', { agent_id: 'a1', repo_connector_id: 'c1', pr_number: 5 }).subscribe();
  const req = http.expectOne('/api/v1/businesses/b1/code-reviews');
  expect(req.request.method).toBe('POST');
  expect(req.request.body.pr_number).toBe(5);
  req.flush({ id: 'r1', status: 'pending' });
});
```

- [ ] **Step 2: Run to verify it fails**

Run (from `web/`): `npm test -- --include='**/code-review.service.spec.ts' --watch=false`
Expected: FAIL (service not found).

- [ ] **Step 3: Implement the service**

Create `code-review.service.ts` mirroring `ai-credentials.service.ts` (injectable, `HttpClient`, `Observable` returns). `listConnectors`/`listReviews` return `{items:...}` shapes; `deleteConnector` → `Observable<void>`.

- [ ] **Step 4: Run to verify it passes**

Run: `npm test -- --include='**/code-review.service.spec.ts' --watch=false`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/core/code-review.service.ts web/src/app/core/code-review.service.spec.ts
git commit -m "feat(007-ui): CodeReviewService API client + types"
```

---

### Task 2: Route + nav entry

**Files:**
- Modify: `web/src/app/app.routes.ts`
- Modify: `web/src/app/ui/nav.ts`

**Interfaces:**
- Produces: route `code-review` → list page; route `code-review/:businessId/:id` → detail page; nav item `{label:'Code Review', route:'/code-review', testid:'nav-code-review'}`.

- [ ] **Step 1: Add the routes**

In `web/src/app/app.routes.ts`, mirror the `agents` entry:
```ts
{ path: 'code-review', canActivate: [authGuard], loadComponent: () => import('./pages/code-review/list').then(m => m.CodeReviewListComponent) },
{ path: 'code-review/:businessId/:id', canActivate: [authGuard], loadComponent: () => import('./pages/code-review/detail').then(m => m.CodeReviewDetailComponent) },
```

- [ ] **Step 2: Add the nav item**

In `web/src/app/ui/nav.ts`, add to `NAV_ITEMS` after `Agents`:
```ts
{ label: 'Code Review', route: '/code-review', testid: 'nav-code-review' },
```

- [ ] **Step 3: Build to verify (components stubbed next task)**

This task compiles only once Task 3/4 create the components. Defer the build check to Task 4 Step 4. (Don't commit yet — commit with Task 3 once components exist, to keep the build green per-commit.)

---

### Task 3: Code Review page — Connectors + Review-a-PR sections

**Files:**
- Create: `web/src/app/pages/code-review/list.ts`
- Test: `web/src/app/pages/code-review/list.spec.ts`

**Interfaces:**
- Consumes: `CodeReviewService` (Task 1), `AgentsService.list` (existing, for the agent dropdown), `CurrentBusinessService`.
- Produces: `CodeReviewListComponent` rendering Connectors (table + add form + delete-confirm), Review-a-PR (agent select, connector select, pr number, submit), and History (Task 4).

- [ ] **Step 1: Write failing component spec (connectors + trigger)**

Read `pages/agents/list.ts` + `pages/credentials/ai/list.ts` for the standalone+signals+CurrentBusiness pattern and testid naming. Create `list.spec.ts` (TestBed + `HttpTestingController` or mocked service): renders connector rows from `listConnectors`; add form posts `createConnector`; delete shows confirm then calls `deleteConnector`; the Review-a-PR submit calls `trigger` with the selected agent/connector/pr.

- [ ] **Step 2: Run to verify it fails**

Run: `npm test -- --include='**/code-review/list.spec.ts' --watch=false`
Expected: FAIL (component not found).

- [ ] **Step 3: Implement the component (connectors + trigger sections)**

Create `list.ts` (standalone, signals). On init, read `current.businessId()`, load `listConnectors`, `listReviews`, and `agents.list` into signals. Render:
- **Connectors** section: `mf-table` of connectors (`data-testid="connector-row"`), an Add toggle (`connector-add-toggle`) revealing a form (display_name, repo, base_url default `https://api.github.com`, api_token, allow_private_base_url) → `createConnector` → refresh; per-row delete with a confirm (`connector-delete` / `connector-delete-confirm`) noting "deletes this connector and its reviews".
- **Review a PR** section: agent `<select data-testid="cr-agent">`, connector `<select data-testid="cr-connector">`, `<input data-testid="cr-pr-number">`, `<button data-testid="cr-submit">Review PR</button>` → `trigger` → on 202 insert an optimistic pending row into the history signal and start polling (Task 4). Surface `400` (egress not allowlisted) / `404` via an `mf-err` block near the form.

- [ ] **Step 4: Run unit test + build**

Run: `npm test -- --include='**/code-review/list.spec.ts' --watch=false` then `npm run build`
Expected: PASS / build clean (routes from Task 2 now resolve).

- [ ] **Step 5: Commit (routes + nav + page)**

```bash
git add web/src/app/app.routes.ts web/src/app/ui/nav.ts web/src/app/pages/code-review/list.ts web/src/app/pages/code-review/list.spec.ts
git commit -m "feat(007-ui): Code Review page — connectors + trigger sections, route + nav"
```

---

### Task 4: History section + polling + detail view

**Files:**
- Modify: `web/src/app/pages/code-review/list.ts` (+ `.spec.ts`)
- Create: `web/src/app/pages/code-review/detail.ts` (+ `.spec.ts`)

**Interfaces:**
- Consumes: `CodeReviewService.{listReviews,getReview}`.
- Produces: History table (status badge, findings count, time, GitHub link, row → detail); polling that refreshes while any visible review is `pending`/`running` and stops when all terminal; `CodeReviewDetailComponent` (summary + full findings table + "View on GitHub").

- [ ] **Step 1: Write failing specs (polling + detail)**

In `list.spec.ts` add: with a `pending` review present, the component polls `listReviews` again after the interval, and STOPS once the review is `succeeded` (assert no further requests). Create `detail.spec.ts`: renders summary + a `data-testid="finding-row"` per finding + a `view-on-github` link from `review_url`.

- [ ] **Step 2: Run to verify they fail**

Run: `npm test -- --include='**/code-review/*.spec.ts' --watch=false`
Expected: FAIL.

- [ ] **Step 3: Implement History + polling + detail**

In `list.ts`: render History (`data-testid="review-row"`, status `mf-` badge, `findings_count`, relative time, external link when `review_url`); clicking a row routes to `/code-review/{businessId}/{id}`. Polling: a signal-driven `timer(0, 3000)` (or `setInterval`) that calls `listReviews`; compute "any non-terminal" from the rows; `unsubscribe`/`clearInterval` when none remain and in `ngOnDestroy`. Create `detail.ts`: read `:businessId/:id` route params, `getReview`, render status + summary + findings table + GitHub link; poll `getReview` while non-terminal.

- [ ] **Step 4: Run specs + build**

Run: `npm test -- --include='**/code-review/*.spec.ts' --watch=false` then `npm run build`
Expected: PASS / clean.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/code-review/list.ts web/src/app/pages/code-review/list.spec.ts web/src/app/pages/code-review/detail.ts web/src/app/pages/code-review/detail.spec.ts
git commit -m "feat(007-ui): review history + live polling + findings detail view"
```

---

### Task 5: Playwright e2e (mocked API) — full flow

**Files:**
- Create: `web/e2e/code-review.spec.ts`

**Interfaces:**
- Consumes: the running web build with mocked API routes.

- [ ] **Step 1: Write the e2e spec**

Read `web/e2e/agents.spec.ts` for the `auth(page)` + `metadata(page)` + route-mock helpers. Create `code-review.spec.ts`:
```ts
test('trigger a review and watch it complete', async ({ page }) => {
  await auth(page);
  // mock: GET repo-connectors, GET agents, GET code-reviews (first pending, then succeeded),
  // POST code-reviews → 202 {id:'r1',status:'pending'}, GET code-reviews/r1 → succeeded+findings
  await page.goto('/code-review');
  await page.getByTestId('cr-agent').selectOption(/* a1 */);
  await page.getByTestId('cr-connector').selectOption(/* c1 */);
  await page.getByTestId('cr-pr-number').fill('5');
  await page.getByTestId('cr-submit').click();
  await expect(page.getByTestId('review-row')).toContainText('pending');
  // advance the polled mock to succeeded → row updates, polling stops
  await expect(page.getByTestId('review-row')).toContainText('succeeded');
  await page.getByTestId('review-row').click();
  await expect(page.getByTestId('finding-row')).toHaveCount(1);
  await expect(page.getByTestId('view-on-github')).toBeVisible();
});
```
Use a counter in the `code-reviews` GET mock to return `pending` then `succeeded` so polling is exercised.

- [ ] **Step 2: Run the e2e**

Run (from `web/`): `npx playwright test code-review`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add web/e2e/code-review.spec.ts
git commit -m "test(007-ui): Playwright e2e — trigger → poll → succeeded → findings detail"
```

---

### Task 6: Real-browser verification against the live stack

**Files:** none (verification + screenshots).

- [ ] **Step 1: Bring up the stack**

Ensure `:8081` API (air) and `:4300` web (`ng serve`) are running; the worker is started by main.go. Log in as the seeded demo user (`live-demo@manyforge.test` / `DevPassw0rd!`).

- [ ] **Step 2: Drive the real page**

Via gstack `$B` / Playwright MCP: open `/code-review`, add a GitHub connector (`joshrendek/manyforge`, base `https://api.github.com`, a PAT with PR read+write), create/select a code-review agent (+ AI credential), trigger a review on a real open PR, and watch the History row transition `pending→running→succeeded`. Open the detail and confirm findings render + the GitHub link works. Capture before/after screenshots.

- [ ] **Step 3: Record the outcome**

If the real run surfaces an opencode-contract failure (the still-open half of `manyforge-2nd`), file/append to `manyforge-2nd` with the exact error and do NOT block the UI merge on it — the UI correctly shows `failed` + `last_error`. Note in the PR what was verified live vs. stubbed.

- [ ] **Step 4: Final gate**

Run: `cd web && npm test -- --watch=false && npm run build && npx playwright test code-review`
Expected: all green.

---

## Self-Review

- **Spec coverage (§6):** service→T1; route+nav→T2; connectors+trigger sections→T3; history+polling+detail→T4; e2e→T5; real-browser→T6. Validation surfacing (400/404 inline)→T3 Step 3. All covered.
- **Type consistency:** `CodeReview`/`RepoConnector`/`Finding` (T1) consumed unchanged in T3/T4/T5; testids (`connector-row`, `cr-agent`, `cr-connector`, `cr-pr-number`, `cr-submit`, `review-row`, `finding-row`, `view-on-github`, `nav-code-review`) are consistent across components, unit specs, and the e2e.
- **Open verification during impl:** exact `CurrentBusinessService` API and the e2e `auth`/`metadata` helper signatures — copy verbatim from `agents.spec.ts` / existing pages in T1/T3/T5.

## Execution Handoff

**Plans complete and saved:**
- `docs/superpowers/plans/2026-06-22-code-review-backend-async-api.md` (Plan A — execute first)
- `docs/superpowers/plans/2026-06-22-code-review-frontend-ui.md` (Plan B — after A merges)

**Two execution options:**
1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session via executing-plans, batched with checkpoints.

Which approach — and do you want me to start with Plan A now, or review the plans first?
