import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
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

  it('sync button POSTs to /sync and shows a success toast', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    (f.nativeElement.querySelector('[data-testid="connector-sync"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/sync');
    expect(req.request.method).toBe('POST');
    req.flush({ status: 'sync_started' });
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Sync started'))).toBe(true);
  });

  it('edit button toggles the edit form, and rotate button closes it', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;

    // Edit form not shown initially
    expect(el.querySelector('[data-testid="connector-form"]')).toBeNull();

    // Click Edit — form appears
    (el.querySelector('[data-testid="connector-edit"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(el.querySelector('[data-testid="connector-form"]')).toBeTruthy();

    // Click Rotate — edit form closes, rotate form appears
    (el.querySelector('[data-testid="connector-rotate"]') as HTMLButtonElement).click();
    f.detectChanges();
    // Only one form shown (rotate), edit closed
    const forms = el.querySelectorAll('[data-testid="connector-form"]');
    expect(forms.length).toBe(1);

    // Click Edit again — rotate form closes, edit form opens
    (el.querySelector('[data-testid="connector-edit"]') as HTMLButtonElement).click();
    f.detectChanges();
    const forms2 = el.querySelectorAll('[data-testid="connector-form"]');
    expect(forms2.length).toBe(1);
  });
});
