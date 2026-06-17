import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { ContactsListComponent } from './contacts-list';

const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const contacts = {
  items: [
    {
      id: 'c1', tenant_root_id: 'b1', primary_email: 'jane@acme.test', display_name: 'Jane Doe',
      company_id: null, created_at: '', updated_at: '',
    },
  ],
  next_cursor: null,
};

describe('ContactsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  // Basic list mount: businesses, then contacts. The new-contact form is inline (always present)
  // but fires no GETs of its own.
  function mount(): ComponentFixture<ContactsListComponent> {
    const f = TestBed.createComponent(ContactsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/contacts').flush(contacts);
    f.detectChanges();
    return f;
  }

  it('loads businesses then lists contacts', () => {
    const f = mount();
    expect(f.componentInstance.items().length).toBe(1);
    expect(f.componentInstance.items()[0].primary_email).toBe('jane@acme.test');
  });

  it('renders a contact row with its email and name', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="contact-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="contact-email-cell"]')?.textContent).toContain('jane@acme.test');
    expect(el.querySelector('[data-testid="contact-name-cell"]')?.textContent).toContain('Jane Doe');
  });

  it('renders an em dash when a contact has no display name', () => {
    const f = TestBed.createComponent(ContactsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/contacts').flush({
      items: [{ id: 'c2', tenant_root_id: 'b1', primary_email: 'no-name@acme.test', display_name: null, company_id: null, created_at: '', updated_at: '' }],
      next_cursor: null,
    });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="contact-name-cell"]')?.textContent).toContain('—');
  });

  it('shows the empty state when there are no contacts', () => {
    const f = TestBed.createComponent(ContactsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/contacts').flush({ items: [], next_cursor: null });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="contacts-empty"]')).toBeTruthy();
  });

  it('renders the inline new-contact form with an email input and create button', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="contact-new"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="contact-new"] input')).toBeTruthy();
    expect(el.querySelector('[data-testid="contact-create"]')).toBeTruthy();
  });

  it('creates a contact via the new form then reloads and toasts', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    // The form's (ngSubmit) binds to create(); invoke it as the submit handler would, after
    // populating the email the input is bound to.
    f.componentInstance.newEmail = 'new@acme.test';
    f.componentInstance.create();
    mock.expectOne('/api/v1/businesses/b1/contacts').flush({
      id: 'c3', tenant_root_id: 'b1', primary_email: 'new@acme.test', display_name: null, company_id: null, created_at: '', updated_at: '',
    });
    f.detectChanges();
    // reload after create
    mock.expectOne('/api/v1/businesses/b1/contacts').flush(contacts);
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Contact created'))).toBe(true);
    expect(f.componentInstance.newEmail).toBe('');
  });

  it('links each row to the contact detail route', () => {
    const el: HTMLElement = mount().nativeElement;
    const link = el.querySelector('[data-testid="contact-row"] a') as HTMLAnchorElement;
    expect(link?.getAttribute('href')).toBe('/crm/b1/contacts/c1');
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
