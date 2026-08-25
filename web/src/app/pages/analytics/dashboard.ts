import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AnalyticsService, AnalyticsSummary, DayPoint } from '../../core/analytics.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';

// Analytics dashboard for one site.
//
// The chart is hand-rolled SVG rather than a charting dependency: it shows one selectable daily
// series at a time, and a chart library would be a much larger addition to the bundle.
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
        <p class="mf-range" data-testid="analytics-date-range">
          Daily data in UTC: {{ s.from }} – {{ s.to }}. Compared with {{ s.comparison.from }} –
          {{ s.comparison.to }}.
          @if (s.data_as_of) {
            Data current through <time [attr.datetime]="s.data_as_of">{{ s.data_as_of }}</time
            >.
          } @else {
            Data freshness is not available yet.
          }
        </p>
        <div class="mf-stats" data-testid="analytics-stats">
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-pageviews">{{
              s.pageviews.toLocaleString()
            }}</span>
            <span class="mf-stat-label">Pageviews</span>
            <span class="mf-stat-change" data-testid="stat-pageviews-change">{{
              percentChangeLabel(s.comparison.pageviews_change_percent, s.comparison.pageviews)
            }}</span>
          </div>
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-visitors">{{
              s.average_daily_visitors.toFixed(1)
            }}</span>
            <span class="mf-stat-label">Average daily visitors</span>
            <span class="mf-stat-change" data-testid="stat-visitors-change">{{
              percentChangeLabel(
                s.comparison.average_daily_visitors_change_percent,
                s.comparison.average_daily_visitors,
                1
              )
            }}</span>
            <span class="mf-stat-detail">Peak day: {{ s.visitors.toLocaleString() }}</span>
          </div>
          <div class="mf-stat">
            <span class="mf-stat-value" data-testid="stat-direct"
              >{{ s.direct_share.toFixed(1) }}%</span
            >
            <span class="mf-stat-label">Direct traffic share</span>
            <span class="mf-stat-change" data-testid="stat-direct-change">{{
              pointChangeLabel(
                s.comparison.direct_share_change_percentage_points,
                s.comparison.direct_share
              )
            }}</span>
            <span class="mf-stat-detail">{{ s.direct_pageviews.toLocaleString() }} pageviews</span>
          </div>
        </div>

        @if (s.pageviews > 0) {
          <div class="mf-chart-header">
            <h2 class="mf-chart-title">Daily traffic</h2>
            <div class="mf-chart-toggle" aria-label="Chart metric">
              <button
                type="button"
                class="mf-btn mf-btn-sm"
                data-testid="chart-pageviews"
                [attr.aria-pressed]="chartMetric() === 'pageviews'"
                (click)="chartMetric.set('pageviews')"
              >
                Pageviews
              </button>
              <button
                type="button"
                class="mf-btn mf-btn-sm"
                data-testid="chart-visitors"
                [attr.aria-pressed]="chartMetric() === 'visitors'"
                (click)="chartMetric.set('visitors')"
              >
                Visitors
              </button>
            </div>
          </div>
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
                [attr.y]="120 - barHeight(chartValue(p))"
                [attr.width]="Math.max(barStep() - 2, 1)"
                [attr.height]="barHeight(chartValue(p))"
              >
                <title>{{ p.date }}: {{ p.pageviews }} pageviews, {{ p.visitors }} visitors</title>
              </rect>
            }
          </svg>
          <div class="mf-chart-axis" aria-hidden="true">
            <span>{{ s.from }}</span
            ><span>{{ s.to }}</span>
          </div>
          <details class="mf-chart-data" data-testid="analytics-chart-data">
            <summary>View daily data</summary>
            <div class="mf-table" role="table" aria-label="Daily analytics data">
              <div class="mf-tr mf-th" role="row">
                <span role="columnheader">Date (UTC)</span>
                <span role="columnheader">Pageviews</span>
                <span role="columnheader">Visitors</span>
              </div>
              @for (p of s.series; track p.date) {
                <div class="mf-tr" role="row" data-testid="daily-data-row">
                  <span role="cell">{{ p.date }}</span>
                  <span role="cell">{{ p.pageviews }}</span>
                  <span role="cell">{{ p.visitors }}</span>
                </div>
              }
            </div>
          </details>
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

        @if (propertyPanels().length) {
          <div class="mf-property-section" data-testid="property-breakdowns">
            <h2 class="mf-chart-title">Event properties</h2>
            <div class="mf-two-col">
              @for (property of propertyPanels(); track property.rule_id) {
                <div>
                  <h3 class="mf-subhead" [id]="'property-label-' + property.rule_id">
                    {{ property.label }}
                  </h3>
                  <p class="mf-property-context">
                    {{ property.event_name }} · {{ property.property_key }}
                  </p>
                  <table
                    class="mf-table mf-property-table"
                    data-testid="property-breakdown"
                    [attr.aria-labelledby]="'property-label-' + property.rule_id"
                  >
                    <thead>
                      <tr>
                        <th scope="col">{{ property.label }}</th>
                        <th scope="col">Events</th>
                      </tr>
                    </thead>
                    <tbody>
                      @for (value of property.values; track value.value) {
                        <tr data-testid="property-breakdown-row">
                          <td>
                            <span class="mf-ellipsis" [title]="value.value">{{ value.value }}</span>
                          </td>
                          <td>{{ value.events }}</td>
                        </tr>
                      }
                    </tbody>
                  </table>
                </div>
              }
            </div>
          </div>
        }

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

        @if (s.pageviews === 0 && !loading()) {
          <mf-empty-state title="No traffic yet" data-testid="analytics-empty">
            Paste this site's embed tag into your HTML and reload the page. Data appears within a
            minute or two.
          </mf-empty-state>
        }
      }

      <div data-testid="country-status" role="status">
        @if (countryUnavailable()) {
          <p class="mf-hint" data-testid="country-unavailable">
            Country data is unavailable for this date range. It appears only when the deployment's
            trusted edge supplies a supported country for a request.
          </p>
        }
      </div>

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
      .mf-stat-change,
      .mf-stat-detail,
      .mf-range,
      .mf-chart-axis,
      .mf-chart-data {
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .mf-stat-change {
        margin-top: 4px;
      }
      .mf-range {
        margin: 12px 0 0;
      }
      .mf-chart-header,
      .mf-chart-axis {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 12px;
      }
      .mf-chart-title {
        font-size: var(--mf-fs-sm);
        margin: 0;
      }
      .mf-chart-toggle {
        display: flex;
        gap: 6px;
      }
      .mf-chart-toggle [aria-pressed='true'] {
        border-color: var(--mf-accent);
        color: var(--mf-accent);
      }
      .mf-chart {
        width: 100%;
        height: 120px;
        margin: 8px 0 4px;
        display: block;
      }
      .mf-bar {
        fill: var(--mf-accent, #4f8ef7);
      }
      .mf-chart-data {
        margin: 8px 0 20px;
      }
      .mf-chart-data summary {
        cursor: pointer;
      }
      .mf-chart-data .mf-table {
        margin-top: 8px;
        max-height: 260px;
        overflow: auto;
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
        color: var(--mf-text);
        margin-top: 16px;
      }
      .mf-property-section {
        margin-top: 24px;
      }
      .mf-property-context {
        margin: -4px 0 8px;
        color: var(--mf-text-muted);
        font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
        font-size: var(--mf-fs-xs);
        overflow-wrap: anywhere;
      }
      .mf-property-table {
        width: 100%;
        border-spacing: 0;
        table-layout: fixed;
      }
      .mf-property-table th,
      .mf-property-table td {
        padding: 11px 14px;
        border-bottom: 1px solid var(--mf-border);
        text-align: left;
      }
      .mf-property-table th {
        background: var(--mf-surface-2);
        color: var(--mf-text-faint);
        font-size: var(--mf-fs-xs);
        font-weight: 600;
        letter-spacing: 0.04em;
        text-transform: uppercase;
      }
      .mf-property-table th:first-child,
      .mf-property-table td:first-child {
        width: 75%;
      }
      .mf-property-table tbody tr:last-child td {
        border-bottom: 0;
      }
      .mf-property-table .mf-ellipsis {
        display: block;
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
  chartMetric = signal<'pageviews' | 'visitors'>('pageviews');
  summary = signal<AnalyticsSummary | null>(null);
  loading = signal(false);
  error = signal('');

  private maxChartValue = computed(() => {
    const s = this.summary();
    if (!s || !s.series.length) return 0;
    const metric = this.chartMetric();
    return s.series.reduce((max, point) => Math.max(max, point[metric]), 0);
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

  // Active rules with no values remain useful in the management surface, but an empty dashboard
  // table is noise. Render only properties with aggregate data in the selected window.
  propertyPanels = computed(() =>
    (this.summary()?.property_breakdowns ?? []).filter((property) => property.values.length > 0),
  );

  // Traffic with no resolved countries can mean a historical range, absent/unsupported edge
  // values, or no trusted edge signal. Surface that neutral state rather than inferring which
  // deployment condition caused it.
  countryUnavailable = computed(() => {
    const s = this.summary();
    if (!s || s.pageviews === 0) return false;
    return (s.breakdowns?.['country']?.length ?? 0) === 0;
  });

  chartLabel = computed(() => {
    const s = this.summary();
    if (!s) return 'Pageviews chart';
    const label = this.chartMetric() === 'pageviews' ? 'Pageviews' : 'Daily visitors';
    return `${label} per day from ${s.from} to ${s.to}, peak ${this.maxChartValue()}`;
  });

  barHeight(v: number): number {
    const max = this.maxChartValue();
    if (max <= 0) return 0;
    return Math.max((v / max) * 110, v > 0 ? 2 : 0);
  }

  chartValue(point: DayPoint): number {
    return point[this.chartMetric()];
  }

  percentChangeLabel(change: number | null, previous: number, previousDigits = 0): string {
    if (change === null) return `No prior baseline (${previous.toFixed(previousDigits)} prior)`;
    const prefix = change > 0 ? '+' : '';
    return `${prefix}${change.toFixed(1)}% vs ${previous.toFixed(previousDigits)} prior`;
  }

  pointChangeLabel(change: number, previous: number): string {
    const prefix = change > 0 ? '+' : '';
    return `${prefix}${change.toFixed(1)} points vs ${previous.toFixed(1)}% prior`;
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
