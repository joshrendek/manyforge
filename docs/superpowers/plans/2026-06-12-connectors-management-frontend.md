# Connectors Management UI (Frontend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Angular "Connectors" management UI — list with sync-health, connect/edit/rotate forms (write-only credential fields), test/enable-disable/delete actions, plus a nav item with a degraded-connector badge — on the Stream-1 design system, against the backend API from the companion backend plan.

**Architecture:** Mirror the Stream-2 Approvals queue (`pages/approvals/queue.ts`) for the list page and `pages/support/inbox-settings.ts` for the forms. Standalone components, Angular signals for state, the hand-built `mf-*` CSS kit + `--mf-*` tokens, template-driven forms (`FormsModule` + `[(ngModel)]`), business selection persisted via `CurrentBusinessService`, and the nav badge stamped in `app.ts` exactly like the approvals badge. No reactive forms, no new CSS primitives.

**Tech Stack:** Angular (standalone components, signals, control flow `@if`/`@for`), RxJS, Vitest (`npm test` → `@angular/build:unit-test`), Playwright (`npm run e2e`, base `http://localhost:4300`).

**Spec:** `docs/superpowers/specs/2026-06-12-connectors-management-design.md`. **Backend plan:** `docs/superpowers/plans/2026-06-12-connectors-management-backend.md`. **Issue:** `manyforge-4zs.3`.

**Conventions you must follow (verbatim from the codebase):**
- Bearer auth is automatic via `core/auth.interceptor.ts` (reads `localStorage['mf_access']`). Services just call `/api/v1/...`.
- Business list: `BusinessService.list()` → `{ items: Business[] }`; `Business` type from `core/tree`. Current business persisted via `CurrentBusinessService` (`businessId` signal + `set(id)`, localStorage key `mf-current-business`).
- Toasts: `ToastService` (`core`/`ui/toast/toast.service`) — `.success(msg)` / `.error(msg)`.
- UI kit (all standalone): `mf-page-header` (`@Input() title`, `subtitle`), `mf-status-pill` (`@Input() tone: Tone`, `label`), `mf-empty-state` (`@Input() icon`, `title`), `mf-spinner`. `Tone = 'neutral'|'accent'|'warn'|'success'|'danger'` from `ui/status`.
- CSS classes (all defined in `web/src/styles.css`; **undefined classes render silently** — only reuse these): `mf-card`, `mf-filters`, `mf-field`, `mf-input`, `mf-select`, `mf-textarea`, `mf-table`, `mf-tr`, `mf-th`, `mf-btn` + `mf-btn-primary`/`mf-btn-ghost`/`mf-btn-danger`/`mf-btn-sm`, `mf-pill`, `mf-err`, `mf-add-form`, `nav-badge`. `.mf-table`/`.mf-tr` are **DIV-flex, not real tables** — align columns with fixed-width `<span style="width:…">`. `.mf-select` is a native `<select>` needing `FormsModule` + `[ngModel]`/`[(ngModel)]`.
- Routes are lazy `loadComponent` behind `authGuard` (`app.routes.ts`). Nav is data-driven (`ui/nav.ts`). Badge is a `computed()` in `app.ts` that stamps `badge` onto a copied `NAV_ITEMS`.

---

## File Structure

| File | Responsibility | Create/Modify |
|---|---|---|
| `web/src/app/core/connectors.service.ts` | HTTP service + `Connector`/`ConnectorHealth`/`TestResult` types + `degradedCount` signal for the badge | Create |
| `web/src/app/pages/connectors/connector-form.ts` | Reusable form for **create** + **rotate-credential** (write-only password fields) | Create |
| `web/src/app/pages/connectors/list.ts` | The `/connectors` page: business filter, list with health pill, row actions (test / enable-disable / rotate / delete-confirm), embeds the create form | Create |
| `web/src/app/ui/nav.ts` | Add the `Connectors` nav item | Modify |
| `web/src/app/app.routes.ts` | Add the lazy `/connectors` route | Modify |
| `web/src/app/app.ts` + `app.html` | Stamp the degraded-connector badge + poll its count | Modify |
| `web/src/app/pages/connectors/connector-form.spec.ts` | Vitest: credential fields are `type=password`; create/rotate submit | Create |
| `web/src/app/pages/connectors/list.spec.ts` | Vitest: list render, health pill, optimistic disable, delete-confirm | Create |
| `web/e2e/connectors.spec.ts` | Playwright: list → create → test → delete-confirm flows (route mocks) | Create |

---

## Task 1: `ConnectorsService` + types

**Files:**
- Create: `web/src/app/core/connectors.service.ts`
- Test: `web/src/app/core/connectors.service.spec.ts`

- [ ] **Step 1: Write the failing test**

`web/src/app/core/connectors.service.spec.ts`:
```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { Connector, ConnectorsService } from './connectors.service';

function conn(over: Partial<Connector> = {}): Connector {
  return {
    id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme', base_url: 'https://acme.atlassian.net',
    allow_private_base_url: false, config: {}, status: 'enabled', last_reconciled_at: null,
    created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
    health: { state: 'healthy', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null },
    ...over,
  };
}

describe('ConnectorsService', () => {
  let svc: ConnectorsService;
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(ConnectorsService);
    mock = TestBed.inject(HttpTestingController);
  });

  it('list() sets degradedCount to the non-healthy connectors', () => {
    svc.list('b1').subscribe();
    mock.expectOne('/api/v1/businesses/b1/connectors').flush({
      items: [conn(), conn({ id: 'c2', health: { state: 'degraded', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 2, last_error: 'boom' } }), conn({ id: 'c3', status: 'disabled', health: { state: 'disabled', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null } })],
    });
    expect(svc.degradedCount()).toBe(2);
  });

  it('rotate() PUTs to the credential subpath', () => {
    svc.rotate('b1', 'c1', { email: 'a@b.c', api_token: 't', webhook_secret: 'w' }).subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/credential');
    expect(req.request.method).toBe('PUT');
    req.flush(conn());
  });

  it('test() POSTs to the test subpath', () => {
    svc.test('b1', 'c1').subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/test');
    expect(req.request.method).toBe('POST');
    req.flush({ ok: true, detail: 'ok' });
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npm test -- --run src/app/core/connectors.service.spec.ts`
Expected: FAIL — cannot find module `./connectors.service`.

- [ ] **Step 3: Implement the service**

`web/src/app/core/connectors.service.ts`:
```typescript
import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap } from 'rxjs';

export interface ConnectorHealth {
  state: 'healthy' | 'degraded' | 'disabled';
  linked_ticket_count: number;
  pending_outbound_ops: number;
  failed_outbound_ops: number;
  last_error: string | null;
}

export interface Connector {
  id: string;
  business_id: string;
  type: string;
  display_name: string;
  base_url: string;
  allow_private_base_url: boolean;
  config: Record<string, unknown>;
  status: string;
  last_reconciled_at: string | null;
  created_at: string;
  updated_at: string;
  health: ConnectorHealth;
}

export interface CreateConnectorBody {
  type: string;
  display_name: string;
  base_url: string;
  allow_private_base_url?: boolean;
  email: string;
  api_token: string;
  webhook_secret?: string;
  config?: Record<string, unknown>;
}

export interface UpdateConnectorBody {
  display_name?: string;
  config?: Record<string, unknown>;
  status?: 'enabled' | 'disabled';
}

export interface RotateCredentialBody {
  email: string;
  api_token: string;
  webhook_secret?: string;
}

export interface TestResult {
  ok: boolean;
  detail: string;
}

// ConnectorsService talks to the connectors.manage API. degradedCount drives the nav badge:
// it counts connectors that are NOT healthy (degraded or disabled) for the current business.
@Injectable({ providedIn: 'root' })
export class ConnectorsService {
  private http = inject(HttpClient);
  readonly degradedCount = signal(0);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/connectors`;
  }

  list(businessId: string): Observable<{ items: Connector[] }> {
    return this.http
      .get<{ items: Connector[] }>(this.base(businessId))
      .pipe(tap((r) => this.degradedCount.set((r.items ?? []).filter((c) => c.health.state !== 'healthy').length)));
  }
  create(businessId: string, body: CreateConnectorBody): Observable<Connector> {
    return this.http.post<Connector>(this.base(businessId), body);
  }
  update(businessId: string, id: string, body: UpdateConnectorBody): Observable<Connector> {
    return this.http.patch<Connector>(`${this.base(businessId)}/${id}`, body);
  }
  rotate(businessId: string, id: string, body: RotateCredentialBody): Observable<Connector> {
    return this.http.put<Connector>(`${this.base(businessId)}/${id}/credential`, body);
  }
  test(businessId: string, id: string): Observable<TestResult> {
    return this.http.post<TestResult>(`${this.base(businessId)}/${id}/test`, {});
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  refreshCount(businessId: string): void {
    this.list(businessId).subscribe({ error: () => this.degradedCount.set(0) });
  }
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd web && npm test -- --run src/app/core/connectors.service.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/core/connectors.service.ts web/src/app/core/connectors.service.spec.ts
git commit -m "feat(ui): ConnectorsService (CRUD/rotate/test + degraded badge count)"
```

---

## Task 2: `ConnectorFormComponent` (create + rotate)

**Files:**
- Create: `web/src/app/pages/connectors/connector-form.ts`
- Test: `web/src/app/pages/connectors/connector-form.spec.ts`

- [ ] **Step 1: Write the failing test**

`web/src/app/pages/connectors/connector-form.spec.ts`:
```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { ConnectorFormComponent } from './connector-form';

describe('ConnectorFormComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    mock = TestBed.inject(HttpTestingController);
  });

  it('credential inputs are type=password (create mode)', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    f.componentInstance.businessId = 'b1';
    f.componentInstance.mode = 'create';
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect((el.querySelector('[data-testid="conn-api-token"]') as HTMLInputElement).type).toBe('password');
    expect((el.querySelector('[data-testid="conn-webhook-secret"]') as HTMLInputElement).type).toBe('password');
  });

  it('create submit POSTs the body and emits saved', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    const c = f.componentInstance;
    c.businessId = 'b1';
    c.mode = 'create';
    let saved = false;
    c.saved.subscribe(() => (saved = true));
    c.displayName = 'Acme';
    c.baseUrl = 'https://acme.atlassian.net';
    c.email = 'a@b.c';
    c.apiToken = 'tok';
    f.detectChanges();
    c.submit();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.api_token).toBe('tok');
    req.flush({ id: 'c1' });
    expect(saved).toBe(true);
  });

  it('rotate mode only shows credential fields and PUTs to /credential', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    const c = f.componentInstance;
    c.businessId = 'b1';
    c.mode = 'rotate';
    c.connectorId = 'c1';
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="conn-display-name"]')).toBeNull();
    c.email = 'a@b.c';
    c.apiToken = 'newtok';
    c.submit();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/credential');
    expect(req.request.method).toBe('PUT');
    req.flush({ id: 'c1' });
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npm test -- --run src/app/pages/connectors/connector-form.spec.ts`
Expected: FAIL — cannot find module `./connector-form`.

- [ ] **Step 3: Implement the form component**

`web/src/app/pages/connectors/connector-form.ts`:
```typescript
import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Connector, ConnectorsService } from '../../core/connectors.service';

// Reusable connector form. mode='create' collects the full connector + credential bundle;
// mode='rotate' collects ONLY the credential bundle (for an existing connector). Credential
// fields are type=password and are write-only — they are sent to the API but the API never
// returns them, so the form starts blank every time (no prefill on rotate).
@Component({
  selector: 'app-connector-form',
  imports: [FormsModule],
  template: `
    <form class="mf-add-form" data-testid="connector-form" (ngSubmit)="submit()">
      @if (mode === 'create') {
        <div class="mf-field" style="flex:0 1 160px">
          <label for="conn-type">Type</label>
          <select id="conn-type" class="mf-select" data-testid="conn-type"
                  [ngModel]="type()" (ngModelChange)="type.set($event)" name="type" [disabled]="submitting()">
            <option value="jira">Jira</option>
            <option value="zendesk">Zendesk</option>
          </select>
        </div>
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-display-name">Display name</label>
          <input id="conn-display-name" type="text" class="mf-input" data-testid="conn-display-name"
                 [(ngModel)]="displayName" name="display_name" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:1 1 240px">
          <label for="conn-base-url">Base URL</label>
          <input id="conn-base-url" type="url" class="mf-input" data-testid="conn-base-url"
                 placeholder="https://acme.atlassian.net" [(ngModel)]="baseUrl" name="base_url"
                 autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-project-key">Project key</label>
          <input id="conn-project-key" type="text" class="mf-input" data-testid="conn-project-key"
                 placeholder="PROJ" [(ngModel)]="projectKey" name="project_key" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-issue-type">Issue type</label>
          <input id="conn-issue-type" type="text" class="mf-input" data-testid="conn-issue-type"
                 placeholder="Task" [(ngModel)]="issueType" name="issue_type" autocomplete="off" [disabled]="submitting()" />
        </div>
      }

      <div class="mf-field" style="flex:1 1 200px">
        <label for="conn-email">Email</label>
        <input id="conn-email" type="email" class="mf-input" data-testid="conn-email"
               [(ngModel)]="email" name="email" autocomplete="off" [disabled]="submitting()" />
      </div>
      <div class="mf-field" style="flex:1 1 200px">
        <label for="conn-api-token">API token</label>
        <input id="conn-api-token" type="password" class="mf-input" data-testid="conn-api-token"
               placeholder="••••••••" [(ngModel)]="apiToken" name="api_token" autocomplete="off" [disabled]="submitting()" />
      </div>
      <div class="mf-field" style="flex:1 1 200px">
        <label for="conn-webhook-secret">Webhook secret</label>
        <input id="conn-webhook-secret" type="password" class="mf-input" data-testid="conn-webhook-secret"
               placeholder="••••••••" [(ngModel)]="webhookSecret" name="webhook_secret" autocomplete="off" [disabled]="submitting()" />
        <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">Never shown again — save it somewhere safe.</span>
      </div>

      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : (mode === 'create' ? 'Connect' : 'Rotate credential') }}
        </button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-form-cancel"
                (click)="cancelled.emit()" [disabled]="submitting()">Cancel</button>
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="connector-form-error" style="flex:1 1 100%">{{ error() }}</p>
      }
    </form>
  `,
})
export class ConnectorFormComponent {
  @Input() businessId = '';
  @Input() mode: 'create' | 'rotate' = 'create';
  @Input() connectorId = '';
  @Output() saved = new EventEmitter<Connector>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(ConnectorsService);

  type = signal<'jira' | 'zendesk'>('jira');
  displayName = '';
  baseUrl = '';
  projectKey = '';
  issueType = '';
  email = '';
  apiToken = '';
  webhookSecret = '';

  submitting = signal(false);
  error = signal('');

  valid(): boolean {
    if (!this.email.trim() || !this.apiToken.trim()) return false;
    if (this.mode === 'create') return !!this.displayName.trim() && !!this.baseUrl.trim();
    return true;
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    const obs =
      this.mode === 'create'
        ? this.api.create(this.businessId, {
            type: this.type(),
            display_name: this.displayName.trim(),
            base_url: this.baseUrl.trim(),
            email: this.email.trim(),
            api_token: this.apiToken,
            webhook_secret: this.webhookSecret || undefined,
            config: this.buildConfig(),
          })
        : this.api.rotate(this.businessId, this.connectorId, {
            email: this.email.trim(),
            api_token: this.apiToken,
            webhook_secret: this.webhookSecret || undefined,
          });
    obs.subscribe({
      next: (c) => {
        this.reset();
        this.submitting.set(false);
        this.saved.emit(c);
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  private buildConfig(): Record<string, unknown> {
    const cfg: Record<string, unknown> = {};
    if (this.projectKey.trim()) cfg['project_key'] = this.projectKey.trim();
    if (this.issueType.trim()) cfg['issue_type'] = this.issueType.trim();
    return cfg;
  }

  private reset(): void {
    this.displayName = '';
    this.baseUrl = '';
    this.projectKey = '';
    this.issueType = '';
    this.email = '';
    this.apiToken = '';
    this.webhookSecret = '';
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'A connector for that system already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd web && npm test -- --run src/app/pages/connectors/connector-form.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/connectors/connector-form.ts web/src/app/pages/connectors/connector-form.spec.ts
git commit -m "feat(ui): ConnectorFormComponent (create + rotate, write-only credential fields)"
```

---

## Task 3: Connectors list page

**Files:**
- Create: `web/src/app/pages/connectors/list.ts`
- Test: `web/src/app/pages/connectors/list.spec.ts`

- [ ] **Step 1: Write the failing test**

`web/src/app/pages/connectors/list.spec.ts`:
```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectorsListComponent } from './list';

const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }] };
const connectors = {
  items: [
    { id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme Jira', base_url: 'https://acme.atlassian.net', allow_private_base_url: false, config: {}, status: 'enabled', last_reconciled_at: null, created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z', health: { state: 'degraded', linked_ticket_count: 3, pending_outbound_ops: 0, failed_outbound_ops: 1, last_error: 'HTTP 500' } },
  ],
};

describe('ConnectorsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { vi.useRealTimers(); document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  function mount() {
    const f = TestBed.createComponent(ConnectorsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/connectors').flush(connectors);
    f.detectChanges();
    return f;
  }

  it('renders a connector row with a health pill', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="connector-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="connector-name"]')?.textContent).toContain('Acme Jira');
    expect(el.querySelector('[data-testid="connector-health"] .mf-pill-warn')).toBeTruthy(); // degraded → warn
  });

  it('delete asks for confirm, then DELETEs and removes the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="connector-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    // Confirm panel appears with the linked-ticket count.
    expect(f.nativeElement.querySelector('[data-testid="connector-delete-confirm"]')?.textContent).toContain('3');
    (f.nativeElement.querySelector('[data-testid="connector-delete-yes"]') as HTMLButtonElement).click();
    mock.expectOne('/api/v1/businesses/b1/connectors/c1').flush(null);
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="connector-row"]')).toBeNull();
  });

  it('disable PATCHes status and updates the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="connector-toggle"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1');
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body.status).toBe('disabled');
    req.flush({ ...connectors.items[0], status: 'disabled', health: { ...connectors.items[0].health, state: 'disabled' } });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="connector-toggle"]')?.textContent).toContain('Enable');
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npm test -- --run src/app/pages/connectors/list.spec.ts`
Expected: FAIL — cannot find module `./list`.

- [ ] **Step 3: Implement the list page**

`web/src/app/pages/connectors/list.ts`:
```typescript
import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { BusinessService } from '../../core/business.service';
import { Connector, ConnectorsService } from '../../core/connectors.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { Tone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { ConnectorFormComponent } from './connector-form';

// Connectors management page: list external connectors for the selected business with their
// sync health, connect new ones, test/enable-disable/rotate/delete. Business selection mirrors
// the approvals queue (same /api/v1/businesses list, persisted via CurrentBusinessService).
// Mutations update the row in place; delete uses an inline type-free confirm panel that names
// the connector and shows how many tickets will be detached to native.
@Component({
  selector: 'app-connectors-list',
  imports: [FormsModule, DatePipe, PageHeader, StatusPill, EmptyState, Spinner, ConnectorFormComponent],
  template: `
    <div class="mf-card" data-testid="connectors-page">
      <mf-page-header title="Connectors" [subtitle]="items().length + ' connected'">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="connectors-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select id="biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Connect a system' }}
          </button>
        </div>
      </div>

      @if (showAdd() && businessId()) {
        <app-connector-form mode="create" [businessId]="businessId()"
                            (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }

      <div class="mf-table" data-testid="connectors-list">
        <div class="mf-tr mf-th">
          <span style="width:80px">Type</span>
          <span style="flex:1">Name</span>
          <span style="width:110px">Health</span>
          <span style="width:90px">Tickets</span>
          <span style="width:110px">Reconciled</span>
          <span style="width:280px"></span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="connector-row" [attr.data-connector-id]="c.id">
            <span style="width:80px;text-transform:capitalize">{{ c.type }}</span>
            <span style="flex:1" data-testid="connector-name">
              {{ c.display_name }}
              <span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ c.base_url }}</span>
            </span>
            <span style="width:110px" data-testid="connector-health">
              <mf-status-pill [tone]="healthTone(c.health.state)" [label]="healthLabel(c.health.state)" />
            </span>
            <span style="width:90px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.health.linked_ticket_count }}</span>
            <span style="width:110px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.last_reconciled_at ? (c.last_reconciled_at | date: 'short') : '—' }}</span>
            <span style="width:280px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (confirmDeleteId() === c.id) {
                <span class="mf-err" data-testid="connector-delete-confirm" style="font-size:var(--mf-fs-xs);align-self:center">
                  Delete {{ c.display_name }}? Detaches {{ c.health.linked_ticket_count }} ticket(s).
                </span>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-delete-no" (click)="confirmDeleteId.set('')">Cancel</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete-yes" (click)="remove(c)">Delete</button>
              } @else {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-test" (click)="test(c)">Test</button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-toggle" (click)="toggle(c)">
                  {{ c.status === 'enabled' ? 'Disable' : 'Enable' }}
                </button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-rotate" (click)="rotateId.set(rotateId() === c.id ? '' : c.id)">Rotate</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete" (click)="confirmDeleteId.set(c.id)">Delete</button>
              }
            </span>
            @if (rotateId() === c.id) {
              <div style="flex:1 1 100%">
                <app-connector-form mode="rotate" [businessId]="businessId()" [connectorId]="c.id"
                                    (saved)="onRotated()" (cancelled)="rotateId.set('')" />
              </div>
            }
          </div>
        }
        @if (!items().length) {
          <mf-empty-state title="No connectors yet" data-testid="connectors-empty">
            Connect Jira or Zendesk to sync tickets with an external system.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="connectors-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class ConnectorsListComponent implements OnInit, OnDestroy {
  private bizApi = inject(BusinessService);
  private api = inject(ConnectorsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Connector[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  rotateId = signal<string>('');
  confirmDeleteId = signal<string>('');

  private timer: ReturnType<typeof setInterval> | undefined;

  healthTone(state: string): Tone {
    return state === 'healthy' ? 'success' : state === 'degraded' ? 'warn' : 'neutral';
  }
  healthLabel(state: string): string {
    return state.charAt(0).toUpperCase() + state.slice(1);
  }

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
    this.timer = setInterval(() => this.reload(), 20000);
  }

  ngOnDestroy(): void {
    clearInterval(this.timer);
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
    this.rotateId.set('');
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load connectors');
        this.loading.set(false);
      },
    });
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Connector added');
    this.reload();
  }

  onRotated(): void {
    this.rotateId.set('');
    this.toast.success('Credential rotated');
    this.reload();
  }

  test(c: Connector): void {
    this.api.test(this.businessId(), c.id).subscribe({
      next: (res) => (res.ok ? this.toast.success('Connection OK') : this.toast.error('Test failed: ' + res.detail)),
      error: () => this.toast.error('Test failed'),
    });
  }

  toggle(c: Connector): void {
    const status = c.status === 'enabled' ? 'disabled' : 'enabled';
    this.api.update(this.businessId(), c.id, { status }).subscribe({
      next: (updated) => {
        this.items.update((xs) => xs.map((x) => (x.id === updated.id ? updated : x)));
        this.api.degradedCount.set(this.items().filter((x) => x.health.state !== 'healthy').length);
        this.toast.success(status === 'enabled' ? 'Enabled' : 'Disabled');
      },
      error: (e: HttpErrorResponse) => this.toast.error(e.status === 404 ? 'Not found' : 'Update failed'),
    });
  }

  remove(c: Connector): void {
    this.api.remove(this.businessId(), c.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== c.id));
        this.api.degradedCount.set(this.items().filter((x) => x.health.state !== 'healthy').length);
        this.confirmDeleteId.set('');
        this.toast.success('Connector deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd web && npm test -- --run src/app/pages/connectors/list.spec.ts`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/connectors/list.ts web/src/app/pages/connectors/list.spec.ts
git commit -m "feat(ui): connectors list page (health pill, test/toggle/rotate/delete-confirm)"
```

---

## Task 4: Nav item + route + badge

**Files:**
- Modify: `web/src/app/ui/nav.ts`
- Modify: `web/src/app/app.routes.ts`
- Modify: `web/src/app/app.ts`

- [ ] **Step 1: Add the nav item**

In `web/src/app/ui/nav.ts`, add the Connectors entry after Approvals:
```typescript
export const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard' },
  { label: 'Support', route: '/support', testid: 'nav-support' },
  { label: 'Approvals', route: '/approvals', testid: 'nav-approvals' },
  { label: 'Connectors', route: '/connectors', testid: 'nav-connectors' },
  { label: 'Accounting', route: '/accounting', testid: 'nav-accounting' },
];
```

- [ ] **Step 2: Add the route**

In `web/src/app/app.routes.ts`, add after the `approvals` route block (before `accounting`):
```typescript
  {
    path: 'connectors',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/connectors/list').then((m) => m.ConnectorsListComponent),
  },
```

- [ ] **Step 3: Stamp + poll the badge in `app.ts`**

In `web/src/app/app.ts`:

(a) Inject the service near the other injects (next to `private approvals = inject(ApprovalsService);`):
```typescript
  private connectors = inject(ConnectorsService);
```
…and add the import at the top:
```typescript
import { ConnectorsService } from './core/connectors.service';
```

(b) Replace the `navItemsWithBadge` computed with the two-badge version:
```typescript
  // Copy NAV_ITEMS (object spread — never mutate the shared array) and stamp the live
  // pending-approvals count and degraded-connector count onto their nav items for the
  // current business.
  readonly navItemsWithBadge = computed(() => {
    const approvals = this.approvals.pendingCount();
    const degraded = this.connectors.degradedCount();
    const hasBiz = !!this.currentBusiness.businessId();
    return NAV_ITEMS.map((item) => {
      if (item.route === '/approvals' && hasBiz && approvals > 0) return { ...item, badge: approvals };
      if (item.route === '/connectors' && hasBiz && degraded > 0) return { ...item, badge: degraded };
      return item;
    });
  });
```

(c) Extend the `ngOnInit` polling to also refresh the connectors count — update the initial refresh and the interval body:
```typescript
    if (this.auth.isAuthenticated()) {
      const id = this.currentBusiness.businessId();
      if (id) {
        this.approvals.refreshCount(id);
        this.connectors.refreshCount(id);
      }
      this.badgeTimer = setInterval(() => {
        const b = this.currentBusiness.businessId();
        if (b) {
          this.approvals.refreshCount(b);
          this.connectors.refreshCount(b);
        }
      }, 20000);
    }
```

- [ ] **Step 4: Build to verify wiring**

Run: `cd web && npm run build`
Expected: build succeeds (no template/DI errors). Also re-run the existing app spec if present: `npm test -- --run src/app/app.spec.ts` (expected PASS).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/ui/nav.ts web/src/app/app.routes.ts web/src/app/app.ts
git commit -m "feat(ui): Connectors nav item + route + degraded-connector badge"
```

---

## Task 5: Playwright e2e

**Files:**
- Create: `web/e2e/connectors.spec.ts`

- [ ] **Step 1: Write the e2e spec**

`web/e2e/connectors.spec.ts`:
```typescript
import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const connector = {
  id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme Jira', base_url: 'https://acme.atlassian.net',
  allow_private_base_url: false, config: {}, status: 'enabled', last_reconciled_at: null,
  created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
  health: { state: 'healthy', linked_ticket_count: 2, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null },
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('connectors: renders list with health pill', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.goto('/connectors');
  await expect(page.getByTestId('connector-name')).toContainText('Acme Jira');
  await expect(page.getByTestId('connector-health')).toContainText('Healthy');
});

test('connectors: create a connector', async ({ page }) => {
  await auth(page);
  let created = false;
  await page.route('**/api/v1/businesses/b1/connectors', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      return r.fulfill({ status: 201, json: connector });
    }
    return r.fulfill({ json: { items: created ? [connector] : [] } });
  });
  await page.goto('/connectors');
  await page.getByTestId('connector-add-toggle').click();
  await page.getByTestId('conn-display-name').fill('Acme Jira');
  await page.getByTestId('conn-base-url').fill('https://acme.atlassian.net');
  await page.getByTestId('conn-email').fill('a@b.c');
  await page.getByTestId('conn-api-token').fill('tok');
  await page.getByTestId('connector-form-submit').click();
  await expect(page.getByTestId('connector-name')).toContainText('Acme Jira');
});

test('connectors: test action shows a toast', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.route('**/api/v1/businesses/b1/connectors/c1/test', (r) => r.fulfill({ json: { ok: true, detail: 'ok' } }));
  await page.goto('/connectors');
  await page.getByTestId('connector-test').click();
  await expect(page.getByTestId('toast')).toContainText(/OK/i);
});

test('connectors: delete asks to confirm then removes the row', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.route('**/api/v1/businesses/b1/connectors/c1', (r) => {
    if (r.request().method() === 'DELETE') return r.fulfill({ status: 204, body: '' });
    return r.fulfill({ json: connector });
  });
  await page.goto('/connectors');
  await page.getByTestId('connector-delete').click();
  await expect(page.getByTestId('connector-delete-confirm')).toContainText('Detaches 2');
  await page.getByTestId('connector-delete-yes').click();
  await expect(page.getByTestId('connector-row')).toHaveCount(0);
});
```

- [ ] **Step 2: Run the e2e spec**

Bring up the dev stack first (backend `:8081` via `air`, frontend `:4300` via `ng serve`), then:
Run: `cd web && npx playwright test e2e/connectors.spec.ts`
Expected: 4 passed. (Routes are mocked, so the backend need not have real connectors; the frontend dev server on `:4300` must be running.)

- [ ] **Step 3: Commit**

```bash
git add web/e2e/connectors.spec.ts
git commit -m "test(ui): connectors e2e (list, create, test, delete-confirm)"
```

---

## Task 6: Eyes-on verification + full gate + close out

**Files:** none (verification only)

- [ ] **Step 1: Build + unit suite**

Run: `cd web && npm run build && npm test -- --run`
Expected: build OK; all Vitest specs pass (incl. the 3 new files).

- [ ] **Step 2: Browser eyes-on (light + dark) — REQUIRED for UI work**

With the dev stack up (`air` on `:8081`, `ng serve` on `:4300`, and `MANYFORGE_CONNECTOR_MASTER_KEY` set in `.air.env` so the backend enables connectors), log in at `http://localhost:4300` as `live-demo@manyforge.test` / `DevPassw0rd!` and verify in a real browser (via the Playwright MCP, `gstack` `$B`, or manual):
  - Navigate to **Connectors**. The nav item renders; with a degraded/disabled connector, the badge shows.
  - "Connect a system" form opens; credential fields render as masked password inputs.
  - Create a connector → it appears with a health pill. **Toggle the theme** (light ⇄ dark) and confirm the table, pills, and form all render correctly in BOTH (undefined CSS classes render silently — the unit/e2e gate will NOT catch a missing `.mf-*` class).
  - Test / Disable / Enable / Rotate / Delete-confirm each behave and toast as expected.

Record what you saw (a screenshot of each theme is ideal). Do not mark this step done on unit tests alone.

- [ ] **Step 3: Lint/format if the project has a frontend linter**

Run: `cd web && npm run lint` (skip if no `lint` script exists).
Expected: no findings.

- [ ] **Step 4: Update bd + close**

```bash
bd update manyforge-4zs.3 --notes "Frontend connectors UI landed (service, list, create/rotate form, nav+badge, Vitest + Playwright). Backend + frontend both complete."
```
If both the backend plan and this plan are fully executed and gates are green, close the issue:
```bash
bd close manyforge-4zs.3
```

- [ ] **Step 5: Push**

```bash
git pull --rebase
bd dolt push
git push
git status   # MUST show "up to date with origin"
```

---

## Notes for the implementer

- **Run the dev stack:** backend `set -a; . ./.air.env; set +a; air` (serves `:8081`); frontend `cd web && npm start` (serves `:4300`, proxies `/api → :8081`). Log in fresh after a backend restart (dev JWT keys are ephemeral).
- **Connectors disabled without the master key:** to exercise real connect/test end-to-end (not just mocked e2e), add `MANYFORGE_CONNECTOR_MASTER_KEY` to `.air.env` and restart `air`. Mocked Vitest/Playwright specs do NOT need it.
- **Undefined CSS classes render silently.** Only reuse the `mf-*` classes listed in the conventions block; verify every new visual in both themes (Task 6 Step 2). This is the one failure mode the green unit/e2e gate cannot catch.
- **Native `<select>` needs `FormsModule`** + `[ngModel]`/`[(ngModel)]`; `.mf-table`/`.mf-tr` are DIV-flex (align with fixed-width `<span>`), not real tables.
- **`npm test`** is `ng test` via `@angular/build:unit-test` (Vitest globals); run a single file with `npm test -- --run <path>`. **`npm run e2e`** is `playwright test` (base `http://localhost:4300`).
- **bd-journal gotcha:** commit with explicit `git add <paths>`; never `git add -A` (it would sweep up untracked `CLAUDE.md`/lock artifacts and the auto-staged `.beads/issues.jsonl`).
```
