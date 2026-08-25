import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AnalyticsDashboardComponent } from './dashboard';

const summary = {
  from: '2026-07-19',
  to: '2026-07-25',
  pageviews: 42,
  visitors: 9,
  average_daily_visitors: 13 / 7,
  direct_pageviews: 30,
  direct_share: (30 / 42) * 100,
  comparison: {
    from: '2026-07-12',
    to: '2026-07-18',
    pageviews: 21,
    average_daily_visitors: 1,
    direct_pageviews: 12,
    direct_share: (12 / 21) * 100,
    pageviews_change_percent: 100,
    average_daily_visitors_change_percent: (6 / 7) * 100,
    direct_share_change_percentage_points: (30 / 42) * 100 - (12 / 21) * 100,
  },
  series: [
    { date: '2026-07-19', pageviews: 0, visitors: 0 },
    { date: '2026-07-20', pageviews: 0, visitors: 0 },
    { date: '2026-07-21', pageviews: 0, visitors: 0 },
    { date: '2026-07-22', pageviews: 0, visitors: 0 },
    { date: '2026-07-23', pageviews: 0, visitors: 0 },
    { date: '2026-07-24', pageviews: 12, visitors: 4 },
    { date: '2026-07-25', pageviews: 30, visitors: 9 },
  ],
  top_pages: [
    { path: '/', pageviews: 25, visitors: 8 },
    { path: '/pricing', pageviews: 17, visitors: 5 },
  ],
  top_referrers: [{ host: 'news.ycombinator.com', pageviews: 12, visitors: 6 }],
  breakdowns: {
    utm_source: [{ value: 'hn', pageviews: 12, visitors: 6 }],
    utm_medium: [],
    utm_campaign: [],
    device: [
      { value: 'desktop', pageviews: 30, visitors: 6 },
      { value: 'mobile', pageviews: 12, visitors: 3 },
    ],
    browser: [{ value: 'Chrome', pageviews: 42, visitors: 9 }],
    country: [],
    event: [{ value: 'grow_start', pageviews: 7, visitors: 5 }],
  },
};

function mount(): {
  fixture: ComponentFixture<AnalyticsDashboardComponent>;
  mock: HttpTestingController;
} {
  TestBed.configureTestingModule({
    providers: [
      provideHttpClient(),
      provideHttpClientTesting(),
      provideRouter([]),
      {
        provide: ActivatedRoute,
        useValue: { snapshot: { paramMap: convertToParamMap({ businessId: 'b1', siteId: 's1' }) } },
      },
    ],
  });
  const mock = TestBed.inject(HttpTestingController);
  const fixture = TestBed.createComponent(AnalyticsDashboardComponent);
  fixture.detectChanges();
  return { fixture, mock };
}

describe('AnalyticsDashboardComponent', () => {
  let mock: HttpTestingController;
  let fixture: ComponentFixture<AnalyticsDashboardComponent>;
  let initialCountryStatus: Element | null;

  beforeEach(() => {
    localStorage.clear();
    ({ fixture, mock } = mount());
    initialCountryStatus = fixture.nativeElement.querySelector('[data-testid="country-status"]');
    mock.expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=30').flush(summary);
    fixture.detectChanges();
  });

  afterEach(() => mock.verify());

  it('mounts the country live region before the async summary arrives', () => {
    expect(initialCountryStatus).toBeTruthy();
    expect(initialCountryStatus?.getAttribute('role')).toBe('status');
  });

  it('renders headline totals', () => {
    const el = fixture.nativeElement;
    expect(el.querySelector('[data-testid="stat-pageviews"]').textContent.trim()).toBe('42');
    expect(el.querySelector('[data-testid="stat-visitors"]').textContent.trim()).toBe('1.9');
    expect(el.querySelector('[data-testid="stat-direct"]').textContent.trim()).toBe('71.4%');
    expect(el.querySelector('[data-testid="stat-pageviews-change"]').textContent).toContain(
      '+100.0%',
    );
    expect(el.querySelector('[data-testid="stat-pageviews-change"]').textContent).toContain(
      '21 prior',
    );
  });

  // The visitor hash rotates daily by design, so a multi-day deduplicated total does not exist.
  // The label must not claim one — this is a correctness property of the privacy model, not
  // wording preference.
  it('uses an average daily visitor headline and preserves peak-day context', () => {
    const stats = fixture.nativeElement.querySelector('[data-testid="analytics-stats"]');
    expect(stats.textContent).toContain('Average daily visitors');
    expect(stats.textContent).toContain('Peak day: 9');
    expect(stats.textContent).not.toContain('Unique visitors');
  });

  it('renders every calendar day and exposes exact dates and accessible daily data', () => {
    const chart = fixture.nativeElement.querySelector('[data-testid="analytics-chart"]');
    expect(chart).toBeTruthy();
    expect(chart.getAttribute('role')).toBe('img');
    expect(chart.getAttribute('aria-label')).toContain('Pageviews per day');
    expect(chart.querySelectorAll('rect').length).toBe(7);
    expect(
      fixture.nativeElement.querySelector('[data-testid="analytics-date-range"]').textContent,
    ).toContain('2026-07-19 – 2026-07-25');
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="daily-data-row"]');
    expect(rows.length).toBe(7);
    expect(rows[1].textContent).toContain('2026-07-20');
    expect(rows[1].textContent).toContain('0');
  });

  it('switches the chart between pageviews and daily visitors', () => {
    const visitors: HTMLButtonElement = fixture.nativeElement.querySelector(
      '[data-testid="chart-visitors"]',
    );
    visitors.click();
    fixture.detectChanges();
    expect(visitors.getAttribute('aria-pressed')).toBe('true');
    expect(
      fixture.nativeElement
        .querySelector('[data-testid="analytics-chart"]')
        .getAttribute('aria-label'),
    ).toContain('Daily visitors per day');
  });

  it('renders top pages and referrers', () => {
    const el = fixture.nativeElement;
    expect(el.querySelectorAll('[data-testid="top-page-row"]').length).toBe(2);
    expect(el.querySelectorAll('[data-testid="top-referrer-row"]').length).toBe(1);
    expect(el.querySelector('[data-testid="top-referrers"]').textContent).toContain(
      'news.ycombinator.com',
    );
  });

  it('refetches when the range changes', () => {
    fixture.componentInstance.setDays(7);
    const req = mock.expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=7');
    expect(req.request.method).toBe('GET');
    req.flush({ ...summary, pageviews: 5, series: [], top_pages: [], top_referrers: [] });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="stat-pageviews"]').textContent.trim(),
    ).toBe('5');
  });

  it('does not invent an infinite percentage when the prior baseline is zero', () => {
    fixture.componentInstance.setDays(7);
    mock.expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=7').flush({
      ...summary,
      comparison: {
        ...summary.comparison,
        pageviews: 0,
        pageviews_change_percent: null,
      },
    });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="stat-pageviews-change"]').textContent,
    ).toContain('No prior baseline');
  });

  it('guides the user when there is no traffic yet', () => {
    fixture.componentInstance.setDays(90);
    mock.expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=90').flush({
      ...summary,
      pageviews: 0,
      visitors: 0,
      direct_pageviews: 0,
      series: [],
      top_pages: [],
      top_referrers: [],
    });
    fixture.detectChanges();
    const empty = fixture.nativeElement.querySelector('[data-testid="analytics-empty"]');
    expect(empty).toBeTruthy();
    expect(empty.textContent).toContain('embed tag');
  });

  // Empty dimensions come back from the API but must not become empty tables on screen.
  it('renders a panel only for dimensions that have data', () => {
    const el = fixture.nativeElement;
    expect(el.querySelector('[data-testid="breakdown-device"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="breakdown-browser"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="breakdown-utm_source"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="breakdown-utm_medium"]')).toBeNull();
    expect(el.querySelector('[data-testid="breakdown-country"]')).toBeNull();
    expect(el.querySelector('[data-testid="breakdown-device"]').textContent).toContain('desktop');
  });

  // Traffic but no countries means no trusted edge signal — say so, rather than letting the user
  // conclude their audience is from nowhere.
  it('explains a missing country breakdown when there is traffic', () => {
    const status = fixture.nativeElement.querySelector('[data-testid="country-status"]');
    const hint = fixture.nativeElement.querySelector('[data-testid="country-unavailable"]');
    expect(status).toBeTruthy();
    expect(status.getAttribute('role')).toBe('status');
    expect(hint).toBeTruthy();
    expect(hint.textContent).toContain('trusted edge supplies a supported country');
  });

  it('renders country data without the unavailable hint', () => {
    fixture.componentInstance.setDays(7);
    mock.expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=7').flush({
      ...summary,
      breakdowns: {
        ...summary.breakdowns,
        country: [{ value: 'US', pageviews: 15, visitors: 6 }],
      },
    });
    fixture.detectChanges();

    const el = fixture.nativeElement;
    const status = el.querySelector('[data-testid="country-status"]');
    expect(status).toBeTruthy();
    expect(status.getAttribute('role')).toBe('status');
    expect(el.querySelector('[data-testid="country-unavailable"]')).toBeNull();
    expect(el.querySelector('[data-testid="breakdown-country"]').textContent).toContain('US');
  });

  // The API reuses the `pageviews` field for every dimension, but for 'event' that number is a
  // count of custom events. Labelling it "Pageviews" would report a figure that silently
  // disagrees with the pageview total shown above it.
  it('labels the event breakdown as Events, not Pageviews', () => {
    const el = fixture.nativeElement;
    const events = el.querySelector('[data-testid="breakdown-event"]');
    expect(events).toBeTruthy();
    expect(events.querySelector('.mf-th').textContent).toContain('Events');
    expect(events.querySelector('.mf-th').textContent).not.toContain('Pageviews');
    // Other dimensions keep the pageview label.
    const device = el.querySelector('[data-testid="breakdown-device"]');
    expect(device.querySelector('.mf-th').textContent).toContain('Pageviews');
  });

  it('surfaces an error without crashing', () => {
    fixture.componentInstance.setDays(7);
    mock
      .expectOne('/api/v1/businesses/b1/analytics/summary?client_id=s1&days=7')
      .flush({}, { status: 404, statusText: 'Not Found' });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="analytics-error"]')).toBeTruthy();
  });
});
