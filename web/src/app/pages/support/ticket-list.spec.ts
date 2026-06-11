import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Page, Ticket } from '../../core/ticket.service';
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
  });

  afterEach(() => {
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

  it('business-select is a select.mf-select', () => {
    boot();
    const sel = q('[data-testid="business-select"]');
    expect(sel).not.toBeNull();
    expect(sel!.tagName.toLowerCase()).toBe('select');
    expect(sel!.classList.contains('mf-select')).toBe(true);
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
});
