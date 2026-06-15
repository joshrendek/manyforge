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
      allow_private_base_url: false, created_at: '', updated_at: '',
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
});
