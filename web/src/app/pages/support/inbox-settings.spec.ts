import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { EmailDomain, InboundAddress, Page } from '../../core/ticket.service';
import { InboxSettingsComponent } from './inbox-settings';

// Component-level coverage for the US4 inbox-settings page. We drive the real
// component against a mock backend (HttpTestingController) so the wiring —
// list GETs → render, add-domain POST → challenge render, verify POST, add-address
// POST, and the no-oracle load-error path — is pinned. The route supplies the
// businessId the component reads in ngOnInit. Mirrors thread-view.spec.ts.
const biz = 'b1';

function mockEmailDomain(over: Partial<EmailDomain> = {}): EmailDomain {
  return {
    id: 'ed1',
    business_id: biz,
    tenant_root_id: 'root',
    domain: 'support.acme.com',
    mode: 'forward_in',
    verification: 'unverified',
    verified_at: null,
    dkim_state: 'pending',
    spf_state: 'unknown',
    dns_challenge: {
      verification_txt: { name: '_manyforge.support.acme.com', value: 'mf-verify=abc123' },
      dkim_record: {
        name: 'mfdeadbeef._domainkey.support.acme.com',
        value: 'v=DKIM1; k=ed25519; p=base64pub',
      },
      spf_hint: 'v=spf1 include:mail.manyforge.example ~all',
      mx_hint: null,
    },
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function mockInboundAddress(over: Partial<InboundAddress> = {}): InboundAddress {
  return {
    id: 'ia1',
    business_id: biz,
    tenant_root_id: 'root',
    address: 'sys@inbox.manyforge.example',
    kind: 'system',
    email_domain_id: null,
    active: true,
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

const domainsUrl = `/api/v1/businesses/${biz}/email-domains`;
const addressesUrl = `/api/v1/businesses/${biz}/inbound-addresses`;

describe('InboxSettingsComponent (US4)', () => {
  let fixture: ComponentFixture<InboxSettingsComponent>;
  let cmp: InboxSettingsComponent;
  let mock: HttpTestingController;

  // Bring the component to a loaded state by flushing the two initial list GETs.
  function loadWith(domains: EmailDomain[], addresses: InboundAddress[]): void {
    fixture = TestBed.createComponent(InboxSettingsComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit fires the two list GETs

    const dPage: Page<EmailDomain> = { items: domains, next_cursor: null };
    const aPage: Page<InboundAddress> = { items: addresses, next_cursor: null };
    mock.expectOne(domainsUrl).flush(dPage);
    mock.expectOne(addressesUrl).flush(aPage);
    fixture.detectChanges();
  }

  function text(testid: string): string {
    const el = fixture.nativeElement.querySelector(
      `[data-testid="${testid}"]`,
    ) as HTMLElement | null;
    return el?.textContent?.trim() ?? '';
  }

  function all(testid: string): HTMLElement[] {
    return Array.from(fixture.nativeElement.querySelectorAll(`[data-testid="${testid}"]`));
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: new Map([['businessId', biz]]) } },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => mock.verify());

  it('renders both lists after the initial GETs', () => {
    loadWith(
      [mockEmailDomain({ domain: 'a.example.com' })],
      [mockInboundAddress({ address: 'sys@x.test' })],
    );

    expect(cmp.domains().length).toBe(1);
    expect(cmp.addresses().length).toBe(1);
    expect(all('domain-row').length).toBe(1);
    expect(all('address-row').length).toBe(1);
    expect(text('domain-name')).toBe('a.example.com');
    expect(text('address-value')).toBe('sys@x.test');
    expect(text('address-kind')).toBe('system');
  });

  it('shows the DNS challenge + a Verify button for an unverified domain', () => {
    loadWith([mockEmailDomain()], []);
    expect(all('dns-challenge').length).toBe(1);
    expect(text('challenge-txt-name')).toBe('_manyforge.support.acme.com');
    expect(text('challenge-txt-value')).toBe('mf-verify=abc123');
    expect(text('challenge-dkim-name')).toBe('mfdeadbeef._domainkey.support.acme.com');
    expect(text('challenge-dkim-value')).toContain('v=DKIM1');
    expect(all('verify-domain').length).toBe(1);
  });

  it('hides the DNS challenge for a verified domain and renders the verified status', () => {
    loadWith(
      [mockEmailDomain({ verification: 'verified', verified_at: '2024-02-02T00:00:00Z' })],
      [],
    );
    expect(all('dns-challenge').length).toBe(0);
    expect(all('verify-domain').length).toBe(0);
    expect(text('domain-status')).toBe('verified');
  });

  it('add-domain POSTs {domain, mode} and renders the returned challenge', () => {
    loadWith([], []);

    cmp.domainDraft = 'support.acme.com';
    cmp.modeDraft = 'subdomain_mx';
    cmp.addDomain();

    const req = mock.expectOne(domainsUrl);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ domain: 'support.acme.com', mode: 'subdomain_mx' });
    req.flush(
      mockEmailDomain({
        mode: 'subdomain_mx',
        dns_challenge: {
          verification_txt: { name: '_manyforge.support.acme.com', value: 'mf-verify=xyz' },
          dkim_record: { name: 'mfsel._domainkey.support.acme.com', value: 'v=DKIM1; p=pub' },
          spf_hint: 'v=spf1 include:mail.manyforge.example ~all',
          mx_hint: 'mail.manyforge.example',
        },
      }),
    );
    fixture.detectChanges();

    expect(cmp.domains().length).toBe(1);
    expect(cmp.domainDraft).toBe(''); // cleared on success
    expect(text('challenge-txt-value')).toBe('mf-verify=xyz');
    // mx_hint present for subdomain_mx → MX record row rendered
    expect(text('challenge-mx-hint')).toBe('mail.manyforge.example');
  });

  it('the Verify button POSTs to the verify endpoint and reflects the returned state', () => {
    loadWith([mockEmailDomain()], []);

    cmp.verify(cmp.domains()[0]);
    const req = mock.expectOne(`${domainsUrl}/ed1/verify`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({});
    req.flush(
      mockEmailDomain({
        verification: 'verified',
        verified_at: '2024-02-02T00:00:00Z',
        dkim_state: 'pass',
      }),
    );
    fixture.detectChanges();

    expect(cmp.domains()[0].verification).toBe('verified');
    expect(all('dns-challenge').length).toBe(0); // challenge gone once verified
  });

  it('a still-unverified verify result surfaces the re-check hint, not an error', () => {
    loadWith([mockEmailDomain()], []);

    cmp.verify(cmp.domains()[0]);
    mock
      .expectOne(`${domainsUrl}/ed1/verify`)
      .flush(mockEmailDomain({ verification: 'unverified' }));
    fixture.detectChanges();

    expect(cmp.verifyHintId()).toBe('ed1');
    expect(cmp.error()).toBe('');
    expect(all('verify-hint').length).toBe(1);
  });

  it('add-address POSTs {address, email_domain_id} for a verified domain', () => {
    loadWith(
      [mockEmailDomain({ verification: 'verified', verified_at: '2024-02-02T00:00:00Z' })],
      [],
    );

    cmp.addressDraft = 'hello@support.acme.com';
    cmp.selectedDomainId = 'ed1';
    cmp.addAddress();

    const req = mock.expectOne(addressesUrl);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ address: 'hello@support.acme.com', email_domain_id: 'ed1' });
    req.flush(mockInboundAddress({ id: 'ia2', address: 'hello@support.acme.com', kind: 'custom' }));
    fixture.detectChanges();

    expect(cmp.addresses().length).toBe(1);
    expect(cmp.addressDraft).toBe(''); // cleared on success
    expect(cmp.selectedDomainId).toBe('');
  });

  it('add-address 409 (unverified/duplicate) surfaces a friendly message', () => {
    loadWith(
      [mockEmailDomain({ verification: 'verified', verified_at: '2024-02-02T00:00:00Z' })],
      [],
    );

    cmp.addressDraft = 'hello@support.acme.com';
    cmp.selectedDomainId = 'ed1';
    cmp.addAddress();
    mock
      .expectOne(addressesUrl)
      .flush(
        { code: 'CONFLICT', message: 'not verified' },
        { status: 409, statusText: 'Conflict' },
      );
    fixture.detectChanges();

    expect(text('settings-error')).toContain('Verify the domain first');
  });

  it('only verified domains are offered in the add-address select', () => {
    loadWith(
      [
        mockEmailDomain({
          id: 'ed-v',
          domain: 'verified.example',
          verification: 'verified',
          verified_at: 'x',
        }),
        mockEmailDomain({ id: 'ed-u', domain: 'unverified.example', verification: 'unverified' }),
      ],
      [],
    );
    expect(cmp.verifiedDomains().map((d) => d.id)).toEqual(['ed-v']);
  });

  // Security: no existence oracle on mutations — a 403 and a 404 from the verify
  // endpoint must both yield the identical generic message so callers cannot
  // distinguish "not yours" from "doesn't exist". Backend detail must not leak.
  it('verify 403 and 404 both show the same generic no-oracle message', () => {
    const genericMsg = "You don't have access to do that.";

    // Case 1: 403 → generic message
    loadWith([mockEmailDomain()], []);
    cmp.verify(cmp.domains()[0]);
    mock
      .expectOne(`${domainsUrl}/ed1/verify`)
      .flush(
        { code: 'FORBIDDEN', message: 'forbidden — do not leak this' },
        { status: 403, statusText: 'Forbidden' },
      );
    fixture.detectChanges();
    expect(cmp.error()).toBe(genericMsg);
    expect(cmp.error()).not.toContain('forbidden — do not leak this');

    // Case 2: 404 → same generic message (no oracle distinguishing existence)
    cmp.error.set('');
    cmp.verify(cmp.domains()[0]);
    mock
      .expectOne(`${domainsUrl}/ed1/verify`)
      .flush(
        { code: 'NOT_FOUND', message: 'not found — do not leak this' },
        { status: 404, statusText: 'Not Found' },
      );
    fixture.detectChanges();
    expect(cmp.error()).toBe(genericMsg);
    expect(cmp.error()).not.toContain('not found — do not leak this');
  });

  it('a 404 on load shows the generic no-oracle error state', () => {
    fixture = TestBed.createComponent(InboxSettingsComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();

    mock
      .expectOne(domainsUrl)
      .flush({ code: 'NOT_FOUND', message: 'not found' }, { status: 404, statusText: 'Not Found' });
    fixture.detectChanges();

    expect(cmp.loadFailed()).toBe(true);
    expect(text('settings-load-error')).toBe("You don't have access to do that.");
  });
});
