import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AnalyticsService, AnalyticsSummary } from '../../core/analytics.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';

// Analytics dashboard for one site.
//
// The chart is hand-rolled SVG rather than a charting dependency: it is a single bar series, and
// a chart library would be a much larger addition to the bundle than the ~20 lines it replaces.
@Component({
  selector: 'app-analytics-dashboard',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="analytics-dashboard-page">
      <mf-page-header title="Analytics" subtitle="Privacy-first, cookieless traffic">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="analytics-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:0 0 160px">
          <label for="an-range">Range</label>
          <select
            id="an-range"
            class="mf-select"
            data-testid="range-select"
            [ngModel]="days()"
            (ngModelChange)="setDays($event)"
            name="days"
          >
            <option [value]="7">Last 7 days</option>
            <option [value]="30">Last 30 days</option>
            <option [value]="90">Last 90 days</option>
          </select>
        </div>
        <div class="mf-field" style="align-self:flex-end">
          <a class="mf-btn mf-btn-sm" [routerLink]="['/analytics']" data-testid="back-to-sites">
            All sites
          </a>
        </div>
      </div>

      @if (summary(); as s) {
        <div class="mf-stats" data-testid="analytics-stats">
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-pageviews">{{ s.pageviews }}</span>
            <span class="mf-stat-label">Pageviews</span>
          </div>
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-visitors">{{ s.visitors }}</span>
            <!-- Labelled "peak daily" rather than "unique visitors" because the visitor hash
                 rotates daily by design: there is no cross-day identifier, so a deduplicated
                 multi-day total does not exist and must not be implied. -->
            <span class="mf-stat-label">Visitors (peak day)</span>
          </div>
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-direct">{{ s.direct_pageviews }}</span>
            <span class="mf-stat-label">Direct</span>
          </div>
        </div>

        @if (s.pageviews > 0) {
          <svg
            class="mf-chart"
            data-testid="analytics-chart"
            [attr.viewBox]="'0 0 ' + chartWidth() + ' 120'"
            preserveAspectRatio="none"
            role="img"
            [attr.aria-label]="chartLabel()"
          >
            @for (p of s.series; track p.date; let i = $index) {
              <rect
                class="mf-bar"
                [attr.x]="i * barStep()"
                [attr.y]="120 - barHeight(p.pageviews)"
                [attr.width]="Math.max(barStep() - 2, 1)"
                [attr.height]="barHeight(p.pageviews)"
              >
                <title>{{ p.date }}: {{ p.pageviews }} pageviews, {{ p.visitors }} visitors</title>
              </rect>
            }
          </svg>
        }

        <div class="mf-two-col">
          <div>
            <h3 class="mf-subhead">Top pages</h3>
            <div class="mf-table" data-testid="top-pages" role="table" aria-label="Top pages">
              <div class="mf-tr mf-th" role="row">
                <span style="flex:3" role="columnheader">Page</span>
                <span style="flex:1" role="columnheader">Pageviews</span>
              </div>
              @for (p of s.top_pages; track p.path) {
                <div class="mf-tr" role="row" data-testid="top-page-row">
                  <span style="flex:3" class="mf-ellipsis" role="cell" [title]="p.path">{{
                    p.path
                  }}</span>
                  <span style="flex:1" role="cell">{{ p.pageviews }}</span>
                </div>
              }
              @if (!s.top_pages.length) {
                <mf-empty-state title="No pages yet"
                  >Waiting for the first pageview.</mf-empty-state
                >
              }
            </div>
          </div>
          <div>
            <h3 class="mf-subhead">Top referrers</h3>
            <div
              class="mf-table"
              data-testid="top-referrers"
              role="table"
              aria-label="Top referrers"
            >
              <div class="mf-tr mf-th" role="row">
                <span style="flex:3" role="columnheader">Referrer</span>
                <span style="flex:1" role="columnheader">Pageviews</span>
              </div>
              @for (r of s.top_referrers; track r.host) {
                <div class="mf-tr" role="row" data-testid="top-referrer-row">
                  <span style="flex:3" class="mf-ellipsis" role="cell" [title]="r.host">{{
                    r.host
                  }}</span>
                  <span style="flex:1" role="cell">{{ r.pageviews }}</span>
                </div>
              }
              @if (!s.top_referrers.length) {
                <mf-empty-state title="No referrers">
                  All traffic so far is direct.
                </mf-empty-state>
              }
            </div>
          </div>
        </div>

        <div class="mf-two-col">
          @for (b of breakdownPanels(); track b.key) {
            <div>
              <h3 class="mf-subhead">{{ b.label }}</h3>
              <div
                class="mf-table"
                [attr.data-testid]="'breakdown-' + b.key"
                role="table"
                [attr.aria-label]="b.label"
              >
                <div class="mf-tr mf-th" role="row">
                  <span style="flex:3" role="columnheader">{{ b.label }}</span>
                  <span style="flex:1" role="columnheader">{{ b.metric }}</span>
                </div>
                @for (v of b.rows; track v.value) {
                  <div class="mf-tr" role="row" data-testid="breakdown-row">
                    <span style="flex:3" class="mf-ellipsis" role="cell" [title]="v.value">{{
                      v.value
                    }}</span>
                    <span style="flex:1" role="cell">{{ v.pageviews }}</span>
                  </div>
                }
              </div>
            </div>
          }
        </div>

        @if (countryUnavailable()) {
          <p class="mf-hint" data-testid="country-unavailable" role="status">
            Country data is unavailable for this date range. Future collection uses Cloudflare's
            trusted <code>CF-IPCountry</code> signal when
            <code>MANYFORGE_TRUST_CF_IPCOUNTRY</code> is enabled.
          </p>
        }

        @if (s.pageviews === 0 && !loading()) {
          <mf-empty-state title="No traffic yet" data-testid="analytics-empty">
            Paste this site's embed tag into your HTML and reload the page. Data appears within a
            minute or two.
          </mf-empty-state>
        }
      }

      @if (error()) {
        <p class="mf-err" data-testid="analytics-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .mf-stats {
        display: flex;
        gap: 24px;
        flex-wrap: wrap;
        margin: 16px 0;
      }
      .mf-stat {
        display: flex;
        flex-direction: column;
        min-width: 120px;
      }
      .mf-stat-value {
        font-size: 28px;
        font-weight: 600;
      }
      .mf-stat-label {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
      }
      .mf-chart {
        width: 100%;
        height: 120px;
        margin: 8px 0 20px;
        display: block;
      }
      .mf-bar {
        fill: var(--mf-accent, #4f8ef7);
      }
      .mf-two-col {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
        gap: 24px;
      }
      .mf-subhead {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
        margin: 0 0 8px;
      }
      .mf-ellipsis {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .mf-hint {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
        margin-top: 16px;
      }
    `,
  ],
})
export class AnalyticsDashboardComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(AnalyticsService);

  // Exposed so the template can call Math.max in the bar width expression.
  protected readonly Math = Math;

  businessId = signal('');
  siteId = signal('');
  days = signal(30);
  summary = signal<AnalyticsSummary | null>(null);
  loading = signal(false);
  error = signal('');

  private maxPageviews = computed(() => {
    const s = this.summary();
    if (!s || !s.series.length) return 0;
    return s.series.reduce((m, p) => (p.pageviews > m ? p.pageviews : m), 0);
  });

  chartWidth = computed(() => Math.max((this.summary()?.series.length ?? 0) * 12, 1));
  barStep = computed(() => 12);

  // Only dimensions that actually have data get a panel. Rendering six empty tables on a site
  // that has never used a UTM tag is noise, not information.
  breakdownPanels = computed(() => {
    const b = this.summary()?.breakdowns ?? {};
    const labels: Record<string, string> = {
      event: 'Events',
      country: 'Countries',
      device: 'Devices',
      browser: 'Browsers',
      utm_source: 'Campaign source',
      utm_medium: 'Campaign medium',
      utm_campaign: 'Campaign',
    };
    return Object.keys(labels)
      .filter((k) => (b[k]?.length ?? 0) > 0)
      .map((k) => ({
        key: k,
        label: labels[k],
        // The 'event' dimension counts CUSTOM EVENTS, not pageviews. The API reuses the
        // `pageviews` field across every dimension, so labelling the column generically would
        // report event counts under the wrong metric — a number that silently disagrees with the
        // pageview total above it.
        metric: k === 'event' ? 'Events' : 'Pageviews',
        rows: b[k],
      }));
  });

  // Distinguishes "no trusted country signal configured" from "no traffic yet": if there are
  // pageviews but not a single country was resolved, explain the missing deployment signal rather
  // than presenting an audience from nowhere.
  countryUnavailable = computed(() => {
    const s = this.summary();
    if (!s || s.pageviews === 0) return false;
    return (s.breakdowns?.['country']?.length ?? 0) === 0;
  });

  chartLabel = computed(() => {
    const s = this.summary();
    if (!s) return 'Pageviews chart';
    return `Pageviews per day from ${s.from} to ${s.to}, peak ${this.maxPageviews()}`;
  });

  barHeight(v: number): number {
    const max = this.maxPageviews();
    if (max <= 0) return 0;
    return Math.max((v / max) * 110, v > 0 ? 2 : 0);
  }

  ngOnInit(): void {
    this.businessId.set(this.route.snapshot.paramMap.get('businessId') ?? '');
    this.siteId.set(this.route.snapshot.paramMap.get('siteId') ?? '');
    this.reload();
  }

  setDays(d: number | string): void {
    this.days.set(Number(d));
    this.reload();
  }

  reload(): void {
    const biz = this.businessId();
    const site = this.siteId();
    if (!biz || !site) return;
    this.loading.set(true);
    this.api.summary(biz, site, this.days()).subscribe({
      next: (s) => {
        this.summary.set(s);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        this.summary.set(null);
        this.error.set('Could not load analytics');
        this.loading.set(false);
      },
    });
  }
}
