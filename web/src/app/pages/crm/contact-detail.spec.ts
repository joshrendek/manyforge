import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, Router, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Company, Contact } from '../../core/crm.service';
import { ToastService } from '../../ui/toast/toast.service';
import { ContactDetailComponent } from './contact-detail';

// Component-level coverage for the contact-detail page. We drive the real component
// against a mock backend (HttpTestingController) so the wiring — getContact →
// header, listCompanies → picker, listContacts → merge select, updateContact /
// mergeContact / deleteContact mutations — is pinned. The route supplies the
// businessId + id the component reads in ngOnInit (mirrors thread-view.spec).
const biz = 'b1';
const cid = 'c1';

function makeContact(over: Partial<Contact> = {}): Contact {
  return {
    id: cid,
    tenant_root_id: 'root',
    primary_email: 'jane@acme.test',
    display_name: 'Jane Doe',
    company_id: null,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function makeCompany(over: Partial<Company> = {}): Company {
  return {
    id: 'co1',
    tenant_root_id: 'root',
    name: 'Acme Inc',
    domain: 'acme.test',
    created_at: '',
    updated_at: '',
    ...over,
  };
}

describe('ContactDetailComponent', () => {
  let fixture: ComponentFixture<ContactDetailComponent>;
  let cmp: ContactDetailComponent;
  let mock: HttpTestingController;

  // Bring the component to a loaded state: flush getContact, listCompanies, and
  // listContacts (the merge picker), leaving the edit + merge controls live.
  function loadWith(
    contact: Contact = makeContact(),
    companies: Company[] = [],
    others: Contact[] = [],
  ): void {
    fixture = TestBed.createComponent(ContactDetailComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit → getContact + listCompanies + listContacts

    mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`).flush(contact);
    mock
      .expectOne(`/api/v1/businesses/${biz}/companies`)
      .flush({ items: companies, next_cursor: null });
    // The merge picker loads all contacts then excludes this one client-side.
    mock
      .expectOne(`/api/v1/businesses/${biz}/contacts`)
      .flush({ items: [contact, ...others], next_cursor: null });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([
                ['businessId', biz],
                ['id', cid],
              ]),
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });

  afterEach(() => {
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
  });

  it('loads the contact and renders the email + name header', () => {
    loadWith(makeContact({ primary_email: 'jane@acme.test', display_name: 'Jane Doe' }));
    expect(cmp.contact()!.primary_email).toBe('jane@acme.test');
    expect(q('[data-testid="contact-detail"]')).toBeTruthy();
    expect(q('[data-testid="contact-detail-email"]')?.textContent).toContain('jane@acme.test');
    expect(q('[data-testid="contact-detail-name"]')?.textContent).toContain('Jane Doe');
  });

  it('renders the edit form with a name input and a company select (with a blank "none" option)', () => {
    loadWith(makeContact(), [makeCompany({ id: 'co1', name: 'Acme Inc' })]);
    expect(q('[data-testid="contact-edit"]')).toBeTruthy();
    const sel = q('[data-testid="contact-company-select"]') as HTMLSelectElement;
    expect(sel).toBeTruthy();
    // One "— none —" option plus one per company.
    expect(sel.querySelectorAll('option').length).toBe(2);
    expect(cmp.companies().length).toBe(1);
  });

  it('renders the merge block with a select of OTHER contacts (excluding this one)', () => {
    loadWith(
      makeContact({ id: cid }),
      [],
      [
        makeContact({ id: 'c2', primary_email: 'bob@acme.test' }),
        makeContact({ id: 'c3', primary_email: 'sue@acme.test' }),
      ],
    );
    expect(q('[data-testid="contact-merge"]')).toBeTruthy();
    const sel = q('[data-testid="contact-merge-select"]') as HTMLSelectElement;
    expect(sel).toBeTruthy();
    // This contact (c1) is excluded; only c2 + c3 remain as merge candidates.
    expect(cmp.otherContacts().length).toBe(2);
    expect(cmp.otherContacts().some((c) => c.id === cid)).toBe(false);
  });

  it('save PATCHes display_name + company_id and reloads + toasts', () => {
    loadWith(makeContact({ display_name: 'Jane Doe' }), [makeCompany({ id: 'co1' })]);
    const toast = TestBed.inject(ToastService);
    const success = vi.spyOn(toast, 'success');

    cmp.name = 'Jane Q. Doe';
    cmp.companyId = 'co1';
    cmp.save();

    const req = mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`);
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({ display_name: 'Jane Q. Doe', company_id: 'co1' });
    req.flush(makeContact({ display_name: 'Jane Q. Doe', company_id: 'co1' }));

    // Reload after save.
    mock
      .expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`)
      .flush(makeContact({ display_name: 'Jane Q. Doe', company_id: 'co1' }));
    fixture.detectChanges();

    expect(success).toHaveBeenCalledWith(expect.stringContaining('saved'));
    expect(cmp.contact()!.display_name).toBe('Jane Q. Doe');
  });

  it('save omits company_id from the PATCH body when the "none" option is selected (blank)', () => {
    loadWith(makeContact({ company_id: 'co1' }), [makeCompany({ id: 'co1' })]);

    cmp.name = 'Jane Doe';
    cmp.companyId = ''; // "— none —"
    cmp.save();

    const req = mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`);
    expect(req.request.body).toEqual({ display_name: 'Jane Doe' });
    expect('company_id' in (req.request.body as object)).toBe(false);
    req.flush(makeContact());
    mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`).flush(makeContact());
    fixture.detectChanges();
  });

  it('merge POSTs {loser_id} to this (winner) contact, toasts success, and navigates back to the list', () => {
    loadWith(makeContact({ id: cid }), [], [makeContact({ id: 'c2', primary_email: 'bob@acme.test' })]);
    const toast = TestBed.inject(ToastService);
    const success = vi.spyOn(toast, 'success');
    const router = TestBed.inject(Router);
    const navigate = vi.spyOn(router, 'navigate').mockResolvedValue(true);

    cmp.selectedLoserId = 'c2';
    cmp.merge();

    const req = mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}/merge`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ loser_id: 'c2' });
    req.flush({ status: 'ok' });

    expect(success).toHaveBeenCalledWith(expect.stringContaining('Merged'));
    expect(navigate).toHaveBeenCalledWith(['/crm/contacts']);
  });

  it('merge is a no-op when no loser is selected', () => {
    loadWith(makeContact({ id: cid }), [], [makeContact({ id: 'c2' })]);
    cmp.selectedLoserId = '';
    cmp.merge();
    mock.expectNone(`/api/v1/businesses/${biz}/contacts/${cid}/merge`);
  });

  it('delete DELETEs the contact, toasts, and navigates back to the list', () => {
    loadWith(makeContact({ id: cid }));
    const toast = TestBed.inject(ToastService);
    const success = vi.spyOn(toast, 'success');
    const router = TestBed.inject(Router);
    const navigate = vi.spyOn(router, 'navigate').mockResolvedValue(true);

    cmp.remove();

    const req = mock.expectOne(`/api/v1/businesses/${biz}/contacts/${cid}`);
    expect(req.request.method).toBe('DELETE');
    req.flush(null);

    expect(success).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith(['/crm/contacts']);
  });

  it('renders a back link to the contacts list', () => {
    loadWith();
    // The back link points at /crm/contacts.
    const back = fixture.nativeElement.querySelector('a[href="/crm/contacts"]') as HTMLAnchorElement;
    expect(back).toBeTruthy();
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    loadWith();
    expect(q('.mf-card')).toBeTruthy();
  });
});
