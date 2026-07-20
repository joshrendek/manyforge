import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../../ui/toast/toast.service';
import { AICredentialsListComponent } from './list';

const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const credentials = {
  items: [
    {
      id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
      allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '', updated_at: '',
    },
  ],
};
const codexCredentials = {
  items: [
    {
      id: 'cx1', business_id: 'b1', provider: 'openai_codex', base_url: '', default_model: 'gpt-5-codex',
      allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '', updated_at: '',
      chatgpt_plan: 'plus', connection_status: 'disconnected', oauth_access_expiry: '2026-01-01T00:00:00Z',
    },
  ],
};

describe('AICredentialsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  function mount(): ComponentFixture<AICredentialsListComponent> {
    const f = TestBed.createComponent(AICredentialsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/ai_credentials').flush(credentials);
    f.detectChanges();
    return f;
  }

  it('loads businesses then lists credentials for the selected business', () => {
    const f = mount();
    expect(f.componentInstance.items().length).toBe(1);
    expect(f.componentInstance.items()[0].provider).toBe('anthropic');
  });

  it('renders a credential row with its provider', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="credential-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="credential-provider"]')?.textContent).toContain('anthropic');
  });

  it('delete asks for confirm, then DELETEs and removes the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="credential-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="credential-delete-confirm"]')).toBeTruthy();
    (f.nativeElement.querySelector('[data-testid="credential-delete-yes"]') as HTMLButtonElement).click();
    mock.expectOne('/api/v1/businesses/b1/ai_credentials/cred1').flush(null);
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="credential-row"]')).toBeNull();
  });

  it('edits default model and lanes inline via PATCH', async () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;
    (el.querySelector('[data-testid="credential-edit"]') as HTMLButtonElement).click();
    f.detectChanges();
    await f.whenStable();
    f.detectChanges();

    const modelInput = el.querySelector('[data-testid="credential-edit-model"]') as HTMLInputElement;
    const lanesInput = el.querySelector('[data-testid="credential-edit-lanes"]') as HTMLInputElement;
    expect(modelInput).toBeTruthy();
    expect(modelInput.value).toBe('claude-opus-4-8');
    expect(lanesInput.value).toBe('4');

    lanesInput.value = '8';
    lanesInput.dispatchEvent(new Event('input'));
    f.detectChanges();

    (el.querySelector('[data-testid="credential-edit-save"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/ai_credentials/cred1');
    expect(req.request.method).toBe('PATCH');
    req.flush({ ...credentials.items[0], default_model: 'claude-opus-4-8', max_concurrent_lanes: 8 });
    f.detectChanges();

    expect(el.querySelector('[data-testid="credential-edit-model"]')).toBeNull();
    expect(el.querySelector('[data-testid="credential-lanes"]')?.textContent?.trim()).toBe('8');
  });

  it('toggles the add form via the add toggle', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="credential-form"]')).toBeNull();
    (el.querySelector('[data-testid="credential-add-toggle"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(el.querySelector('[data-testid="credential-form"]')).toBeTruthy();
  });

  it('shows a success toast after a credential is created', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    f.componentInstance.onCreated();
    mock.expectOne('/api/v1/businesses/b1/ai_credentials').flush(credentials);
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Credential added'))).toBe(true);
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });

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
});
