import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { OverviewSite } from '../../core/analytics.service';
import { AnalyticsOverviewComponent } from './overview';

function site(over: Partial<OverviewSite> = {}): OverviewSite {
  return {
    client_id: 'c1',
    name: 'garden.gg',
    business_id: 'b1',
    business_name: 'Bluescripts',
    pageviews: 100,
    visitors: 40,
    average_daily_visitors: 12.5,
    series: [
      { date: '2026-07-01', pageviews: 10, visitors: 5 },
      { date: '2026-07-02', pageviews: 0, visitors: 0 },
      { date: '2026-07-03', pageviews: 20, visitors: 9 },
    ],
    ...over,
  };
}

describe('AnalyticsOverviewComponent', () => {
  let fixture: ComponentFixture<AnalyticsOverviewComponent>;
  let http: HttpTestingController;

  beforeEach(async () => {
    await TestBed.configureTestingModule({
      imports: [AnalyticsOverviewComponent],
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    }).compileComponents();
    fixture = TestBed.createComponent(AnalyticsOverviewComponent);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function flush(
    sites: OverviewSite[],
    days = 30,
    dataAsOf: string | null = '2026-07-30T23:59:30Z',
  ): void {
    fixture.detectChanges();
    http.expectOne(`/api/v1/analytics/overview?days=${days}`).flush({
      sites,
      data_as_of: dataAsOf,
    });
    fixture.detectChanges();
  }

  it('requests the overview without a business id', () => {
    fixture.detectChanges();
    // The absence of a business id is the feature: the server decides which businesses qualify.
    const req = http.expectOne('/api/v1/analytics/overview?days=30');
    expect(req.request.method).toBe('GET');
    req.flush({ sites: [], data_as_of: null });
  });

  it('shows the common dashboard freshness watermark', () => {
    flush([site()]);
    const freshness = fixture.nativeElement.querySelector('[data-testid="overview-freshness"]');
    expect(freshness.textContent).toContain('2026-07-30T23:59:30Z');
    expect(freshness.querySelector('time').getAttribute('datetime')).toBe('2026-07-30T23:59:30Z');
  });

  it('shows unavailable freshness when one rollup has never completed', () => {
    flush([site()], 30, null);
    expect(
      fixture.nativeElement.querySelector('[data-testid="overview-freshness"]').textContent,
    ).toContain('Data freshness is not available yet');
  });

  it('groups sites by business, preserving traffic order', () => {
    flush([
      site({ client_id: 'c1', business_id: 'b1', business_name: 'Bluescripts', visitors: 90 }),
      site({ client_id: 'c2', business_id: 'b2', business_name: 'Garden.GG', visitors: 50 }),
      site({ client_id: 'c3', business_id: 'b1', business_name: 'Bluescripts', visitors: 10 }),
    ]);

    const groups = fixture.componentInstance.groups();
    expect(groups.length).toBe(2);
    // First appearance drives group order, and sites arrive busiest-first, so the busiest
    // business leads rather than whichever name sorts first.
    expect(groups[0].name).toBe('Bluescripts');
    expect(groups[0].sites.map((s) => s.client_id)).toEqual(['c1', 'c3']);
    expect(groups[1].sites.map((s) => s.client_id)).toEqual(['c2']);
  });

  it('renders a card per site, grouped in the DOM', () => {
    flush([
      site({ client_id: 'c1', business_id: 'b1', business_name: 'Bluescripts' }),
      site({ client_id: 'c2', business_id: 'b2', business_name: 'Garden.GG' }),
    ]);
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelectorAll('[data-testid="overview-group"]').length).toBe(2);
    expect(el.querySelectorAll('[data-testid="overview-card"]').length).toBe(2);
    expect(el.querySelector('[data-testid="overview-card-visitors"]')?.textContent.trim()).toBe(
      '12.5',
    );
    expect(el.querySelector('[data-testid="overview-card"]')?.textContent).toContain(
      'average daily visitors',
    );
    expect(el.querySelector('[data-testid="overview-card"]')?.textContent).toContain(
      'peak 40 visitors',
    );
  });

  it('links a card to that site’s dashboard under its OWN business', () => {
    flush([site({ client_id: 'c9', business_id: 'bZ' })]);
    const card = fixture.nativeElement.querySelector(
      '[data-testid="overview-card"]',
    ) as HTMLAnchorElement;
    // Using the card's own business_id matters: a sub-business site linked under the parent id
    // would 404 on the dashboard, since summary asserts the client belongs to the URL business.
    expect(card.getAttribute('href')).toBe('/analytics/bZ/c9');
  });

  it('shows an explicit no-data message instead of an empty sparkline', () => {
    flush([site({ series: [], pageviews: 0, visitors: 0 })]);
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelector('[data-testid="overview-card-nodata"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="overview-card-spark"]')).toBeNull();
  });

  it('treats an all-zero series as no data', () => {
    // A site whose rollups exist but are all zero is indistinguishable to a reader from one with
    // no rollups at all; drawing a flat line at the baseline would imply real, flat traffic.
    flush([
      site({
        series: [
          { date: '2026-07-01', pageviews: 0, visitors: 0 },
          { date: '2026-07-02', pageviews: 0, visitors: 0 },
        ],
      }),
    ]);
    expect(
      fixture.nativeElement.querySelector('[data-testid="overview-card-nodata"]'),
    ).toBeTruthy();
  });

  it('reloads with the chosen range', () => {
    flush([site()]);
    const sel: HTMLSelectElement = fixture.nativeElement.querySelector(
      '[data-testid="overview-range"]',
    );
    sel.value = '7';
    sel.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    http.expectOne('/api/v1/analytics/overview?days=7').flush({ sites: [] });
    fixture.detectChanges();
    expect(fixture.componentInstance.days()).toBe(7);
  });

  it('a slow earlier response cannot overwrite a newer one with the SAME range', () => {
    // 30 -> 7 -> 30. Guarding on the requested range alone looks equivalent to a request token but
    // is not: when the first 30-day response finally lands, the current range is 30 again, so it
    // passes that check and clobbers the newer result. This is the case the reviewer identified.
    fixture.detectChanges();
    const first30 = http.expectOne('/api/v1/analytics/overview?days=30');

    const sel: HTMLSelectElement = fixture.nativeElement.querySelector(
      '[data-testid="overview-range"]',
    );
    sel.value = '7';
    sel.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    const req7 = http.expectOne('/api/v1/analytics/overview?days=7');

    sel.value = '30';
    sel.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    const second30 = http.expectOne('/api/v1/analytics/overview?days=30');

    // Newest completes first with the real data...
    second30.flush({ sites: [site({ client_id: 'NEW' })] });
    // ...then the two stale ones straggle in.
    req7.flush({ sites: [site({ client_id: 'STALE7' })] });
    first30.flush({ sites: [site({ client_id: 'STALE30' })] });
    fixture.detectChanges();

    expect(fixture.componentInstance.sites().map((s) => s.client_id)).toEqual(['NEW']);
  });

  it('a stale error cannot blank out a newer successful load', () => {
    fixture.detectChanges();
    const first = http.expectOne('/api/v1/analytics/overview?days=30');
    const sel: HTMLSelectElement = fixture.nativeElement.querySelector(
      '[data-testid="overview-range"]',
    );
    sel.value = '7';
    sel.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    const second = http.expectOne('/api/v1/analytics/overview?days=7');

    second.flush({ sites: [site({ client_id: 'GOOD' })] });
    first.flush({ error: 'boom' }, { status: 500, statusText: 'Server Error' });
    fixture.detectChanges();

    expect(fixture.componentInstance.error()).toBe('');
    expect(fixture.componentInstance.sites().map((s) => s.client_id)).toEqual(['GOOD']);
  });

  it('announces load failures to assistive technology', () => {
    fixture.detectChanges();
    http
      .expectOne('/api/v1/analytics/overview?days=30')
      .flush({ error: 'boom' }, { status: 500, statusText: 'Server Error' });
    fixture.detectChanges();
    // Focus stays on the range select after a reload, so a silent error is never discovered.
    const err = fixture.nativeElement.querySelector('[data-testid="overview-error"]');
    expect(err.getAttribute('role')).toBe('alert');
  });

  it('business sections are h2, one level below the page h1', () => {
    flush([site()]);
    const heading = fixture.nativeElement.querySelector('[data-testid="overview-group-name"]');
    expect(heading.tagName).toBe('H2');
  });

  it('shows an empty state when there are no sites', () => {
    flush([]);
    expect(fixture.nativeElement.querySelector('[data-testid="overview-empty"]')).toBeTruthy();
  });

  it('surfaces an error without leaving a stale grid on screen', () => {
    fixture.detectChanges();
    http
      .expectOne('/api/v1/analytics/overview?days=30')
      .flush({ error: 'boom' }, { status: 500, statusText: 'Server Error' });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="overview-error"]')).toBeTruthy();
    expect(fixture.componentInstance.sites().length).toBe(0);
  });

  describe('sparkPoints', () => {
    it('scales to the series max and stays inside the viewBox', () => {
      const pts = fixture.componentInstance
        .sparkPoints([
          { date: 'a', pageviews: 0, visitors: 0 },
          { date: 'b', pageviews: 50, visitors: 0 },
          { date: 'c', pageviews: 100, visitors: 0 },
        ])
        .split(' ')
        .map((p) => p.split(',').map(Number));

      expect(pts.length).toBe(3);
      // x spans the full width; y is inverted (SVG origin is top-left).
      expect(pts[0][0]).toBeCloseTo(0, 1);
      expect(pts[2][0]).toBeCloseTo(100, 1);
      expect(pts[2][1]).toBeLessThan(pts[0][1]);
      for (const [x, y] of pts) {
        expect(x).toBeGreaterThanOrEqual(0);
        expect(x).toBeLessThanOrEqual(100);
        // The 1px inset keeps a max-value point off the very edge, where a 1.5px stroke would be
        // visibly clipped.
        expect(y).toBeGreaterThanOrEqual(0);
        expect(y).toBeLessThanOrEqual(28);
      }
    });

    it('does not divide by zero on an all-zero series', () => {
      const pts = fixture.componentInstance.sparkPoints([
        { date: 'a', pageviews: 0, visitors: 0 },
        { date: 'b', pageviews: 0, visitors: 0 },
      ]);
      expect(pts).not.toContain('NaN');
    });

    it('centres a single point rather than pinning it to x=0', () => {
      const pts = fixture.componentInstance.sparkPoints([{ date: 'a', pageviews: 5, visitors: 1 }]);
      expect(pts.split(',')[0]).toBe('50.00');
    });

    it('returns empty for an empty series', () => {
      expect(fixture.componentInstance.sparkPoints([])).toBe('');
    });
  });
});
