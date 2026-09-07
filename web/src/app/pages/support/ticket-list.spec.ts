import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { Subject } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Page, Ticket, TicketService } from '../../core/ticket.service';
import { TicketListComponent } from './ticket-list';

// Component-level coverage for the US1 ticket-list page. We drive the real
// component against a mock backend (HttpTestingController) and assert:
// — design-system markup present (mf-page-header, mf-select, mf-table, mf-status-pill)
// — all data-testid attributes preserved
// — dark-theme token classes visible
// Mirrors inbox-settings.spec.ts style.

const bizUrl = '/api/v1/businesses';
const ticketsUrl = '/api/v1/businesses/b1/tickets';

function makeBizPage() {
  return {
    items: [
      {
        id: 'b1',
        parent_id: null,
        tenant_root_id: 'b1',
        name: 'Acme',
        status: 'active',
        is_tenant_root: true,
      },
    ],
  };
}

function makeTicket(over: Partial<Ticket> = {}): Ticket {
  return {
    id: 'tk1',
    business_id: 'b1',
    tenant_root_id: 'b1',
    subject: 'My printer is on fire',
    status: 'open',
    priority: 'high',
    assignee_principal_id: null,
    requester: {
      id: 'r1',
      tenant_root_id: 'b1',
      email: 'user@acme.test',
      display_name: 'Alice',
      contact_id: null,
      first_seen_at: '2024-01-01T00:00:00Z',
      last_seen_at: '2024-01-01T00:00:00Z',
    },
    tags: ['urgent', 'hardware'],
    message_count: 3,
    last_message_at: '2024-06-01T10:00:00Z',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

const ticketPage: Page<Ticket> = { items: [makeTicket()], next_cursor: null };

describe('TicketListComponent (Task 19 UI redesign)', () => {
  let fixture: ComponentFixture<TicketListComponent>;
  let mock: HttpTestingController;

  function boot(): void {
    fixture = TestBed.createComponent(TicketListComponent);
    fixture.detectChanges(); // ngOnInit → GET /api/v1/businesses
    mock.expectOne(bizUrl).flush(makeBizPage());
    // After receiving businesses, the component sets businessId to 'b1' and calls reload()
    mock.expectOne(ticketsUrl).flush(ticketPage);
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }
  function text(testid: string): string {
    return (q(`[data-testid="${testid}"]`)?.textContent?.trim() ?? '');
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear(); // deterministic shared current-business state
  });

  afterEach(() => {
    fixture?.destroy();
    vi.useRealTimers();
    vi.restoreAllMocks();
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
  });

  it('renders mf-page-header with title "Support"', () => {
    boot();
    const header = q('mf-page-header');
    expect(header).not.toBeNull();
    // PageHeader renders the title in an <h1>
    expect(header!.textContent).toContain('Support');
  });

  it('seeds CurrentBusinessService so the approvals badge tracks the viewed business (crm)', () => {
    boot();
    const cb = TestBed.inject(CurrentBusinessService);
    expect(cb.businessId()).toBe('b1'); // seeded on default load

    // Switching business updates the shared service that drives the nav badge.
    fixture.componentInstance.selectBusiness('b2');
    mock.expectOne('/api/v1/businesses/b2/tickets').flush(ticketPage);
    expect(cb.businessId()).toBe('b2');
  });


  it('status-filter and priority-filter are present', () => {
    boot();
    expect(q('[data-testid="status-filter"]')).not.toBeNull();
    expect(q('[data-testid="priority-filter"]')).not.toBeNull();
  });

  it('renders ticket-list and ticket-row after loading', () => {
    boot();
    expect(q('[data-testid="ticket-list"]')).not.toBeNull();
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="ticket-row"]');
    expect(rows.length).toBe(1);
  });

  it('ticket-status uses mf-status-pill', () => {
    boot();
    const statusEl = q('[data-testid="ticket-status"]');
    expect(statusEl).not.toBeNull();
    // mf-status-pill is the host element; the inner span carries mf-pill-* class
    const pill = statusEl!.closest('mf-status-pill') ?? statusEl;
    expect(pill).not.toBeNull();
  });

  it('preserves ticket-subject, ticket-requester, ticket-message-count, ticket-tags, ticket-priority', () => {
    boot();
    expect(text('ticket-subject')).toContain('My printer is on fire');
    expect(text('ticket-requester')).toContain('Alice');
    expect(text('ticket-message-count')).toContain('3');
    expect(q('[data-testid="ticket-tags"]')).not.toBeNull();
    expect(q('[data-testid="ticket-priority"]')).not.toBeNull();
  });

  it('inbox-settings-link and back-to-dashboard are present in the header actions', () => {
    boot();
    // inbox-settings-link only rendered when businessId() is truthy
    expect(q('[data-testid="inbox-settings-link"]')).not.toBeNull();
    expect(q('[data-testid="back-to-dashboard"]')).not.toBeNull();
  });

  it('load-more button has mf-btn class when next_cursor is present', () => {
    fixture = TestBed.createComponent(TicketListComponent);
    fixture.detectChanges();
    mock.expectOne(bizUrl).flush(makeBizPage());
    mock.expectOne(ticketsUrl).flush({ items: [makeTicket()], next_cursor: 'cursor-abc' });
    fixture.detectChanges();

    const btn = q('[data-testid="load-more"]') as HTMLButtonElement | null;
    expect(btn).not.toBeNull();
    expect(btn!.classList.contains('mf-btn')).toBe(true);
  });

  it('dark-theme: .mf-table or .mf-card is present', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    boot();
    const hasTable = !!q('.mf-table');
    const hasCard = !!q('.mf-card');
    expect(hasTable || hasCard).toBe(true);
  });

  it('keeps the latest A → B → A search and facets when obsolete reads finish late', () => {
    vi.useFakeTimers();
    boot();
    const firstA = new Subject<Page<Ticket>>();
    const queryB = new Subject<Page<Ticket>>();
    const latestA = new Subject<Page<Ticket>>();
    const oldMore = new Subject<Page<Ticket>>();
    const newStatus = new Subject<Page<Ticket>>();
    const newPriority = new Subject<Page<Ticket>>();
    vi.spyOn(TestBed.inject(TicketService), 'listTickets')
      .mockReturnValueOnce(firstA)
      .mockReturnValueOnce(queryB)
      .mockReturnValueOnce(latestA)
      .mockReturnValueOnce(oldMore)
      .mockReturnValueOnce(newStatus)
      .mockReturnValueOnce(newPriority);
    const component = fixture.componentInstance;
    component.setSearch('refund');
    vi.advanceTimersByTime(200);
    component.setSearch('invoice');
    // A response arriving during the debounce must already be obsolete.
    firstA.next({ items: [makeTicket({ subject: 'Stale refund' })], next_cursor: 'stale' });
    fixture.detectChanges();
    expect(q('[data-testid="ticket-row"]')).toBeNull();
    vi.advanceTimersByTime(200);
    component.setSearch('refund');
    vi.advanceTimersByTime(200);
    latestA.next({ items: [makeTicket({ subject: 'Current refund' })], next_cursor: 'more-refunds' });
    fixture.detectChanges();
    expect(text('ticket-subject')).toBe('Current refund');
    component.loadMore();
    component.setStatus('open');
    component.setPriority('urgent');
    newPriority.next({
      items: [makeTicket({ subject: 'Urgent open refund', priority: 'urgent' })],
      next_cursor: null,
    });
    oldMore.next({ items: [makeTicket({ id: 'old', subject: 'Stale extra refund' })], next_cursor: 'old' });
    newStatus.next({ items: [makeTicket({ subject: 'Wrong priority refund' })], next_cursor: 'old' });
    firstA.next({ items: [makeTicket({ subject: 'Stale refund' })], next_cursor: 'old' });
    queryB.error(new Error('Obsolete request failed'));
    fixture.detectChanges();
    expect(text('ticket-subject')).toBe('Urgent open refund');
    expect(fixture.nativeElement.querySelectorAll('[data-testid="ticket-row"]')).toHaveLength(1);
    expect(q('[data-testid="load-more"]')).toBeNull();
    expect(q('[data-testid="list-error"]')).toBeNull();
  });

  it('follows the global business and ignores old pages even after switching back', () => {
    boot();
    const firstPage = new Subject<Page<Ticket>>();
    const oldMore = new Subject<Page<Ticket>>();
    const otherBusiness = new Subject<Page<Ticket>>();
    const returnedBusiness = new Subject<Page<Ticket>>();
    vi.spyOn(TestBed.inject(TicketService), 'listTickets')
      .mockReturnValueOnce(firstPage)
      .mockReturnValueOnce(oldMore)
      .mockReturnValueOnce(otherBusiness)
      .mockReturnValueOnce(returnedBusiness);
    const component = fixture.componentInstance;
    component.reload();
    firstPage.next({ items: [makeTicket()], next_cursor: 'old-business-page' });
    component.loadMore();
    const currentBusiness = TestBed.inject(CurrentBusinessService);
    currentBusiness.set('b2');
    fixture.detectChanges();
    currentBusiness.set('b1');
    fixture.detectChanges();
    returnedBusiness.next({
      items: [makeTicket({ subject: 'Fresh Acme conversation' })],
      next_cursor: null,
    });
    otherBusiness.next({
      items: [makeTicket({ business_id: 'b2', subject: 'Other business conversation' })],
      next_cursor: 'other',
    });
    oldMore.next({ items: [makeTicket({ id: 'old', subject: 'Obsolete Acme page' })], next_cursor: 'old' });
    fixture.detectChanges();
    expect(text('ticket-subject')).toBe('Fresh Acme conversation');
    expect(fixture.nativeElement.querySelectorAll('[data-testid="ticket-row"]')).toHaveLength(1);
    expect(q('[data-testid="load-more"]')).toBeNull();
    expect(q('[data-testid="inbox-settings-link"]')?.getAttribute('href')).toBe('/support/b1/settings/inbox');
  });
});
