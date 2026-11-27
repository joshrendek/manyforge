import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Page } from '../../core/ticket.service';
import { PortalDomain } from '../../core/portal-domain.service';
import { PortalDomainsComponent } from './portal-domains';

const biz = 'b1';
const base = `/api/v1/businesses/${biz}/portal-domains`;

function domain(over: Partial<PortalDomain> = {}): PortalDomain {
  return {
    id: 'pd1',
    hostname: 'portal.example.test',
    verified: false,
    verified_at: null,
    dns: {
      verification_txt: {
        name: '_manyforge-portal.portal.example.test',
        value: 'mf-portal-verify=abc123',
      },
      target: { type: 'CNAME', name: 'portal.example.test', value: 'app.example.test' },
    },
    created_at: '2026-01-01T00:00:00Z',
    ...over,
  };
}

describe('PortalDomainsComponent', () => {
  let fixture: ComponentFixture<PortalDomainsComponent>;
  let mock: HttpTestingController;

  function load(items: PortalDomain[]): void {
    fixture = TestBed.createComponent(PortalDomainsComponent);
    fixture.componentRef.setInput('businessId', biz);
    fixture.detectChanges();
    const page: Page<PortalDomain> = { items, next_cursor: null };
    const req = mock.expectOne((r) => r.url === base);
    expect(req.request.method).toBe('GET');
    expect(req.request.params.get('limit')).toBe('50');
    req.flush(page);
    fixture.detectChanges();
  }

  const q = (id: string): HTMLElement | null =>
    fixture.nativeElement.querySelector(`[data-testid="${id}"]`);
  const all = (id: string): HTMLElement[] =>
    Array.from(fixture.nativeElement.querySelectorAll(`[data-testid="${id}"]`));
  const text = (id: string): string => q(id)?.textContent?.trim() ?? '';

  function type(value: string): void {
    const input = q('portal-domain-input') as HTMLInputElement;
    input.value = value;
    input.dispatchEvent(new Event('input'));
    fixture.detectChanges();
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    mock.verify();
  });

  it('renders the empty state', () => {
    load([]);
    expect(q('portal-domain-empty')).not.toBeNull();
    expect(all('portal-domain-row').length).toBe(0);
  });

  it('shows a load error with Retry that reloads', () => {
    fixture = TestBed.createComponent(PortalDomainsComponent);
    fixture.componentRef.setInput('businessId', biz);
    fixture.detectChanges();
    mock.expectOne((r) => r.url === base).flush({}, { status: 500, statusText: 'x' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toContain('Something went wrong. Try again.');
    (q('portal-domain-retry') as HTMLButtonElement).click();
    mock.expectOne((r) => r.url === base).flush({ items: [], next_cursor: null });
    fixture.detectChanges();
    expect(q('portal-domain-empty')).not.toBeNull();
  });

  it('disables Add while empty and posts {hostname}, prepending the row', () => {
    load([domain({ id: 'old', hostname: 'old.example.test' })]);
    const add = q('portal-domain-add') as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    type('portal.example.test');
    expect(add.disabled).toBe(false);
    add.click();
    const req = mock.expectOne(base);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ hostname: 'portal.example.test' });
    req.flush(domain({ id: 'new' }), { status: 201, statusText: 'Created' });
    fixture.detectChanges();
    const rows = all('portal-domain-row');
    expect(rows.length).toBe(2);
    expect(rows[0].textContent).toContain('portal.example.test');
    expect((q('portal-domain-input') as HTMLInputElement).value).toBe('');
  });

  it('shows both DNS records for an unverified row and none for a verified row', () => {
    load([domain(), domain({ id: 'pd2', hostname: 'v.example.test', verified: true })]);
    const rows = all('portal-domain-row');
    expect(rows[0].querySelectorAll('.mf-dns-rec').length).toBe(2);
    expect(rows[0].textContent).toContain('CNAME');
    expect(rows[0].textContent).toContain('app.example.test');
    expect(rows[0].textContent).toContain('TXT');
    expect(rows[0].textContent).toContain('_manyforge-portal.portal.example.test');
    expect(rows[0].textContent).toContain('mf-portal-verify=abc123');
    expect(rows[0].querySelector('[data-testid="portal-domain-status"]')?.textContent).toContain(
      'Pending',
    );
    expect(rows[1].querySelector('[data-testid="portal-domain-dns"]')).toBeNull();
    expect(rows[1].querySelector('[data-testid="portal-domain-verify"]')).toBeNull();
    expect(rows[1].querySelector('[data-testid="portal-domain-status"]')?.textContent).toContain(
      'Verified',
    );
  });

  it('omits the target record when its value is empty', () => {
    const d = domain();
    d.dns.target.value = '';
    load([d]);
    expect(all('portal-domain-row')[0].querySelectorAll('.mf-dns-rec').length).toBe(1);
    expect(q('portal-domain-dns')?.textContent).not.toContain('CNAME');
  });

  it('verify success flips the status pill', () => {
    load([domain()]);
    (q('portal-domain-verify') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(text('portal-domain-verify')).toBe('Checking DNS…');
    const req = mock.expectOne(`${base}/pd1/verify`);
    expect(req.request.method).toBe('POST');
    req.flush(domain({ verified: true, verified_at: '2026-01-02T00:00:00Z' }));
    fixture.detectChanges();
    expect(text('portal-domain-status')).toContain('Verified');
    expect(q('portal-domain-verify')).toBeNull();
    expect(q('portal-domain-verify-hint')).toBeNull();
  });

  it('verify still pending shows the hint, not an error', () => {
    load([domain()]);
    (q('portal-domain-verify') as HTMLButtonElement).click();
    mock.expectOne(`${base}/pd1/verify`).flush(domain());
    fixture.detectChanges();
    expect(text('portal-domain-verify-hint')).toBe(
      'Not verified yet. DNS changes can take a few minutes; try again shortly.',
    );
    expect(q('portal-domain-error')).toBeNull();
  });

  it('remove does nothing when confirm is declined', () => {
    const confirmSpy = vi.spyOn(globalThis, 'confirm').mockReturnValue(false);
    load([domain()]);
    (q('portal-domain-remove') as HTMLButtonElement).click();
    expect(confirmSpy).toHaveBeenCalledWith(
      'Remove portal.example.test? Its pages stop being served there.',
    );
    mock.expectNone(`${base}/pd1`);
    expect(all('portal-domain-row').length).toBe(1);
  });

  it('remove sends DELETE after confirm and removes the row', () => {
    vi.spyOn(globalThis, 'confirm').mockReturnValue(true);
    load([domain()]);
    (q('portal-domain-remove') as HTMLButtonElement).click();
    const req = mock.expectOne(`${base}/pd1`);
    expect(req.request.method).toBe('DELETE');
    req.flush(null, { status: 204, statusText: 'No Content' });
    fixture.detectChanges();
    expect(all('portal-domain-row').length).toBe(0);
    expect(q('portal-domain-empty')).not.toBeNull();
  });

  it('add 409 shows the already-verified message', () => {
    load([]);
    type('portal.example.test');
    (q('portal-domain-add') as HTMLButtonElement).click();
    mock.expectOne(base).flush({ code: 'CONFLICT' }, { status: 409, statusText: 'Conflict' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toBe(
      'That hostname is already verified by another business.',
    );
  });

  it('verify 404 shows the access message', () => {
    load([domain()]);
    (q('portal-domain-verify') as HTMLButtonElement).click();
    mock.expectOne(`${base}/pd1/verify`).flush({}, { status: 404, statusText: 'Not Found' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toBe("You don't have access to do that.");
  });

  it('add 400 shows the server message, with a fallback', () => {
    load([]);
    type('bad');
    (q('portal-domain-add') as HTMLButtonElement).click();
    mock
      .expectOne(base)
      .flush({ code: 'VALIDATION', message: 'hostname is not a valid DNS name' }, { status: 400, statusText: 'Bad' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toBe('hostname is not a valid DNS name');

    (q('portal-domain-add') as HTMLButtonElement).click();
    mock.expectOne(base).flush({}, { status: 400, statusText: 'Bad' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toBe("That hostname isn't valid.");
  });

  it('other failures show the generic message', () => {
    load([]);
    type('portal.example.test');
    (q('portal-domain-add') as HTMLButtonElement).click();
    mock.expectOne(base).flush({}, { status: 500, statusText: 'x' });
    fixture.detectChanges();
    expect(text('portal-domain-error')).toBe('Something went wrong. Try again.');
  });
});
