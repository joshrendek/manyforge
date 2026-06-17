import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { CompaniesListComponent } from './companies-list';

const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const companies = {
  items: [
    { id: 'co1', tenant_root_id: 'b1', name: 'Acme Inc', domain: 'acme.test', created_at: '', updated_at: '' },
  ],
  next_cursor: null,
};

describe('CompaniesListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  // Basic list mount: businesses, then companies. The new-company form is inline (always present)
  // but fires no GETs of its own.
  function mount(): ComponentFixture<CompaniesListComponent> {
    const f = TestBed.createComponent(CompaniesListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/companies').flush(companies);
    f.detectChanges();
    return f;
  }

  it('loads businesses then lists companies', () => {
    const f = mount();
    expect(f.componentInstance.items().length).toBe(1);
    expect(f.componentInstance.items()[0].name).toBe('Acme Inc');
  });

  it('renders a company row with its name and domain', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="company-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="company-name-cell"]')?.textContent).toContain('Acme Inc');
    expect(el.querySelector('[data-testid="company-domain-cell"]')?.textContent).toContain('acme.test');
  });

  it('renders an em dash when a company has no domain', () => {
    const f = TestBed.createComponent(CompaniesListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/companies').flush({
      items: [{ id: 'co2', tenant_root_id: 'b1', name: 'No Domain Co', domain: null, created_at: '', updated_at: '' }],
      next_cursor: null,
    });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="company-domain-cell"]')?.textContent).toContain('—');
  });

  it('shows the empty state when there are no companies', () => {
    const f = TestBed.createComponent(CompaniesListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/companies').flush({ items: [], next_cursor: null });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="companies-empty"]')).toBeTruthy();
  });

  it('renders the inline new-company form with a name input and create button', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="company-new"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="company-new"] input')).toBeTruthy();
    expect(el.querySelector('[data-testid="company-create"]')).toBeTruthy();
  });

  it('creates a company via the new form (name + domain) then reloads and toasts', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    f.componentInstance.newName = 'New Co';
    f.componentInstance.newDomain = 'new.test';
    f.componentInstance.create();
    const req = mock.expectOne('/api/v1/businesses/b1/companies');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ name: 'New Co', domain: 'new.test' });
    req.flush({ id: 'co3', tenant_root_id: 'b1', name: 'New Co', domain: 'new.test', created_at: '', updated_at: '' });
    f.detectChanges();
    // reload after create
    mock.expectOne('/api/v1/businesses/b1/companies').flush(companies);
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Company created'))).toBe(true);
    expect(f.componentInstance.newName).toBe('');
    expect(f.componentInstance.newDomain).toBe('');
  });

  it('omits domain from the create body when blank', () => {
    const f = mount();
    f.componentInstance.newName = 'No Domain Co';
    f.componentInstance.newDomain = '';
    f.componentInstance.create();
    const req = mock.expectOne('/api/v1/businesses/b1/companies');
    expect(req.request.body).toEqual({ name: 'No Domain Co' });
    expect('domain' in (req.request.body as object)).toBe(false);
    req.flush({ id: 'co4', tenant_root_id: 'b1', name: 'No Domain Co', domain: null, created_at: '', updated_at: '' });
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/companies').flush(companies);
    f.detectChanges();
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
