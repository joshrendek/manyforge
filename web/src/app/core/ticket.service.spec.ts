import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  EmailDomain,
  InboundAddress,
  Page,
  PatchTicket,
  Requester,
  Ticket,
  TicketMessage,
  TicketService,
} from './ticket.service';

// Realistic US4 mock payloads (full shapes, matching identity.go json tags).
function mockEmailDomain(over: Partial<EmailDomain> = {}): EmailDomain {
  return {
    id: 'ed1',
    business_id: 'b1',
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
    business_id: 'b1',
    tenant_root_id: 'root',
    address: 'hello@support.acme.com',
    kind: 'custom',
    email_domain_id: 'ed1',
    active: true,
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

// Exercised against a mock backend so we pin the actual URLs and keyset/filter
// query-param construction rather than mocking the service itself.
describe('TicketService', () => {
  let svc: TicketService;
  let mock: HttpTestingController;
  const biz = 'b1';

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    svc = TestBed.inject(TicketService);
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('lists tickets with no filters at the plain business path', () => {
    let out: Page<Ticket> | undefined;
    svc.listTickets(biz).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets`);
    expect(req.request.method).toBe('GET');
    expect(req.request.params.keys()).toEqual([]);
    const page: Page<Ticket> = { items: [], next_cursor: null };
    req.flush(page);
    expect(out).toEqual(page);
  });

  it('encodes status/priority/assignee/tag/cursor/limit as query params', () => {
    svc
      .listTickets(biz, {
        status: 'open',
        priority: 'high',
        assignee: 'unassigned',
        tag: 'billing',
        cursor: 'c1',
        limit: 25,
      })
      .subscribe();
    const req = mock.expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets`);
    expect(req.request.params.get('status')).toBe('open');
    expect(req.request.params.get('priority')).toBe('high');
    expect(req.request.params.get('assignee')).toBe('unassigned');
    expect(req.request.params.get('tag')).toBe('billing');
    expect(req.request.params.get('cursor')).toBe('c1');
    expect(req.request.params.get('limit')).toBe('25');
    req.flush({ items: [], next_cursor: null });
  });

  it('gets a single ticket', () => {
    svc.getTicket(biz, 't1').subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.method).toBe('GET');
    req.flush({} as Ticket);
  });

  it('lists messages and threads the keyset cursor', () => {
    let out: Page<TicketMessage> | undefined;
    svc.listMessages(biz, 't1', 'cur-2', 50).subscribe((r) => (out = r));
    const req = mock.expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets/t1/messages`);
    expect(req.request.params.get('cursor')).toBe('cur-2');
    expect(req.request.params.get('limit')).toBe('50');
    const page: Page<TicketMessage> = { items: [], next_cursor: 'cur-3' };
    req.flush(page);
    expect(out!.next_cursor).toBe('cur-3');
  });

  it('lists requesters with an optional email filter', () => {
    svc.listRequesters(biz, 'a@b.test').subscribe();
    const req = mock.expectOne((r) => r.url === `/api/v1/businesses/${biz}/requesters`);
    expect(req.request.params.get('email')).toBe('a@b.test');
    req.flush({ items: [], next_cursor: null });
  });

  it('gets a single requester', () => {
    svc.getRequester(biz, 'r1').subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/requesters/r1`);
    expect(req.request.method).toBe('GET');
    req.flush({} as Requester);
  });

  it('reply() POSTs to .../reply with body_text and returns the TicketMessage', () => {
    let out: TicketMessage | undefined;
    svc.reply(biz, 't1', { body_text: 'Hello' }).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1/reply`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ body_text: 'Hello' });
    const msg: TicketMessage = {
      id: 'm1',
      ticket_id: 't1',
      direction: 'outbound',
      message_id: null,
      in_reply_to: null,
      references: [],
      author_principal_id: 'p1',
      body_text: 'Hello',
      body_html: null,
      attachments: [],
      spf_result: 'unknown',
      dkim_result: 'unknown',
      dmarc_result: 'unknown',
      delivery_state: 'pending',
      created_at: '2024-01-01T00:00:00Z',
    };
    req.flush(msg);
    expect(out).toEqual(msg);
  });

  it('reply() includes optional body_html when provided', () => {
    svc.reply(biz, 't1', { body_text: 'Hi', body_html: '<p>Hi</p>' }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1/reply`);
    expect(req.request.body).toEqual({ body_text: 'Hi', body_html: '<p>Hi</p>' });
    req.flush({} as TicketMessage);
  });

  it('addNote() POSTs to .../note with body_text and returns the TicketMessage', () => {
    let out: TicketMessage | undefined;
    svc.addNote(biz, 't1', { body_text: 'Internal note content' }).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1/note`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ body_text: 'Internal note content' });
    const msg: TicketMessage = {
      id: 'm2',
      ticket_id: 't1',
      direction: 'note',
      message_id: null,
      in_reply_to: null,
      references: [],
      author_principal_id: 'p1',
      body_text: 'Internal note content',
      body_html: null,
      attachments: [],
      spf_result: 'unknown',
      dkim_result: 'unknown',
      dmarc_result: 'unknown',
      delivery_state: null,
      created_at: '2024-01-01T00:00:00Z',
    };
    req.flush(msg);
    expect(out).toEqual(msg);
  });

  // ── patchTicket (US3 triage) ──────────────────────────────────────────────
  it('patchTicket() PATCHes to .../tickets/{tid} and returns the updated Ticket', () => {
    let out: Ticket | undefined;
    const updated = { id: 't1', status: 'open' } as Ticket;
    svc.patchTicket(biz, 't1', { status: 'open' }).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.method).toBe('PATCH');
    req.flush(updated);
    expect(out).toEqual(updated);
  });

  it('patchTicket() sends ONLY the status field when changing status', () => {
    svc.patchTicket(biz, 't1', { status: 'solved' }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.body).toEqual({ status: 'solved' });
    expect('assignee_principal_id' in (req.request.body as object)).toBe(false);
    req.flush({} as Ticket);
  });

  it('patchTicket() sends ONLY the priority field when changing priority', () => {
    svc.patchTicket(biz, 't1', { priority: 'urgent' }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.body).toEqual({ priority: 'urgent' });
    req.flush({} as Ticket);
  });

  it('patchTicket() sends the FULL tag set (replacement) and can clear with []', () => {
    svc.patchTicket(biz, 't1', { tags: ['billing', 'vip'] }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.body).toEqual({ tags: ['billing', 'vip'] });
    req.flush({} as Ticket);

    svc.patchTicket(biz, 't1', { tags: [] }).subscribe();
    const req2 = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req2.request.body).toEqual({ tags: [] });
    req2.flush({} as Ticket);
  });

  it('patchTicket() OMITS assignee_principal_id entirely when it is not in the patch', () => {
    // Tri-state: a status-only change must not touch the assignee on the wire.
    svc.patchTicket(biz, 't1', { status: 'pending' }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect('assignee_principal_id' in (req.request.body as object)).toBe(false);
    req.flush({} as Ticket);
  });

  it('patchTicket() sends literal null to unassign', () => {
    const patch: PatchTicket = { assignee_principal_id: null };
    svc.patchTicket(biz, 't1', patch).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.body).toEqual({ assignee_principal_id: null });
    // Distinguish null (unassign) from absent (no change): the key is present.
    expect('assignee_principal_id' in (req.request.body as object)).toBe(true);
    expect((req.request.body as PatchTicket).assignee_principal_id).toBeNull();
    req.flush({} as Ticket);
  });

  it('patchTicket() sends the principal uuid to assign', () => {
    svc.patchTicket(biz, 't1', { assignee_principal_id: 'p-self' }).subscribe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/t1`);
    expect(req.request.body).toEqual({ assignee_principal_id: 'p-self' });
    req.flush({} as Ticket);
  });

  // ── US4 inbox management ──────────────────────────────────────────────────
  it('listEmailDomains() GETs the email-domains path and returns the page', () => {
    let out: Page<EmailDomain> | undefined;
    svc.listEmailDomains(biz).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/email-domains`);
    expect(req.request.method).toBe('GET');
    expect(req.request.params.keys()).toEqual([]);
    const page: Page<EmailDomain> = { items: [mockEmailDomain()], next_cursor: null };
    req.flush(page);
    expect(out).toEqual(page);
  });

  it('listEmailDomains() forwards the keyset cursor', () => {
    svc.listEmailDomains(biz, 'cur-2').subscribe();
    const req = mock.expectOne((r) => r.url === `/api/v1/businesses/${biz}/email-domains`);
    expect(req.request.params.get('cursor')).toBe('cur-2');
    req.flush({ items: [], next_cursor: null });
  });

  it('createEmailDomain() POSTs {domain, mode} and returns the created EmailDomain', () => {
    let out: EmailDomain | undefined;
    svc
      .createEmailDomain(biz, { domain: 'support.acme.com', mode: 'subdomain_mx' })
      .subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/email-domains`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ domain: 'support.acme.com', mode: 'subdomain_mx' });
    const created = mockEmailDomain({ mode: 'subdomain_mx' });
    req.flush(created);
    expect(out).toEqual(created);
  });

  it('verifyEmailDomain() POSTs an empty body to .../verify and returns the EmailDomain', () => {
    let out: EmailDomain | undefined;
    svc.verifyEmailDomain(biz, 'ed1').subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/email-domains/ed1/verify`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({});
    const verified = mockEmailDomain({
      verification: 'verified',
      verified_at: '2024-02-02T00:00:00Z',
      dkim_state: 'pass',
    });
    req.flush(verified);
    expect(out!.verification).toBe('verified');
  });

  it('listInboundAddresses() GETs the inbound-addresses path and returns the page', () => {
    let out: Page<InboundAddress> | undefined;
    svc.listInboundAddresses(biz).subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/inbound-addresses`);
    expect(req.request.method).toBe('GET');
    const page: Page<InboundAddress> = {
      items: [mockInboundAddress({ kind: 'system', email_domain_id: null })],
      next_cursor: null,
    };
    req.flush(page);
    expect(out).toEqual(page);
  });

  it('createInboundAddress() POSTs {address, email_domain_id} and returns the InboundAddress', () => {
    let out: InboundAddress | undefined;
    svc
      .createInboundAddress(biz, { address: 'hello@support.acme.com', email_domain_id: 'ed1' })
      .subscribe((r) => (out = r));
    const req = mock.expectOne(`/api/v1/businesses/${biz}/inbound-addresses`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({
      address: 'hello@support.acme.com',
      email_domain_id: 'ed1',
    });
    const created = mockInboundAddress();
    req.flush(created);
    expect(out).toEqual(created);
  });
});
