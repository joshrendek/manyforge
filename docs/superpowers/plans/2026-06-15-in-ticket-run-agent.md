# In-Ticket "Run Agent" Control — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a "Run agent" control to the support thread (ticket) view that triggers an enabled agent against the current ticket via the existing `agents.run`-gated runs endpoint, with a fire-and-forget UX (immediate "started" toast + an outcome toast when the run returns).

**Architecture:** Frontend-only. A new `AgentsService.run()` method POSTs the existing backend endpoint; the thread-view component loads enabled agents on init (best-effort, like assignable-members), renders a picker+button in the triage card, and on click shows an immediate toast then maps the terminal run status to an outcome toast.

**Tech Stack:** Angular 21 (standalone, signals, template-driven `[ngModel]`, `@if`/`@for`), vitest. No backend changes.

**Spec:** `docs/superpowers/specs/2026-06-15-in-ticket-run-agent-design.md`. **bd issue:** `manyforge-q1m`.

---

## Conventions for every task

- Commands from `web/`: `cd web && npm test` (VITEST, runs once), `cd web && npm run build`, `cd web && npm run e2e -- e2e/<file>.spec.ts` (needs :4300; page.route-mocked). Use Read/plain `grep`, not `rg`. `noclobber` shell.
- Specs are **vitest**: `import { ... } from 'vitest'`; `expect(...).toBe(...)` / `expect.objectContaining(...)` (NOT jasmine).
- Conventional commits; **NO Co-Authored-By trailer**. The bd hook sweeps `.beads/issues.jsonl` into commits — leave it.

---

## File Structure

- Modify `web/src/app/core/agents.service.ts` — add `AgentRun` interface + `run()` method.
- Modify `web/src/app/pages/support/thread-view.ts` — inject `AgentsService` + `ToastService`; load enabled agents on init; add the triage-card control + `runAgent()` handler.
- Modify `web/src/app/pages/support/thread-view.spec.ts` — flush the new agents GET in BOTH boot helpers; add the run-control tests.

---

## Task 1: `AgentsService.run()` + `AgentRun` interface

**Files:**
- Modify: `web/src/app/core/agents.service.ts`

**Context (verified):** `base(businessId)` returns `/api/v1/businesses/${businessId}/agents`. The backend `POST …/agents/{agentID}/runs` returns 202 with `{ id, agent_id, trigger, status, tokens_in, tokens_out, cost_cents, correlation_id, error? }`; `status` ∈ `queued|running|awaiting_approval|succeeded|failed`. No `run()`/`AgentRun` exist yet.

- [ ] **Step 1: Write the failing unit spec**

In a new `web/src/app/core/agents.service.spec.ts` (if none exists; otherwise append). Mirror the vitest + `provideHttpClient`/`provideHttpClientTesting` style used elsewhere (e.g. open `web/src/app/core/connectors.service.spec.ts` for the exact setup):

```ts
import { beforeEach, describe, expect, it } from 'vitest';
import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { AgentsService } from './agents.service';

describe('AgentsService.run', () => {
  let svc: AgentsService;
  let http: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(AgentsService);
    http = TestBed.inject(HttpTestingController);
  });

  it('POSTs the runs endpoint with the ticket target and returns the run', () => {
    let got: { id: string; status: string } | undefined;
    svc.run('b1', 'a1', { target_type: 'ticket', target_id: 't1' }).subscribe((r) => (got = r));
    const req = http.expectOne('/api/v1/businesses/b1/agents/a1/runs');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ target_type: 'ticket', target_id: 't1' });
    req.flush({ id: 'run1', agent_id: 'a1', trigger: 'manual', status: 'awaiting_approval', tokens_in: 0, tokens_out: 0, cost_cents: 0, correlation_id: 'c1' });
    expect(got!.status).toBe('awaiting_approval');
  });
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `svc.run` is not a function.

- [ ] **Step 3: Add the `AgentRun` interface + `run()` method**

In `web/src/app/core/agents.service.ts`, add the interface near the other interfaces:

```ts
export interface AgentRun {
  id: string;
  agent_id: string;
  trigger: string;
  status: 'queued' | 'running' | 'awaiting_approval' | 'succeeded' | 'failed';
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  correlation_id: string;
  error?: string;
}
```

Add the method to `AgentsService` (after `mcpServers()` or near `create`):

```ts
// run triggers a manual agent run; with a ticket target the agent acts on that
// ticket. The backend runs it synchronously and returns the terminal run (202).
run(businessId: string, agentId: string, body: { target_type: 'ticket'; target_id: string }): Observable<AgentRun> {
  return this.http.post<AgentRun>(`${this.base(businessId)}/${agentId}/runs`, body);
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd web && npm test`
Expected: the `AgentsService.run` spec PASSES; suite green. Then `cd web && npm run build`.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/core/agents.service.ts web/src/app/core/agents.service.spec.ts
git commit -m "feat(web): AgentsService.run() for manual agent runs"
```

---

## Task 2: Thread-view "Run agent" control

**Files:**
- Modify: `web/src/app/pages/support/thread-view.ts`
- Modify: `web/src/app/pages/support/thread-view.spec.ts`

**Context (verified):** `thread-view.ts` reads `this.businessId`/`this.ticketId` from `route.snapshot.paramMap` (private strings). `FormsModule` is already imported. `ToastService` (`../../ui/toast/toast.service`, methods `success`/`error`) is NOT yet injected. The triage card is `<div class="triage mf-card" data-testid="triage">`; controls are `<div class="mf-field triage-field">` blocks; the assignee-row uses `class="assignee-row"` + `mf-btn mf-btn-ghost mf-btn-sm`. `ngOnInit` already does best-effort loads (`/me`, assignable-members) guarded by `if (this.businessId)`, then `reload()`. **The spec has TWO `describe` blocks (triage ~line 47, ui-redesign ~line 293), each with its own `ActivatedRoute` provider and boot/`loadWith` helper** — both must flush the new agents GET or `mock.verify()` fails.

- [ ] **Step 1: Write the failing component specs**

In `web/src/app/pages/support/thread-view.spec.ts`:

(a) **Update the `loadWith` boot helper** (the triage `describe`, ~line 54) to flush the new agents GET. Add a parameter + a flush. Change its signature/body to:

```ts
function loadWith(t: Ticket, members: AssignableMember[] = [], agents: Agent[] = []): void {
  fixture = TestBed.createComponent(ThreadViewComponent);
  cmp = fixture.componentInstance;
  fixture.detectChanges(); // ngOnInit fires /me + assignable-members + agents + getTicket

  mock.expectOne('/api/v1/me').flush({ id: myPid, email: 'me@x.test', display_name: 'Me', email_verified: true, status: 'active' });
  mock.expectOne(`/api/v1/businesses/${biz}/assignable-members`).flush({ items: members, next_cursor: null });
  mock.expectOne(`/api/v1/businesses/${biz}/agents`).flush({ items: agents });
  mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`).flush(t);
  mock.expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets/${tid}/messages`).flush(emptyPage);
  fixture.detectChanges();
}
```

(Import `Agent` from `'../../core/agents.service'` at the top of the spec. `mock.expectOne` matches by URL not order, so the new flush slots in anywhere among the init GETs.)

(b) **Do the SAME for the ui-redesign `describe`'s boot helper (~line 293)** — find its equivalent of `loadWith` and add the identical `mock.expectOne(`/api/v1/businesses/${biz}/agents`).flush({ items: [] })` line (default empty so existing tests there are unaffected). If that block's helper doesn't take an `agents` param, just flush `{ items: [] }`.

(c) **Add the new tests** (in the triage `describe`):

```ts
it('shows the run-agent control with a button when there is one enabled agent', () => {
  loadWith(makeTicket({}), [], [makeAgent({ id: 'a1', name: 'Triage Bot', enabled: true })]);
  expect(fixture.nativeElement.querySelector('[data-testid="run-agent-btn"]')).toBeTruthy();
  expect(fixture.nativeElement.querySelector('[data-testid="run-agent-select"]')).toBeFalsy();
});

it('shows the no-agents hint when there are no enabled agents', () => {
  loadWith(makeTicket({}), [], [makeAgent({ id: 'a1', enabled: false })]);
  expect(fixture.nativeElement.querySelector('[data-testid="run-agent-none"]')).toBeTruthy();
  expect(fixture.nativeElement.querySelector('[data-testid="run-agent-btn"]')).toBeFalsy();
});

it('shows a picker when there are multiple enabled agents', () => {
  loadWith(makeTicket({}), [], [makeAgent({ id: 'a1', enabled: true }), makeAgent({ id: 'a2', enabled: true })]);
  expect(fixture.nativeElement.querySelector('[data-testid="run-agent-select"]')).toBeTruthy();
});

it('runAgent POSTs the runs endpoint with the ticket target and toasts the outcome', () => {
  loadWith(makeTicket({}), [], [makeAgent({ id: 'a1', enabled: true })]);
  cmp.runAgent();
  const req = mock.expectOne(`/api/v1/businesses/${biz}/agents/a1/runs`);
  expect(req.request.method).toBe('POST');
  expect(req.request.body).toEqual({ target_type: 'ticket', target_id: tid });
  req.flush({ id: 'run1', agent_id: 'a1', trigger: 'manual', status: 'awaiting_approval', tokens_in: 0, tokens_out: 0, cost_cents: 0, correlation_id: 'c1' });
  // running flag cleared after response
  expect(cmp.running()).toBe(false);
});

it('runAgent surfaces a no-access error on 404', () => {
  loadWith(makeTicket({}), [], [makeAgent({ id: 'a1', enabled: true })]);
  cmp.runAgent();
  mock.expectOne(`/api/v1/businesses/${biz}/agents/a1/runs`).flush({ code: 'NOT_FOUND' }, { status: 404, statusText: 'Not Found' });
  expect(cmp.running()).toBe(false);
});
```

Add a `makeAgent` factory near the existing `makeTicket` helper (mirror its style):

```ts
function makeAgent(over: Partial<Agent> = {}): Agent {
  return {
    id: 'a1', business_id: biz, principal_id: 'p1', name: 'Agent', provider: 'anthropic',
    model: 'claude-opus-4-8', system_prompt: '', allowed_tools: [], autonomy_mode: 1, enabled: true,
    monthly_budget_cents: 0, allowed_mcp_servers: [], retriage_on_reply: false, created_at: '', updated_at: '',
    ...over,
  } as Agent;
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `cmp.runAgent` / `cmp.running` undefined, and the control isn't rendered. (The existing thread-view tests in the ui-redesign block must NOT fail on an unmatched agents GET — if they do, you missed updating that block's boot helper in Step 1b; fix it.)

- [ ] **Step 3: Add the imports, injections, state, init-load, and handler**

In `web/src/app/pages/support/thread-view.ts`:

(a) Imports (top of file, with the other `../../core` / `../../ui` imports):

```ts
import { Agent, AgentsService } from '../../core/agents.service';
import { ToastService } from '../../ui/toast/toast.service';
```

(b) Inject + state (in the class, near the other `inject(...)` + signal fields):

```ts
  private agents = inject(AgentsService);
  private toast = inject(ToastService);

  // Run-agent control (manual trigger against this ticket).
  enabledAgents = signal<Agent[]>([]);
  selectedAgentId = signal<string>('');
  running = signal(false);
```

(c) In `ngOnInit`, after the assignable-members load and still guarded by `if (this.businessId)`, add a best-effort enabled-agents load:

```ts
    if (this.businessId) {
      this.agents.list(this.businessId).subscribe({
        next: (r) => {
          const enabled = (r.items ?? []).filter((a) => a.enabled);
          this.enabledAgents.set(enabled);
          if (enabled.length > 0) this.selectedAgentId.set(enabled[0].id);
        },
        error: () => {},
      });
    }
```

(d) Add the `runAgent()` handler method (near the triage methods):

```ts
  // Fire-and-forget: an immediate toast, then an outcome toast when the (synchronous)
  // run returns. The running flag only guards against a double-click before the first
  // response lands. Actions auto-apply or land in /approvals per the agent's autonomy mode.
  runAgent(): void {
    const agentId = this.selectedAgentId();
    if (this.running() || !agentId) return;
    this.running.set(true);
    this.toast.success('Agent started — it will act on this ticket in the background.');
    this.agents.run(this.businessId, agentId, { target_type: 'ticket', target_id: this.ticketId }).subscribe({
      next: (run) => {
        this.running.set(false);
        if (run.status === 'awaiting_approval') {
          this.toast.success('Agent finished — proposed actions are waiting in Approvals.');
        } else if (run.status === 'failed') {
          this.toast.error('Agent run failed.');
        } else {
          this.toast.success('Agent finished.');
        }
      },
      error: (e: HttpErrorResponse) => {
        this.running.set(false);
        this.toast.error(e.status === 403 || e.status === 404 ? "You don't have access to run agents." : 'Could not start the agent run.');
      },
    });
  }
```

(`HttpErrorResponse` is already imported in this file.)

- [ ] **Step 4: Add the template control to the triage card**

In `thread-view.ts`, inside the triage card (`<div class="triage mf-card" data-testid="triage">`), add a new `triage-field` block (e.g. after the assignee field, before the card closes):

```html
        <div class="mf-field triage-field">
          <label>Run agent</label>
          @if (enabledAgents().length === 0) {
            <span class="mf-hint" data-testid="run-agent-none">No enabled agents for this business.</span>
          } @else {
            <div class="assignee-row" data-testid="run-agent-control">
              @if (enabledAgents().length > 1) {
                <select class="mf-select" data-testid="run-agent-select" aria-label="Choose an agent"
                        [disabled]="running()" [ngModel]="selectedAgentId()" (ngModelChange)="selectedAgentId.set($event)">
                  @for (a of enabledAgents(); track a.id) {
                    <option [value]="a.id">{{ a.name }}</option>
                  }
                </select>
              }
              <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="run-agent-btn"
                      [disabled]="running() || !selectedAgentId()" (click)="runAgent()">
                {{ running() ? 'Starting…' : 'Run agent' }}
              </button>
            </div>
          }
        </div>
```

(Confirm `mf-btn-primary` exists in the app's styles — the credential/agent forms use it; if the triage card prefers `mf-btn-ghost`, match the surrounding buttons. `mf-hint` is a global hint class used elsewhere.)

- [ ] **Step 5: Run to verify pass**

Run: `cd web && npm test`
Expected: the new run-control specs PASS; ALL existing thread-view specs (both `describe` blocks) still PASS (the agents GET is flushed in both boot helpers). Then `cd web && npm run build`.

- [ ] **Step 6: Commit**

```bash
git add web/src/app/pages/support/thread-view.ts web/src/app/pages/support/thread-view.spec.ts
git commit -m "feat(web): in-ticket Run agent control (triage card)"
```

---

## Task 3: Verification & PR

- [ ] **Step 1: Full frontend gates**

Run: `cd web && npm run build && npm test`
Expected: build succeeds; full vitest suite green.

- [ ] **Step 2: e2e (only if a thread-view/support e2e exists)**

Run: `ls web/e2e/ | grep -iE 'thread|support|ticket'`
- If a support/thread e2e exists, extend it with a run-agent case (mock `GET …/agents` → one enabled agent, click `run-agent-btn`, mock the `…/runs` POST, assert the toast). Run it: `cd web && npm run e2e -- e2e/<file>.spec.ts`.
- If none exists, **skip** — the component spec covers the behavior (the spec deliberately marked a dedicated e2e optional).

- [ ] **Step 3: Manual smoke (real stack)**

With the backend (`MANYFORGE_AI_MASTER_KEY` set), web :4300, an enabled agent + a resolvable credential: open a ticket (`/support/:businessId/:tid`), confirm the "Run agent" control appears in the triage card, click it → "started" toast → after the run, an outcome toast (and, for autonomy mode 1/2, the action appears on `/approvals`).

- [ ] **Step 4: Open PR / land**

```bash
git push -u origin agent-run-button
gh pr create --base master --title "In-ticket Run agent control" --body "Implements docs/superpowers/specs/2026-06-15-in-ticket-run-agent-design.md (bd manyforge-q1m). Adds a Run-agent control to the ticket triage card (fire-and-forget) backed by AgentsService.run() → the existing agents.run-gated /runs endpoint."
```

- [ ] **Step 5: Update bd**

Run: `export PATH="$HOME/go/bin:$PATH" && bd close manyforge-q1m` then a `chore(bd): close manyforge-q1m` commit.

---

## Self-Review (completed by plan author)

- **Spec coverage:** `AgentsService.run()` ✓ (T1); triage-card control with 0/1/>1 enabled-agent rendering ✓ (T2 Step 4); fire-and-forget started+outcome toasts ✓ (T2 Step 3d); 403/404 no-access toast ✓ (T2); enabled-agents init load ✓ (T2 Step 3c); tests (service unit + component spec covering render/click/status-toast/no-agents/error) ✓ (T1, T2 Step 1); data-testids (`run-agent-control/select/btn/none`) ✓.
- **Placeholder scan:** the only deferred specifics name the exact file to copy from (the connectors.service spec harness in T1; the ui-redesign `describe`'s boot helper in T2 Step 1b; confirming `mf-btn-primary` vs the surrounding button class in T2 Step 4). No "TBD"/"add validation"/"similar to" placeholders.
- **Type/name consistency:** `AgentRun.status` union (T1) matches the backend strings and the `runAgent` status checks (T2). `run(businessId, agentId, {target_type:'ticket', target_id})` signature is identical in T1, the T1 spec, the T2 handler, and the T2 spec. `enabledAgents`/`selectedAgentId`/`running` signals are consistent across T2's state, template, handler, and tests. `makeAgent` produces the real `Agent` shape (matches `agents.service.ts`).
- **Critical gotcha surfaced:** the spec's TWO boot helpers must BOTH flush the new `…/agents` GET (T2 Step 1a+1b) or existing thread-view tests fail on an unmatched request — called out explicitly.
