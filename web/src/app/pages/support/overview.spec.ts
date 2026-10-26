import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { Router, provideRouter } from '@angular/router';
import { vi } from 'vitest';
import { CurrentBusinessService } from '../../core/current-business.service';
import { SupportOverview, SupportOverviewCounts } from '../../core/ticket.service';
import { OVERVIEW_REFRESH_MS, SupportOverviewComponent, formatDuration, waitedSeconds } from './overview';

const AS_OF = '2026-10-26T12:00:00Z';
const URL = '/api/v1/support/overview';

function counts(over: Partial<SupportOverviewCounts> = {}): SupportOverviewCounts {
  return {
    open: 0,
    new: 0,
    pending: 0,
    awaiting_reply: 0,
    unassigned: 0,
    urgent: 0,
    high: 0,
    oldest_awaiting_at: null,
    created_7d: 0,
    resolved_7d: 0,
    median_first_response_seconds_7d: null,
    ...over,
  };
}

function overview(): SupportOverview {
  return {
    as_of: AS_OF,
    totals: counts({
      open: 7,
      new: 2,
      pending: 1,
      awaiting_reply: 3,
      unassigned: 2,
      urgent: 1,
      high: 2,
      oldest_awaiting_at: '2026-10-24T08:00:00Z', // 2d 4h before AS_OF
      created_7d: 9,
      resolved_7d: 4,
      median_first_response_seconds_7d: 2700,
    }),
    businesses: [
      {
        business_id: 'b1',
        business_name: 'Bluescripts',
        ...counts({ open: 5, awaiting_reply: 2, urgent: 1, oldest_awaiting_at: '2026-10-26T09:00:00Z', median_first_response_seconds_7d: 1800 }),
      },
      { business_id: 'b2', business_name: 'garden.gg', ...counts({ open: 2, awaiting_reply: 1 }) },
    ],
    attention: [
      {
        business_id: 'b2',
        business_name: 'garden.gg',
        ticket_id: 't9',
        subject: 'Cannot log in',
        status: 'open',
        priority: 'urgent',
        assigned: false,
        waiting_since: '2026-10-26T11:15:00Z',
      },
    ],
  };
}

describe('formatDuration', () => {
  it('renders the two largest units', () => {
    expect(formatDuration(null)).toBe('—');
    expect(formatDuration(20)).toBe('<1m');
    expect(formatDuration(45 * 60)).toBe('45m');
    expect(formatDuration(3 * 3600)).toBe('3h');
    expect(formatDuration(3 * 3600 + 12 * 60 + 5)).toBe('3h 12m');
    expect(formatDuration(2 * 86400 + 4 * 3600 + 59)).toBe('2d 4h');
    expect(formatDuration(-5)).toBe('<1m');
  });

  it('measures waits against the server as_of, not the client clock', () => {
    expect(waitedSeconds('2026-10-26T11:00:00Z', AS_OF)).toBe(3600);
    expect(waitedSeconds(null, AS_OF)).toBeNull();
  });
});

describe('SupportOverviewComponent', () => {
  let fixture: ComponentFixture<SupportOverviewComponent>;
  let http: HttpTestingController;
  const q = (sel: string) => fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  const qa = (sel: string) => Array.from(fixture.nativeElement.querySelectorAll(sel)) as HTMLElement[];

  beforeEach(async () => {
    await TestBed.configureTestingModule({
      imports: [SupportOverviewComponent],
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    }).compileComponents();
    fixture = TestBed.createComponent(SupportOverviewComponent);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    http.verify();
    vi.useRealTimers();
  });

  function load(body: SupportOverview = overview()): void {
    fixture.detectChanges();
    const req = http.expectOne(URL);
    // No business id anywhere: the server decides which businesses qualify.
    expect(req.request.method).toBe('GET');
    req.flush(body);
    fixture.detectChanges();
  }

  it('renders pooled KPI tiles', () => {
    load();
    expect(q('[data-testid="kpi-awaiting"]')!.textContent).toBe('3');
    expect(q('[data-testid="kpi-awaiting-tile"]')!.textContent).toContain('oldest 2d 4h');
    expect(q('[data-testid="kpi-open"]')!.textContent).toBe('7');
    expect(q('[data-testid="kpi-open-tile"]')!.textContent).toContain('2 new · 1 pending');
    expect(q('[data-testid="kpi-priority"]')!.textContent).toBe('1 / 2');
    expect(q('[data-testid="kpi-flow"]')!.textContent).toBe('9 / 4');
    expect(q('[data-testid="kpi-frt"]')!.textContent).toBe('45m');
  });

  it('links each attention ticket to its thread with its wait', () => {
    load();
    const rows = qa('[data-testid="support-overview-attention-row"]');
    expect(rows.length).toBe(1);
    expect(rows[0].getAttribute('href')).toBe('/support/b2/t9');
    expect(rows[0].textContent).toContain('Cannot log in');
    expect(rows[0].textContent).toContain('garden.gg');
    expect(rows[0].textContent).toContain('Unassigned');
    expect(q('[data-testid="support-overview-attention-waiting"]')!.textContent!.trim()).toBe('45m');
    // 3 waiting but only 1 listed → the cap is explained, not silent.
    expect(fixture.nativeElement.textContent).toContain('Showing the 1 most urgent of 3 waiting tickets');
  });

  it('lists every business in server order and opens one by switching business context', () => {
    load();
    const rows = qa('[data-testid="support-overview-business-row"]');
    expect(rows.map((r) => r.getAttribute('data-business-id'))).toEqual(['b1', 'b2']);
    expect(rows[0].querySelector('[data-testid="biz-awaiting"]')!.textContent).toBe('2');
    expect(rows[0].querySelector('[data-testid="biz-oldest"]')!.textContent).toBe('3h');
    expect(rows[0].querySelector('[data-testid="biz-frt"]')!.textContent).toBe('30m');
    expect(rows[1].querySelector('[data-testid="biz-frt"]')!.textContent).toBe('—');

    const nav = vi.spyOn(TestBed.inject(Router), 'navigate').mockResolvedValue(true);
    rows[1].click();
    expect(TestBed.inject(CurrentBusinessService).businessId()).toBe('b2');
    expect(nav).toHaveBeenCalledWith(['/support']);
  });

  it('says so when nobody is waiting', () => {
    const body = overview();
    body.attention = [];
    body.totals.awaiting_reply = 0;
    load(body);
    expect(q('[data-testid="support-overview-caught-up"]')).toBeTruthy();
    expect(q('[data-testid="support-overview-attention"]')).toBeNull();
  });

  it('shows an empty state when the caller has no support access anywhere', () => {
    load({ as_of: AS_OF, totals: counts(), businesses: [], attention: [] });
    expect(q('[data-testid="support-overview-empty"]')).toBeTruthy();
    expect(q('[data-testid="stat-tiles"]')).toBeNull();
  });

  it('shows a retryable error on first-load failure', () => {
    fixture.detectChanges();
    http.expectOne(URL).flush({ code: 'INTERNAL' }, { status: 500, statusText: 'err' });
    fixture.detectChanges();
    expect(q('[data-testid="support-overview-error"]')!.getAttribute('role')).toBe('alert');
    q('[data-testid="support-overview-error"] button')!.click();
    http.expectOne(URL).flush(overview());
    fixture.detectChanges();
    expect(q('[data-testid="support-overview-error"]')).toBeNull();
    expect(q('[data-testid="kpi-awaiting"]')!.textContent).toBe('3');
  });

  it('keeps the last good numbers when a background poll fails', () => {
    vi.useFakeTimers();
    load();
    vi.advanceTimersByTime(OVERVIEW_REFRESH_MS);
    http.expectOne(URL).flush({}, { status: 502, statusText: 'bad gateway' });
    fixture.detectChanges();
    expect(q('[data-testid="support-overview-error"]')).toBeTruthy();
    expect(q('[data-testid="kpi-awaiting"]')!.textContent).toBe('3');
  });

  it('ignores a stale response that lands after a newer refresh', () => {
    fixture.detectChanges();
    const first = http.expectOne(URL);
    q('[data-testid="support-overview-refresh"]')!.click();
    const second = http.match(URL);
    expect(second.length).toBe(1);
    const fresh = overview();
    fresh.totals.awaiting_reply = 11;
    second[0].flush(fresh);
    first.flush(overview()); // older request resolves last
    fixture.detectChanges();
    expect(q('[data-testid="kpi-awaiting"]')!.textContent).toBe('11');
  });
});
