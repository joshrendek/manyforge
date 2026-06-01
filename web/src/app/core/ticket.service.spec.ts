import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Page, Requester, Ticket, TicketMessage, TicketService } from './ticket.service';

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
});
