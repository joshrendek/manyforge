import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AnalyticsService, DayPoint, OverviewSite } from '../../core/analytics.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';

// The analytics landing page: every site the account can see, grouped by business.
//
// This replaces a screen that made you pick ONE business from a dropdown before it showed
// anything, which meant comparing sub-businesses was N page loads. The grid is the whole point —
// traffic across the tree is visible at a glance, and a card is a link into the existing per-site
// dashboard rather than a different way of reading the same numbers.
//
// The sparkline is hand-rolled SVG, matching the per-site chart. A charting dependency would be a
// far larger addition to the bundle than the handful of lines it replaces, and every card renders
// one, so its cost is multiplied.

interface BusinessGroup {
  id: string;
  name: string;
  sites: OverviewSite[];
}

@Component({
  selector: 'app-analytics-overview',
  imports: [RouterLink, PageHeader, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="analytics-overview-page">
      <mf-page-header title="Analytics" subtitle="Every site you can see, across all businesses">
        <span class="mf-hdr" actions>
          @if (loading()) {
            <mf-spinner data-testid="overview-loading" />
          }
          <label class="mf-sr-only" for="ov-range">Time range</label>
          <select
            id="ov-range"
            class="mf-select mf-select-sm"
            data-testid="overview-range"
            [value]="days()"
            (change)="setDays($event)"
          >
            <option value="7">Last 7 days</option>
            <option value="30">Last 30 days</option>
            <option value="90">Last 90 days</option>
          </select>
          <a class="mf-btn mf-btn-sm" routerLink="/analytics/sites" data-testid="overview-manage">
            Manage sites
          </a>
        </span>
      </mf-page-header>

      @if (!loading() && !groups().length && !error()) {
        <mf-empty-state title="No sites yet" data-testid="overview-empty">
          Add a site to get an embed tag, then paste it into your site's HTML.
        </mf-empty-state>
      }

      @for (g of groups(); track g.id) {
        <section class="mf-group" data-testid="overview-group" [attr.data-business-id]="g.id">
          <h2 class="mf-group-title" data-testid="overview-group-name">{{ g.name }}</h2>
          <div class="mf-grid">
            @for (s of g.sites; track s.client_id) {
              <a
                class="mf-site"
                data-testid="overview-card"
                [attr.data-client-id]="s.client_id"
                [routerLink]="['/analytics', s.business_id, s.client_id]"
              >
                <span class="mf-site-name" data-testid="overview-card-name">{{ s.name }}</span>
                <span class="mf-site-nums">
                  <span class="mf-site-visitors" data-testid="overview-card-visitors">{{
                    s.average_daily_visitors.toFixed(1)
                  }}</span>
                  <span class="mf-site-unit">average daily visitors</span>
                </span>
                <span class="mf-site-pv" data-testid="overview-card-pageviews">
                  {{ s.pageviews.toLocaleString() }} pageviews · peak
                  {{ s.visitors.toLocaleString() }} visitors
                </span>

                @if (hasTraffic(s)) {
                  <svg
                    class="mf-spark"
                    data-testid="overview-card-spark"
                    viewBox="0 0 100 28"
                    preserveAspectRatio="none"
                    aria-hidden="true"
                  >
                    <polyline [attr.points]="sparkPoints(s.series)" />
                  </svg>
                } @else {
                  <!-- An explicit "nothing yet" beats an empty box: a site that was tagged minutes
                       ago has no rollups, and silence there reads as a broken tag. -->
                  <span class="mf-site-none" data-testid="overview-card-nodata">
                    No data yet — check the embed tag
                  </span>
                }
              </a>
            }
          </div>
        </section>
      }

      @if (error()) {
        <!-- role=alert: this appears after an async reload, and a screen-reader user who just
             changed the range is still focused on the select and would otherwise never learn the
             reload failed. -->
        <p class="mf-err" role="alert" data-testid="overview-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .mf-hdr {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .mf-select-sm {
        width: auto;
        padding: 4px 8px;
        font-size: var(--mf-fs-xs);
      }
      .mf-group + .mf-group {
        margin-top: 22px;
      }
      .mf-group-title {
        font-size: var(--mf-fs-sm);
        font-weight: 600;
        color: var(--mf-text-muted);
        margin: 0 0 10px;
        padding-bottom: 6px;
        border-bottom: 1px solid var(--mf-border);
      }
      .mf-grid {
        display: grid;
        /* auto-fill rather than auto-fit: with one site, auto-fit would stretch a single card the
           full page width, which looks like a rendering fault rather than a layout. */
        grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
        gap: 12px;
      }
      .mf-site {
        display: flex;
        flex-direction: column;
        gap: 4px;
        padding: 14px;
        border: 1px solid var(--mf-border);
        border-radius: 8px;
        /* --mf-surface-inset, not --mf-surface-2. This is a surface nested inside .mf-card, which
           is what inset is for (board-detail uses it the same way). It also passes contrast:
           --mf-text-muted on --mf-surface-2 is 4.28:1 in light mode, BELOW the 4.5:1 AA minimum,
           and the card labels are all muted. On inset it is 4.76:1 light / 7.49:1 dark. */
        background: var(--mf-surface-inset);
        text-decoration: none;
        color: inherit;
        transition:
          border-color 0.12s ease,
          transform 0.12s ease;
      }
      .mf-site:hover,
      .mf-site:focus-visible {
        border-color: var(--mf-accent);
        transform: translateY(-1px);
      }
      .mf-site-name {
        font-weight: 600;
        font-size: var(--mf-fs-sm);
        overflow-wrap: anywhere;
      }
      .mf-site-nums {
        display: flex;
        align-items: baseline;
        gap: 6px;
      }
      .mf-site-visitors {
        font-size: 1.6rem;
        font-weight: 700;
        line-height: 1.1;
      }
      .mf-site-unit,
      .mf-site-pv,
      .mf-site-none {
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .mf-spark {
        width: 100%;
        height: 28px;
        margin-top: 6px;
      }
      .mf-spark polyline {
        fill: none;
        stroke: var(--mf-accent);
        stroke-width: 1.5;
        vector-effect: non-scaling-stroke;
      }
      .mf-site-none {
        margin-top: 6px;
        font-style: italic;
      }
      .mf-sr-only {
        position: absolute;
        width: 1px;
        height: 1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
      }
    `,
  ],
})
export class AnalyticsOverviewComponent implements OnInit {
  private api = inject(AnalyticsService);

  sites = signal<OverviewSite[]>([]);
  loading = signal(false);
  error = signal('');
  days = signal(30);
  // Monotonic request counter; only the newest response is allowed to write state.
  private reqToken = 0;

  // Sites arrive ordered by traffic. Grouping preserves that order within a business, and the
  // groups themselves follow first appearance — so the busiest business leads.
  groups = computed<BusinessGroup[]>(() => {
    const out: BusinessGroup[] = [];
    const byId = new Map<string, BusinessGroup>();
    for (const s of this.sites()) {
      let g = byId.get(s.business_id);
      if (!g) {
        g = { id: s.business_id, name: s.business_name, sites: [] };
        byId.set(s.business_id, g);
        out.push(g);
      }
      g.sites.push(s);
    }
    return out;
  });

  ngOnInit(): void {
    this.reload();
  }

  setDays(e: Event): void {
    const n = Number((e.target as HTMLSelectElement).value);
    if (!Number.isFinite(n) || n < 1) return;
    this.days.set(n);
    this.reload();
  }

  reload(): void {
    // A per-request token, NOT the requested range. Guarding on `days` alone looks equivalent but
    // is not: for 30 → 7 → 30, the first slow 30-day response still matches the current range when
    // it lands and would overwrite the newer 30-day result — the exact case the guard exists to
    // prevent. A monotonic token is unambiguous because it can only ever match the latest request.
    const token = ++this.reqToken;
    this.loading.set(true);
    this.api.overview(this.days()).subscribe({
      next: (r) => {
        if (token !== this.reqToken) return;
        this.sites.set(r.sites ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (token !== this.reqToken) return;
        this.sites.set([]);
        this.error.set('Could not load analytics');
        this.loading.set(false);
      },
    });
  }

  hasTraffic(s: OverviewSite): boolean {
    return s.series.some((p) => p.pageviews > 0);
  }

  // Points for a 100x28 viewBox. The API guarantees one ordered point per requested UTC day, so
  // equal index spacing is equal calendar spacing even across zero-traffic gaps.
  sparkPoints(series: DayPoint[]): string {
    if (!series.length) return '';
    const max = Math.max(...series.map((p) => p.pageviews), 1);
    const step = series.length > 1 ? 100 / (series.length - 1) : 0;
    return series
      .map((p, i) => {
        const x = series.length > 1 ? i * step : 50;
        // 1px inset top and bottom so a flat-max line is not clipped by the viewBox edge.
        const y = 27 - (p.pageviews / max) * 26;
        return `${x.toFixed(2)},${y.toFixed(2)}`;
      })
      .join(' ');
  }
}
