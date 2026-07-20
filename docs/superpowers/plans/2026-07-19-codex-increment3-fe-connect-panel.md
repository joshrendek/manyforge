# Codex Increment 3 — FE Connect Panel + Model Catalog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a frontend "Sign in with ChatGPT" connect panel (device-code primary, PKCE-paste fallback) plus a curated `openai_codex` model catalog, connection-health display, and review-setup selectability — so a user can connect their ChatGPT subscription and use it in code reviews end-to-end.

**Architecture:** The Increment 1–2 backend already ships the OAuth connect endpoints, token refresh, per-run mint, and connection-health read fields. This increment is the FE surface + one backend seed/filter: (1) a `0097` migration seeding `$0`-priced, non-`*-pro` codex models into the existing `model_pricing` catalog plus a defensive `*-pro` filter in `ListModels`; (2) a dedicated `codex-connect.ts` Angular component owning the OAuth state machine, rendered by `credential-form.ts` when the provider is `openai_codex`; (3) a health badge + Reconnect in the credential list; (4) `openai_codex` added to the review-setup and agent-form provider pickers, where it uses the existing catalog-`<select>` path automatically.

**Tech Stack:** Go + pgx + sqlc + chi (backend); Angular 21 (standalone components, signals, `@if`/`@for`, `inject()`, decorator `@Input`/`@Output`, template-driven `[(ngModel)]` for form fields); Vitest + Angular TestBed + `HttpTestingController` (FE unit); Playwright (e2e). PostgreSQL migrations in `.up.sql`/`.down.sql` pairs.

## Global Constraints

- **Angular idioms (match existing code exactly):** standalone components with `imports: [...]`; state in `signal()`/`computed()`; `@if`/`@for (x of xs(); track x.id)` control flow (never `*ngIf`/`*ngFor`); field-level `inject()` (never constructor DI); decorator `@Input()`/`@Output() = new EventEmitter<>()` (not `input()`/`output()`); reactive/derived state in signals BUT plain class properties with two-way `[(ngModel)]` for template-driven form fields; `HttpClient` returning RxJS `Observable`s the component `.subscribe()`s.
- **FE tests:** Vitest (`import { describe, it, expect, beforeEach } from 'vitest'`), Angular `TestBed`, `provideHttpClient()` + `provideHttpClientTesting()`, `HttpTestingController`. Drive components by setting signals/props directly and calling methods, then assert on outbound HTTP (`http.expectOne`) and on signal getters / `data-testid` DOM queries.
- **Provider name:** the literal is `openai_codex` everywhere (PG enum, `AIProvider` union, catalog `provider` column, provider option lists). User-facing label: `OpenAI Codex (ChatGPT)`.
- **Backend gates before commit (Task 1 only):** `go build ./...`, `go test ./internal/agents/...`, `go test -tags contract ./cmd/...`, `make lint`, `make sec-test`. No `make generate` is needed — Task 1 adds no sqlc query.
- **Codex `base_url` is never user-exposed** — the ChatGPT backend base is fixed server-side; the connect body omits `base_url`, which also keeps `allow_private_base_url=false` (SSRF surface stays closed).
- **Codex completions are flat-rate** (ChatGPT plan quota, not metered `api.openai.com`) → catalog pricing is `0` cents.
- **Git:** work on the single branch `codex-inc3-fe-connect-panel` (already created off `master`). Commit per task. Do NOT add a `Co-Authored-By` trailer.

---

### Task 1: Backend — seed the `openai_codex` model catalog + defensive `*-pro` filter

**Files:**
- Create: `migrations/0097_codex_model_catalog.up.sql`
- Create: `migrations/0097_codex_model_catalog.down.sql`
- Modify: `internal/agents/metadata.go` (add `filterCodexPro`, apply in `ListModels`)
- Create: `internal/agents/metadata_codex_test.go`

**Interfaces:**
- Consumes: existing `model_pricing` table (`migrations/0038`), `ModelCatalog.ListModels(ctx) ([]ModelInfo, error)`, `ModelInfo{Provider, ModelID string}`.
- Produces: `openai_codex` catalog rows surfaced by `GET /agents/models`; `filterCodexPro([]ModelInfo) []ModelInfo` (drops `openai_codex` `*-pro`).

- [ ] **Step 1: Write the failing Go test for the filter**

Create `internal/agents/metadata_codex_test.go`:

```go
package agents

import "testing"

func TestFilterCodexPro(t *testing.T) {
	in := []ModelInfo{
		{Provider: "openai_codex", ModelID: "gpt-5-codex"},
		{Provider: "openai_codex", ModelID: "gpt-5"},
		{Provider: "openai_codex", ModelID: "gpt-5-pro"}, // dropped: 403s on ChatGPT auth
		{Provider: "openai", ModelID: "gpt-4o-pro"},      // kept: non-codex -pro is fine
	}
	got := filterCodexPro(in)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ModelID)
	}
	want := []string{"gpt-5-codex", "gpt-5", "gpt-4o-pro"}
	if len(ids) != len(want) {
		t.Fatalf("filterCodexPro: got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("filterCodexPro[%d]: got %q, want %q (full: %v)", i, ids[i], want[i], ids)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agents/ -run TestFilterCodexPro`
Expected: FAIL — `undefined: filterCodexPro`.

- [ ] **Step 3: Implement `filterCodexPro` and apply it in `ListModels`**

In `internal/agents/metadata.go`, ensure `strings` is imported (add to the import block if absent), then add the helper below the `ListModels` method:

```go
// filterCodexPro drops openai_codex *-pro models: the ChatGPT-account backend refuses them
// with a 403 even when advertised, so they must never reach the model picker. Defense in depth
// on top of not seeding them (migration 0097). Non-codex providers are untouched.
func filterCodexPro(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if m.Provider == "openai_codex" && strings.HasSuffix(m.ModelID, "-pro") {
			continue
		}
		out = append(out, m)
	}
	return out
}
```

Then change the final `return` of `ListModels` (currently `return out, nil`) to:

```go
	return filterCodexPro(out), nil
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/agents/ -run TestFilterCodexPro`
Expected: PASS.

- [ ] **Step 5: Write the seed migration (up + down)**

Create `migrations/0097_codex_model_catalog.up.sql`:

```sql
-- Seed OpenAI Codex (ChatGPT-subscription) model presets into the system model catalog.
-- Completions run against the flat-rate ChatGPT plan (chatgpt.com/backend-api/codex), not
-- metered api.openai.com, so input/output pricing is 0. *-pro variants are intentionally
-- omitted — the ChatGPT-account backend refuses them with a 403; filterCodexPro in
-- internal/agents/metadata.go also drops any that are ever added, as defense in depth.
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5-codex', 'openai_codex', 'GPT-5 Codex (ChatGPT)', 400000, 0, 0, true),
    ('gpt-5',       'openai_codex', 'GPT-5 (ChatGPT)',        400000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
```

Create `migrations/0097_codex_model_catalog.down.sql`:

```sql
DELETE FROM model_pricing WHERE provider = 'openai_codex';
```

- [ ] **Step 6: Verify the build and the wider agents suite**

Run: `go build ./... && go test ./internal/agents/...`
Expected: PASS (the enum-pin `TestKnownProvidersTrackEnum` and `internal/security_regression` model-pricing pins are unaffected — they pin the 0038 seed and the credential enum, not the `model_pricing.provider` value set).

- [ ] **Step 7: Verify contract + lint + security gates**

Run: `go test -tags contract ./cmd/... && make lint && make sec-test`
Expected: PASS. (The `openai_codex` provider is already in the OpenAPI enum from Increment 1; no contract change here.)

- [ ] **Step 8: Commit**

```bash
git add migrations/0097_codex_model_catalog.up.sql migrations/0097_codex_model_catalog.down.sql \
        internal/agents/metadata.go internal/agents/metadata_codex_test.go
git commit -m "feat(codex): seed openai_codex model catalog (\$0, non-pro) + *-pro filter"
```

---

### Task 2: FE service — `openai_codex` provider, health fields, and codex connect methods

**Files:**
- Modify: `web/src/app/core/ai-credentials.service.ts`
- Create: `web/src/app/core/ai-credentials.service.spec.ts`

**Interfaces:**
- Consumes: existing `AICredentialsService` (`inject(HttpClient)`, `base(businessId)` = `/api/v1/businesses/${businessId}/ai_credentials`).
- Produces (later tasks rely on these exact names/types):
  - `AIProvider` union now includes `'openai_codex'`.
  - `AICredential` gains optional `chatgpt_plan?: string`, `connection_status?: 'connected' | 'disconnected'`, `oauth_access_expiry?: string`.
  - `CodexConnectBody { default_model: string; base_url?: string; max_concurrent_lanes?: number }`.
  - `CodexDeviceStart { pending_id; user_code; verification_uri; verification_uri_complete; interval; expires_in }`.
  - `CodexPKCEStart { pending_id; authorize_url }`.
  - `CodexConnectStatus { status: 'pending'|'approved'|'expired'|'denied'; credential_id? }`.
  - Methods `codexDeviceStart`, `codexDeviceStatus`, `codexPKCEStart`, `codexPKCEExchange`.

- [ ] **Step 1: Write the failing service spec**

Create `web/src/app/core/ai-credentials.service.spec.ts`:

```ts
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { AICredentialsService } from './ai-credentials.service';

describe('AICredentialsService codex methods', () => {
  let svc: AICredentialsService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(AICredentialsService);
    http = TestBed.inject(HttpTestingController);
  });

  it('codexDeviceStart POSTs the connect body to /codex/device/start', () => {
    let ok = false;
    svc.codexDeviceStart('b1', { default_model: 'gpt-5-codex', max_concurrent_lanes: 4 }).subscribe(() => (ok = true));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5-codex' }));
    req.flush({ pending_id: 'p1', user_code: 'ABCD-1234', verification_uri: 'https://x', verification_uri_complete: 'https://x?c=ABCD-1234', interval: 5, expires_in: 900 });
    expect(ok).toBe(true);
  });

  it('codexDeviceStatus GETs the pending status', () => {
    let status = '';
    svc.codexDeviceStatus('b1', 'p1').subscribe((s) => (status = s.status));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status');
    expect(req.request.method).toBe('GET');
    req.flush({ status: 'approved', credential_id: 'cred9' });
    expect(status).toBe('approved');
  });

  it('codexPKCEExchange POSTs pending_id + redirect_url to /codex/pkce/exchange', () => {
    svc.codexPKCEExchange('b1', 'p1', 'http://localhost:1455/auth/callback?code=z&state=p1').subscribe();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ pending_id: 'p1', redirect_url: 'http://localhost:1455/auth/callback?code=z&state=p1' });
    req.flush({ status: 'approved', credential_id: 'cred9' });
  });
});
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npx vitest run src/app/core/ai-credentials.service.spec.ts`
Expected: FAIL — `codexDeviceStart` / `codexDeviceStatus` / `codexPKCEExchange` do not exist.

- [ ] **Step 3: Add the provider, health fields, DTOs, and methods**

In `web/src/app/core/ai-credentials.service.ts`, change the `AIProvider` union to add `openai_codex`:

```ts
export type AIProvider = 'anthropic' | 'openai' | 'ollama' | 'vllm' | 'openrouter' | 'huggingface' | 'openai_codex';
```

Add the three optional health fields to the `AICredential` interface (after `updated_at`):

```ts
  // openai_codex-only connection health (omitted/empty for other providers; never secret-bearing).
  chatgpt_plan?: string;
  connection_status?: 'connected' | 'disconnected';
  oauth_access_expiry?: string;
```

Add the codex DTOs after `CreateAICredentialBody`:

```ts
export interface CodexConnectBody {
  default_model: string;
  base_url?: string;
  max_concurrent_lanes?: number;
}
export interface CodexDeviceStart {
  pending_id: string;
  user_code: string;
  verification_uri: string;
  verification_uri_complete: string;
  interval: number;
  expires_in: number;
}
export interface CodexPKCEStart {
  pending_id: string;
  authorize_url: string;
}
export interface CodexConnectStatus {
  status: 'pending' | 'approved' | 'expired' | 'denied';
  credential_id?: string;
}
```

Add the four methods inside the `AICredentialsService` class (after `remove`):

```ts
  codexDeviceStart(businessId: string, body: CodexConnectBody): Observable<CodexDeviceStart> {
    return this.http.post<CodexDeviceStart>(`${this.base(businessId)}/codex/device/start`, body);
  }
  codexDeviceStatus(businessId: string, pendingId: string): Observable<CodexConnectStatus> {
    return this.http.get<CodexConnectStatus>(`${this.base(businessId)}/codex/device/${pendingId}/status`);
  }
  codexPKCEStart(businessId: string, body: CodexConnectBody): Observable<CodexPKCEStart> {
    return this.http.post<CodexPKCEStart>(`${this.base(businessId)}/codex/pkce/start`, body);
  }
  codexPKCEExchange(businessId: string, pendingId: string, redirectUrl: string): Observable<CodexConnectStatus> {
    return this.http.post<CodexConnectStatus>(`${this.base(businessId)}/codex/pkce/exchange`, {
      pending_id: pendingId,
      redirect_url: redirectUrl,
    });
  }
```

- [ ] **Step 4: Run the spec to verify it passes**

Run: `cd web && npx vitest run src/app/core/ai-credentials.service.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/core/ai-credentials.service.ts web/src/app/core/ai-credentials.service.spec.ts
git commit -m "feat(codex): FE credential service — openai_codex provider, health fields, connect methods"
```

---

### Task 3: FE connect panel component (`codex-connect.ts`)

**Files:**
- Create: `web/src/app/pages/credentials/ai/codex-connect.ts`
- Create: `web/src/app/pages/credentials/ai/codex-connect.spec.ts`

**Interfaces:**
- Consumes: `AICredentialsService` (Task 2 methods + DTOs), `AgentsService.models(businessId): Observable<{ items: ModelDescriptor[] }>` from `../../core/agents.service`.
- Produces: `CodexConnectComponent` (selector `app-codex-connect`) with `@Input() businessId: string`, `@Output() connected = new EventEmitter<string>()` (emits `credential_id`), `@Output() cancelled = new EventEmitter<void>()`. Public methods used by tests: `startDevice()`, `pollOnce()`, `startPaste()`, `submitPaste()`, `reset()`; public signals `phase`, `device`, `model`, `codexModels`, `error`, `showPaste`.

- [ ] **Step 1: Write the failing component spec**

Create `web/src/app/pages/credentials/ai/codex-connect.spec.ts`:

```ts
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { CodexConnectComponent } from './codex-connect';

describe('CodexConnectComponent', () => {
  let fixture: ComponentFixture<CodexConnectComponent>;
  let http: HttpTestingController;

  function mount(): CodexConnectComponent {
    fixture = TestBed.createComponent(CodexConnectComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges(); // triggers ngOnInit → models() fetch
    http.expectOne('/api/v1/businesses/b1/agents/models').flush({
      items: [
        { provider: 'openai_codex', model_id: 'gpt-5-codex' },
        { provider: 'anthropic', model_id: 'claude-opus-4-8' },
      ],
    });
    fixture.detectChanges();
    return fixture.componentInstance;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [CodexConnectComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  it('lists only openai_codex models in the picker', () => {
    const c = mount();
    expect(c.codexModels().map((m) => m.model_id)).toEqual(['gpt-5-codex']);
  });

  it('startDevice posts the body and moves to the authorizing phase showing the user code', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5-codex' }));
    req.flush({ pending_id: 'p1', user_code: 'ABCD-1234', verification_uri: 'https://x', verification_uri_complete: 'https://x?c=ABCD', interval: 5, expires_in: 900 });
    fixture.detectChanges();
    expect(c.phase()).toBe('authorizing');
    expect(fixture.nativeElement.querySelector('[data-testid="codex-user-code"]')?.textContent).toContain('ABCD-1234');
  });

  it('pollOnce emits connected with the credential id when approved', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'approved', credential_id: 'cred9' });
    expect(emitted).toBe('cred9');
  });

  it('pollOnce moves to the expired phase when the code expires', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'expired' });
    expect(c.phase()).toBe('expired');
  });

  it('submitPaste exchanges the pasted redirect URL and emits connected', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'http://localhost:1455/auth/callback?code=z&state=p1';
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.submitPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange').flush({ status: 'approved', credential_id: 'cred9' });
    expect(emitted).toBe('cred9');
  });
});
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/codex-connect.spec.ts`
Expected: FAIL — cannot resolve `./codex-connect`.

- [ ] **Step 3: Implement the component**

Create `web/src/app/pages/credentials/ai/codex-connect.ts`:

```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnDestroy, OnInit, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  AICredentialsService,
  CodexConnectBody,
  CodexConnectStatus,
  CodexDeviceStart,
  CodexPKCEStart,
} from '../../../core/ai-credentials.service';
import { AgentsService, ModelDescriptor } from '../../../core/agents.service';

// CodexConnectComponent drives the "Sign in with ChatGPT" flow for an openai_codex credential.
// Device-code is primary (no URL copy-paste); PKCE-paste is a fallback for accounts where
// device-code login is disabled. The backend upserts on (business_id, provider), so re-running
// this flow for an already-connected account replaces it in place — this component is reused
// verbatim for "Reconnect".
@Component({
  selector: 'app-codex-connect',
  imports: [FormsModule],
  template: `
    <div class="mf-card" data-testid="codex-connect" style="padding:12px;margin-top:8px">
      @if (phase() === 'configure') {
        <div class="mf-field">
          <label for="codex-model">Model</label>
          <select id="codex-model" class="mf-select" data-testid="codex-model"
                  [ngModel]="model()" (ngModelChange)="model.set($event)" name="codex-model">
            <option value="" disabled>Choose a model…</option>
            @for (m of codexModels(); track m.model_id) {
              <option [value]="m.model_id">{{ m.model_id }}</option>
            }
          </select>
        </div>
        <div class="mf-field">
          <label for="codex-lanes">Max concurrent lanes</label>
          <input id="codex-lanes" class="mf-input" type="number" min="1" max="16"
                 name="codex-lanes" [(ngModel)]="maxLanes" />
        </div>
        @if (error()) { <p class="mf-err" data-testid="codex-error">{{ error() }}</p> }
        <div style="display:flex;gap:8px">
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-signin"
                  [disabled]="submitting() || !model()" (click)="startDevice()">
            {{ submitting() ? 'Starting…' : 'Sign in with ChatGPT' }}
          </button>
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        </div>
      }

      @if (phase() === 'authorizing') {
        <div data-testid="codex-authorizing">
          <p>Enter this code at ChatGPT to authorize this connection:</p>
          <p class="mf-code" data-testid="codex-user-code" style="font-size:var(--mf-fs-lg);letter-spacing:2px">{{ device()?.user_code }}</p>
          <a class="mf-btn mf-btn-primary mf-btn-sm" [href]="device()?.verification_uri_complete"
             target="_blank" rel="noopener" data-testid="codex-open">Open ChatGPT</a>
          <p class="mf-hint" data-testid="codex-waiting">Waiting for approval…</p>
          @if (error()) { <p class="mf-err" data-testid="codex-error">{{ error() }}</p> }
          <details style="margin-top:8px">
            <summary data-testid="codex-paste-toggle">Trouble signing in? Paste a link instead</summary>
            <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="codex-paste-start"
                    style="margin:6px 0" (click)="startPaste()">Open sign-in page</button>
            @if (showPaste()) {
              <div class="mf-field">
                <label for="codex-paste">Paste the redirect URL from the address bar</label>
                <input id="codex-paste" class="mf-input" type="text" name="codex-paste" [(ngModel)]="pasteUrl"
                       placeholder="http://localhost:1455/auth/callback?code=…&state=…" data-testid="codex-paste-url" />
              </div>
              <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-paste-submit"
                      [disabled]="submitting() || !pasteUrl.trim()" (click)="submitPaste()">Finish sign-in</button>
            }
          </details>
        </div>
      }

      @if (phase() === 'expired') {
        <div data-testid="codex-expired">
          <p class="mf-err">{{ error() || 'The code expired before it was approved.' }}</p>
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-retry" (click)="reset()">Try again</button>
        </div>
      }
    </div>
  `,
})
export class CodexConnectComponent implements OnInit, OnDestroy {
  @Input() businessId = '';
  @Output() connected = new EventEmitter<string>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AICredentialsService);
  private agents = inject(AgentsService);

  phase = signal<'configure' | 'authorizing' | 'expired'>('configure');
  codexModels = signal<ModelDescriptor[]>([]);
  model = signal<string>('');
  maxLanes = 4;
  device = signal<CodexDeviceStart | null>(null);
  pkce = signal<CodexPKCEStart | null>(null);
  showPaste = signal(false);
  pasteUrl = '';
  submitting = signal(false);
  error = signal('');

  private pollTimer: ReturnType<typeof setTimeout> | null = null;

  ngOnInit(): void {
    this.agents.models(this.businessId).subscribe({
      next: (r) => this.codexModels.set((r.items ?? []).filter((m) => m.provider === 'openai_codex')),
      error: () => this.codexModels.set([]),
    });
  }

  ngOnDestroy(): void {
    this.stopPolling();
  }

  private body(): CodexConnectBody {
    return {
      default_model: this.model(),
      max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.maxLanes) || 4))),
    };
  }

  startDevice(): void {
    if (this.submitting() || !this.model()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api.codexDeviceStart(this.businessId, this.body()).subscribe({
      next: (d) => {
        this.device.set(d);
        this.phase.set('authorizing');
        this.submitting.set(false);
        this.scheduleNextPoll();
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  // pollOnce fetches the pending status once and applies it. Production schedules repeated calls
  // via scheduleNextPoll; tests call it directly and flush the HTTP response.
  pollOnce(): void {
    const d = this.device();
    if (!d) return;
    this.api.codexDeviceStatus(this.businessId, d.pending_id).subscribe({
      next: (s) => this.applyStatus(s),
      error: () => this.scheduleNextPoll(), // transient error: keep polling
    });
  }

  startPaste(): void {
    if (!this.model()) return;
    this.error.set('');
    this.api.codexPKCEStart(this.businessId, this.body()).subscribe({
      next: (p) => {
        this.pkce.set(p);
        this.showPaste.set(true);
        window.open(p.authorize_url, '_blank', 'noopener');
      },
      error: (e: HttpErrorResponse) => this.error.set(this.describe(e)),
    });
  }

  submitPaste(): void {
    const p = this.pkce();
    if (!p || !this.pasteUrl.trim()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api.codexPKCEExchange(this.businessId, p.pending_id, this.pasteUrl.trim()).subscribe({
      next: (s) => {
        this.submitting.set(false);
        this.applyStatus(s);
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  reset(): void {
    this.stopPolling();
    this.device.set(null);
    this.pkce.set(null);
    this.showPaste.set(false);
    this.pasteUrl = '';
    this.error.set('');
    this.phase.set('configure');
  }

  private applyStatus(s: CodexConnectStatus): void {
    if (s.status === 'approved' && s.credential_id) {
      this.stopPolling();
      this.connected.emit(s.credential_id);
      return;
    }
    if (s.status === 'expired') {
      this.stopPolling();
      this.phase.set('expired');
      return;
    }
    if (s.status === 'denied') {
      this.stopPolling();
      this.error.set('Sign-in was denied. Try again.');
      this.phase.set('expired');
      return;
    }
    this.scheduleNextPoll(); // pending
  }

  private scheduleNextPoll(): void {
    const d = this.device();
    if (!d) return;
    this.stopPolling();
    this.pollTimer = setTimeout(() => this.pollOnce(), Math.max(1, d.interval) * 1000);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Could not start sign-in. Check the model and try again.';
    }
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    if (e.status === 409) return 'A ChatGPT connection already exists for this business.';
    return 'Could not connect. Please try again.';
  }
}
```

- [ ] **Step 4: Run the spec to verify it passes**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/codex-connect.spec.ts`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/credentials/ai/codex-connect.ts web/src/app/pages/credentials/ai/codex-connect.spec.ts
git commit -m "feat(codex): connect panel component — device-code + PKCE-paste state machine"
```

---

### Task 4: Wire the panel into `credential-form.ts`

**Files:**
- Modify: `web/src/app/pages/credentials/ai/credential-form.ts`
- Modify: `web/src/app/pages/credentials/ai/credential-form.spec.ts`

**Interfaces:**
- Consumes: `CodexConnectComponent` (Task 3).
- Produces: the form renders `<app-codex-connect>` when `provider() === 'openai_codex'`; `@Output() saved` is now `EventEmitter<void>()` (payload dropped — its only consumer, `list.ts#onCreated`, ignores it); new `@Input() initialProvider: AIProvider | null = null` presets the provider on init (used by Reconnect in Task 5).

- [ ] **Step 1: Write the failing form spec additions**

Add these tests to `web/src/app/pages/credentials/ai/credential-form.spec.ts` (inside the existing `describe`):

```ts
  it('renders the codex connect panel when provider is openai_codex', () => {
    const c = fixture.componentInstance;
    c.onProviderChange('openai_codex');
    fixture.detectChanges();
    // ngOnInit of the child fetches the model catalog
    http.expectOne('/api/v1/businesses/b1/agents/models').flush({ items: [] });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="codex-connect"]')).toBeTruthy();
    // the api-key field is hidden for codex
    expect(fixture.nativeElement.querySelector('[data-testid="credential-form-submit"]')).toBeNull();
  });

  it('starts on the codex provider when initialProvider is set', () => {
    const f = TestBed.createComponent(CredentialFormComponent);
    f.componentInstance.businessId = 'b1';
    f.componentInstance.initialProvider = 'openai_codex';
    f.detectChanges();
    http.expectOne('/api/v1/businesses/b1/agents/models').flush({ items: [] });
    f.detectChanges();
    expect(f.componentInstance.provider()).toBe('openai_codex');
  });
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/credential-form.spec.ts`
Expected: FAIL — no `codex-connect` element; `initialProvider` undefined.

- [ ] **Step 3: Modify the form**

In `web/src/app/pages/credentials/ai/credential-form.ts`:

1. Add the import:

```ts
import { CodexConnectComponent } from './codex-connect';
```

2. Add `CodexConnectComponent` to the component `imports` array:

```ts
  imports: [FormsModule, CodexConnectComponent],
```

3. Add the codex `<option>` to the provider `<select>` (after the huggingface option):

```html
          <option value="openai_codex">OpenAI Codex (ChatGPT)</option>
```

4. Wrap the existing form fields so the codex branch replaces them. Immediately AFTER the provider `<div class="mf-field">…</div>` block that holds the `<select>`, wrap the REMAINING fields (api key, default model, base url, private-url checkbox, max lanes, error, submit/cancel buttons) in an `@else`, and add the codex branch before it:

```html
      @if (provider() === 'openai_codex') {
        <app-codex-connect [businessId]="businessId"
                           (connected)="saved.emit()" (cancelled)="cancelled.emit()" />
      } @else {
        <!-- existing api-key / default-model / base-url / checkbox / lanes / error / buttons here -->
      }
```

5. Change the `saved` output type and its emit. Replace:

```ts
  @Output() saved = new EventEmitter<AICredential>();
```

with:

```ts
  @Output() saved = new EventEmitter<void>();
```

and in `submit()`'s success handler replace `this.saved.emit(c);` with:

```ts
          this.saved.emit();
```

(The unused `c` parameter can stay as `(c) =>` or be simplified to `() =>`; if `AICredential` becomes an unused import, drop it from the import line to satisfy lint.)

6. Add the `initialProvider` input and an `ngOnInit` to apply it. Add `OnInit` to the imports from `@angular/core`, declare `implements OnInit`, and add:

```ts
  @Input() initialProvider: AIProvider | null = null;

  ngOnInit(): void {
    if (this.initialProvider) {
      this.provider.set(this.initialProvider);
    }
  }
```

- [ ] **Step 4: Run the form spec + the list spec to verify they pass**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/credential-form.spec.ts src/app/pages/credentials/ai/list.spec.ts`
Expected: PASS. (The existing `list.spec` binds `(saved)="onCreated()"` and ignores the payload, so the `void` change is compatible.)

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/credentials/ai/credential-form.ts web/src/app/pages/credentials/ai/credential-form.spec.ts
git commit -m "feat(codex): render connect panel in the credential form for openai_codex"
```

---

### Task 5: Connection-health badge + Reconnect in `list.ts`

**Files:**
- Modify: `web/src/app/pages/credentials/ai/list.ts`
- Modify: `web/src/app/pages/credentials/ai/list.spec.ts`

**Interfaces:**
- Consumes: `AICredential` health fields (Task 2), `credential-form`'s `initialProvider` input (Task 4).
- Produces: a codex health badge (`data-testid="codex-health"`) + a `Reconnect` button (`data-testid="codex-reconnect"`) shown when a codex credential is not connected; Reconnect opens the add form preset to `openai_codex`.

- [ ] **Step 1: Write the failing list spec additions**

Add to `web/src/app/pages/credentials/ai/list.spec.ts`. First add a disconnected-codex fixture near the top-of-file fixtures:

```ts
const codexCredentials = {
  items: [
    {
      id: 'cx1', business_id: 'b1', provider: 'openai_codex', base_url: '', default_model: 'gpt-5-codex',
      allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '', updated_at: '',
      chatgpt_plan: 'plus', connection_status: 'disconnected', oauth_access_expiry: '2026-01-01T00:00:00Z',
    },
  ],
};
```

Then add a test that mounts with the codex fixture (mirroring the existing `mount()` but flushing `codexCredentials`):

```ts
  it('shows a codex health badge and a Reconnect button when disconnected', () => {
    const f = TestBed.createComponent(AICredentialsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/ai_credentials').flush(codexCredentials);
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="codex-health"]')?.textContent).toContain('disconnected');
    const reconnect = el.querySelector('[data-testid="codex-reconnect"]') as HTMLButtonElement;
    expect(reconnect).toBeTruthy();
    reconnect.click();
    f.detectChanges();
    // Reconnect opens the add form; the child form fetches the model catalog on init.
    mock.expectOne('/api/v1/businesses/b1/agents/models').flush({ items: [] });
    f.detectChanges();
    expect(el.querySelector('[data-testid="codex-connect"]')).toBeTruthy();
  });
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/list.spec.ts`
Expected: FAIL — no `codex-health` / `codex-reconnect` elements.

- [ ] **Step 3: Modify the list**

In `web/src/app/pages/credentials/ai/list.ts`:

1. Add a `reconnectProvider` signal alongside the others:

```ts
  reconnectProvider = signal<AIProvider | null>(null);
```

and import `AIProvider` from the service if not already imported:

```ts
import { AICredential, AICredentialsService, AIProvider } from '../../../core/ai-credentials.service';
```

2. In the provider table cell, add the health badge after `{{ c.provider }}`:

```html
            <span style="width:120px;text-transform:capitalize" data-testid="credential-provider">{{ c.provider }}
              @if (c.provider === 'openai_codex') {
                <span data-testid="codex-health"
                      [style.color]="c.connection_status === 'connected' ? 'var(--mf-ok, green)' : 'var(--mf-danger, crimson)'"
                      style="font-size:var(--mf-fs-xs);display:block">
                  {{ c.connection_status || 'unknown' }}@if (c.chatgpt_plan) { · {{ c.chatgpt_plan }} }
                </span>
              }
            </span>
```

3. In the base-URL cell, for codex rows show the token expiry instead of the (hidden) base URL:

```html
            <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">
              @if (c.provider === 'openai_codex') { {{ c.oauth_access_expiry ? ('expires ' + c.oauth_access_expiry) : '—' }} }
              @else { {{ c.base_url || '—' }} }
            </span>
```

4. In the actions cell, add a Reconnect button before the delete controls, shown only for a non-connected codex credential:

```html
              @if (c.provider === 'openai_codex' && c.connection_status !== 'connected') {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="codex-reconnect" (click)="reconnect()">Reconnect</button>
              }
```

5. Add the `reconnect()` handler and make the generic add-toggle clear the preset. Change the add-toggle button binding to call a method, and add both methods:

Change the toggle button:

```html
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-add-toggle"
                  (click)="toggleAdd()" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Add credential' }}
          </button>
```

Add the methods (near `onCreated`):

```ts
  toggleAdd(): void {
    if (this.showAdd()) {
      this.showAdd.set(false);
      return;
    }
    this.reconnectProvider.set(null);
    this.showAdd.set(true);
  }

  reconnect(): void {
    this.reconnectProvider.set('openai_codex');
    this.showAdd.set(true);
  }
```

6. Pass the preset to the form. Find the `<app-credential-form>` usage in the template and add the `initialProvider` input:

```html
        <app-credential-form [businessId]="businessId()" [initialProvider]="reconnectProvider()"
                             (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
```

(If the existing `(cancelled)` / `(saved)` bindings differ, keep them and only add `[initialProvider]`.)

- [ ] **Step 4: Run the spec to verify it passes**

Run: `cd web && npx vitest run src/app/pages/credentials/ai/list.spec.ts`
Expected: PASS (existing tests + the new badge/reconnect test).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/credentials/ai/list.ts web/src/app/pages/credentials/ai/list.spec.ts
git commit -m "feat(codex): credential list — connection-health badge + Reconnect"
```

---

### Task 6: Review-setup + agent-form selectability

**Files:**
- Modify: `web/src/app/pages/code-review/setup.ts`
- Modify: `web/src/app/pages/code-review/setup.spec.ts`
- Modify: `web/src/app/pages/agents/agent-form.ts`

**Interfaces:**
- Consumes: the seeded `openai_codex` catalog (Task 1) surfaced via `/agents/models`.
- Produces: `openai_codex` selectable in the review fallback-chain provider picker and the agent form; both use the existing catalog-`<select>` model path (codex is NOT added to `FREE_TEXT_MODEL_PROVIDERS` or `LIVE_CATALOG_PROVIDERS`).

- [ ] **Step 1: Write the failing setup spec addition**

Add to `web/src/app/pages/code-review/setup.spec.ts` (inside the existing `describe`; if the file has a mount helper, reuse it — otherwise assert on the exported constant behavior via the component):

```ts
  it('offers openai_codex as a review provider', () => {
    // PROVIDERS is rendered into every fallback-chain provider <select>; assert the option exists.
    const f = mountSetup(); // reuse the file's existing mount helper
    const opts = Array.from(f.nativeElement.querySelectorAll('option')).map((o: any) => o.value);
    expect(opts).toContain('openai_codex');
  });
```

If `setup.spec.ts` has no `mountSetup` helper, instead add a minimal assertion against the provider list by importing the component and reading a rendered provider `<select>` after `detectChanges()`; match the existing spec's mounting pattern in that file.

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npx vitest run src/app/pages/code-review/setup.spec.ts`
Expected: FAIL — `openai_codex` not among the options.

- [ ] **Step 3: Add codex to `setup.ts` PROVIDERS**

In `web/src/app/pages/code-review/setup.ts`, add to the `PROVIDERS` array (after the huggingface entry):

```ts
  { value: 'openai_codex', label: 'OpenAI Codex (ChatGPT)' },
```

- [ ] **Step 4: Add codex to `agent-form.ts` provider select**

In `web/src/app/pages/agents/agent-form.ts`, add the option to the provider `<select>` (after the existing `<option value="openrouter">` / huggingface options, around line 41):

```html
          <option value="openai_codex">OpenAI Codex (ChatGPT)</option>
```

(Do NOT add `openai_codex` to `FREE_TEXT_MODEL_PROVIDERS` or `LIVE_CATALOG_PROVIDERS` in either file — codex models come from the static catalog, so the `modelsForProvider` `<select>` path renders them.)

- [ ] **Step 5: Run the setup + agent-form specs to verify they pass**

Run: `cd web && npx vitest run src/app/pages/code-review/setup.spec.ts src/app/pages/agents/agent-form.spec.ts`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/src/app/pages/code-review/setup.ts web/src/app/pages/code-review/setup.spec.ts web/src/app/pages/agents/agent-form.ts
git commit -m "feat(codex): make openai_codex selectable in review setup + agent form"
```

---

### Task 7: End-to-end Playwright coverage + real-browser verification

**Files:**
- Modify: `web/e2e/ai-credentials.spec.ts`

**Interfaces:**
- Consumes: the full stack from Tasks 2–6, exercised through the browser against mocked codex endpoints.

- [ ] **Step 1: Add the connect-flow e2e test**

Append to `web/e2e/ai-credentials.spec.ts` a test that (a) installs an empty `**/api/**` fallback route FIRST (known gotcha: unmocked shell nav-badge calls 401 → token refresh → redirect to `/login` mid-test), then (b) mocks the codex endpoints and drives the panel. Match the file's existing Playwright style (`test`, `page.route`, `expect(page.locator(...))`):

```ts
test('connects an openai_codex credential via device code', async ({ page }) => {
  // Fallback FIRST so shell nav-badge/approvals/connectors calls don't 401 → logout mid-test.
  await page.route('**/api/**', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{"items":[]}' }));

  // Businesses + initial credential list (empty).
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null }) }));
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => {
    if (r.request().method() === 'GET') return r.fulfill({ status: 200, contentType: 'application/json', body: '{"items":[]}' });
    return r.fallback();
  });
  // Codex model catalog for the panel's model <select>.
  await page.route('**/api/v1/businesses/b1/agents/models', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ items: [{ provider: 'openai_codex', model_id: 'gpt-5-codex' }] }) }));
  // Device start → then status approved.
  await page.route('**/api/v1/businesses/b1/ai_credentials/codex/device/start', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ pending_id: 'p1', user_code: 'ABCD-1234', verification_uri: 'https://auth.openai.com/device', verification_uri_complete: 'https://auth.openai.com/device?c=ABCD-1234', interval: 1, expires_in: 900 }) }));
  await page.route('**/api/v1/businesses/b1/ai_credentials/codex/device/p1/status', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'approved', credential_id: 'cx1' }) }));

  await page.goto('/credentials/ai'); // adjust to the file's existing route constant
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-provider').selectOption('openai_codex');
  await page.getByTestId('codex-model').selectOption('gpt-5-codex');
  await page.getByTestId('codex-signin').click();
  await expect(page.getByTestId('codex-user-code')).toContainText('ABCD-1234');
  // Poll (interval 1s) reaches approved and emits connected → the form's saved → list reload.
  await expect(page.getByTestId('codex-authorizing')).toBeHidden({ timeout: 5000 });
});
```

(Adjust the `page.goto` path, business-selection steps, and route-registration idioms to match the existing tests in `ai-credentials.spec.ts`.)

- [ ] **Step 2: Run the e2e spec in a real browser**

Run: `cd web && npx playwright test e2e/ai-credentials.spec.ts`
Expected: PASS. This is the real-browser verification required for the visible UI. If the dev server must be running, follow the pattern the other e2e tests in this file use (`playwright.config.ts` `webServer`).

- [ ] **Step 3: Manual smoke via the browser tooling (belt-and-suspenders)**

Drive the connect panel once through gstack `$B` or the Playwright MCP against the running FE to confirm the three panel states render (configure → authorizing code shown → paste disclosure expands) and the health badge appears. This catches CDK/provider-injection/focus issues that mocked unit tests miss.

- [ ] **Step 4: Commit**

```bash
git add web/e2e/ai-credentials.spec.ts
git commit -m "test(codex): e2e connect-flow coverage for the ChatGPT sign-in panel"
```

---

## Self-Review

**Spec coverage:**
- Spec §1 (provider plumbing) → Task 2 (service enum/DTOs) + Task 4 (form option). ✓
- Spec §2 (connect panel device-code + paste) → Task 3. ✓
- Spec §3 (model catalog + `*-pro` filter, `$0` pricing) → Task 1. ✓
- Spec §4 (health badge + Reconnect; reconnect = replace-in-place resolved via `UpsertCodexCredential` upsert) → Task 5. ✓
- Spec §5 (review-setup selectability) → Task 6. ✓
- Spec testing plan → per-task Vitest specs + Task 7 e2e. ✓

**Placeholder scan:** No `TBD`/`TODO`. The three "adjust to the file's existing…" notes in Tasks 6–7 are bounded to matching a local mount-helper / route-constant idiom, with the exact assertion/route content supplied; they are not deferred logic.

**Type consistency:** `AIProvider` includes `'openai_codex'` (Task 2) and is imported where used (Tasks 4, 5). `CodexConnectBody`/`CodexDeviceStart`/`CodexPKCEStart`/`CodexConnectStatus` are defined in Task 2 and consumed by name in Task 3. `models()` returns `{ items: ModelDescriptor[] }` (used in Task 3 `ngOnInit` and the Task 3 spec's flush shape). `connected` emits `string` (credential_id); the form binds `(connected)="saved.emit()"` and `saved` is `EventEmitter<void>` (Task 4) — consistent with `list.ts#onCreated()` which ignores the payload. `filterCodexPro([]ModelInfo) []ModelInfo` (Task 1) matches `ModelInfo{Provider, ModelID}`.

## Open item carried from the spec — RESOLVED

Reconnect semantics: `db/query/ai.sql UpsertCodexCredential` uses `ON CONFLICT (business_id, provider) DO UPDATE`, so re-running the connect flow **replaces the existing codex credential in place** (same `id`, refreshed tokens). Reconnect therefore just re-opens the connect panel (Task 5) — no delete-then-connect. (`base_url` is not in the conflict `SET`, but codex never sets a user base_url, so this is moot.)

## Pointers

- Spec: `docs/superpowers/specs/2026-07-19-codex-increment3-fe-connect-panel-design.md`
- Issue: `manyforge-6fx.1` (epic `manyforge-6fx`)
- Backend endpoints: `internal/agents/credential_handler.go`, `internal/agents/credential_codex.go`; catalog: `internal/agents/metadata.go`, `migrations/0038_model_pricing.up.sql`
- FE: `web/src/app/core/ai-credentials.service.ts`, `web/src/app/core/agents.service.ts`, `web/src/app/pages/credentials/ai/{credential-form,list,codex-connect}.ts`, `web/src/app/pages/code-review/setup.ts`, `web/src/app/pages/agents/agent-form.ts`, `web/e2e/ai-credentials.spec.ts`
