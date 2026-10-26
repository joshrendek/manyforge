import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { timer } from 'rxjs';
import { CurrentBusinessService } from '../../core/current-business.service';
import {
  SupportOverview,
  SupportOverviewBusiness,
  TicketService,
} from '../../core/ticket.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatTile, StatTiles } from '../../ui/stat-tiles/stat-tiles';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ticketPriorityTone, ticketStatusTone } from '../../ui/status';

// How often the overview re-polls while open. The query is a single aggregate, so a minute is
// cheap and keeps "waiting 3h" honest without a manual refresh.
export const OVERVIEW_REFRESH_MS = 60_000;

// formatDuration renders a non-negative span in seconds as its two largest units
// ("2d 4h", "3h 12m", "45m", "<1m"). Null in → em dash, the table's "no data" mark.
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds)) return '—';
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return '<1m';
}

// waitedSeconds is how long ago `since` was, measured against the server's as_of so the
// numbers on one page agree with each other regardless of client clock skew.
export function waitedSeconds(since: string | null, asOf: string): number | null {
  if (!since) return null;
  return (Date.parse(asOf) - Date.parse(since)) / 1000;
}

// Cross-business by design: every business where the caller holds tickets.read, not the
// global business context. Opening a business row switches that context, then lands on /support.
@Component({
  selector: 'app-support-overview',
  imports: [RouterLink, PageHeader, EmptyState, Spinner, StatTiles, StatusPill],
  template: `
    <div class="mf-card" data-testid="support-overview-page">
      <mf-page-header
        eyebrow="All businesses"
        title="Support overview"
        subtitle="Who is waiting on a reply, across every business you can read support for."
      >
        <span class="mf-hdr" actions>
          @if (loading()) {
            <mf-spinner data-testid="support-overview-loading" />
          }
          <button
            type="button"
            class="mf-btn mf-btn-ghost mf-btn-sm"
            data-testid="support-overview-refresh"
            (click)="reload()"
          >
            Refresh
          </button>
          <a class="mf-btn mf-btn-sm" routerLink="/support" data-testid="support-overview-inbox">Inbox</a>
        </span>
      </mf-page-header>

      @if (error()) {
        <!-- role=alert: a poll failure lands while the user is reading, so announce it. -->
        <div class="mf-empty-inline" role="alert" data-testid="support-overview-error">
          <p>{{ error() }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      }

      @if (data(); as d) {
        @if (!d.businesses.length) {
          <mf-empty-state title="No support to show" data-testid="support-overview-empty">
            You don't have support access in any business yet.
          </mf-empty-state>
        } @else {
          <mf-stat-tiles [tiles]="tiles()" />

          <section class="sov-block" aria-labelledby="attn-h">
            <h2 id="attn-h" class="mf-section">
              Needs a reply
              <span class="mf-count" data-testid="support-overview-attention-count">{{ d.totals.awaiting_reply }}</span>
            </h2>
            @if (d.attention.length) {
              <div class="mf-table" data-testid="support-overview-attention">
                <div class="mf-tr mf-th">
                  <span style="flex:3">Subject</span>
                  <span class="sov-wide" style="flex:1.4">Business</span>
                  <span style="width:84px">Priority</span>
                  <span class="sov-wide" style="width:70px">Status</span>
                  <span class="sov-wide" style="width:92px">Owner</span>
                  <span class="mf-td-num" style="width:84px">Waiting</span>
                </div>
                @for (t of d.attention; track t.ticket_id) {
                  <a
                    class="mf-tr mf-clickable mf-row-link"
                    data-testid="support-overview-attention-row"
                    [attr.data-ticket-id]="t.ticket_id"
                    [routerLink]="['/support', t.business_id, t.ticket_id]"
                  >
                    <span class="sov-subject" style="flex:3">
                      <span class="mf-tr-name" style="font-weight:500">{{ t.subject || '(no subject)' }}</span>
                      <span class="mf-muted sov-narrow">{{ t.business_name }}</span>
                    </span>
                    <span class="mf-muted sov-wide" style="flex:1.4">{{ t.business_name }}</span>
                    <span style="width:84px"
                      ><mf-status-pill [tone]="priorityTone(t.priority)" [label]="t.priority"
                    /></span>
                    <span class="sov-wide" style="width:70px"><mf-status-pill [tone]="statusTone(t.status)" [label]="t.status" outline /></span>
                    <span class="sov-wide" style="width:92px" [class.mf-warn-text]="!t.assigned">{{ t.assigned ? 'Assigned' : 'Unassigned' }}</span>
                    <span class="mf-td-num" style="width:84px" data-testid="support-overview-attention-waiting">{{
                      wait(t.waiting_since, d.as_of)
                    }}</span>
                  </a>
                }
              </div>
              @if (d.totals.awaiting_reply > d.attention.length) {
                <p class="mf-note">
                  Showing the {{ d.attention.length }} most urgent of {{ d.totals.awaiting_reply }} waiting tickets.
                </p>
              }
            } @else {
              <p class="mf-empty-inline" data-testid="support-overview-caught-up">Nobody is waiting on a reply.</p>
            }
          </section>

          <section class="sov-block" aria-labelledby="biz-h">
            <h2 id="biz-h" class="mf-section">By business</h2>
            <div class="sov-scroll">
              <table class="sov-table" data-testid="support-overview-businesses">
                <thead>
                  <tr>
                    <th scope="col" class="sov-name">Business</th>
                    <th scope="col">Waiting</th>
                    <th scope="col">Open</th>
                    <th scope="col">New</th>
                    <th scope="col">Pending</th>
                    <th scope="col">Unassigned</th>
                    <th scope="col">Urgent / high</th>
                    <th scope="col">Oldest wait</th>
                    <th scope="col">7d in / solved</th>
                    <th scope="col">1st reply</th>
                  </tr>
                </thead>
                <tbody>
                  @for (b of d.businesses; track b.business_id) {
                    <!-- The whole row is clickable for the mouse; the name button is the keyboard
                         and screen-reader target, so the row itself needs no role. -->
                    <tr
                      class="sov-row"
                      data-testid="support-overview-business-row"
                      [attr.data-business-id]="b.business_id"
                      (click)="openBusiness(b)"
                    >
                      <th scope="row" class="sov-name">
                        <button
                          type="button"
                          class="sov-open"
                          [attr.aria-label]="'Open support inbox for ' + b.business_name"
                          (click)="$event.stopPropagation(); openBusiness(b)"
                        >
                          {{ b.business_name }}
                        </button>
                      </th>
                      <td [class.mf-hot]="b.awaiting_reply > 0" data-testid="biz-awaiting">{{ b.awaiting_reply }}</td>
                      <td data-testid="biz-open">{{ b.open }}</td>
                      <td>{{ b.new }}</td>
                      <td>{{ b.pending }}</td>
                      <td [class.mf-warn-text]="b.unassigned > 0">{{ b.unassigned }}</td>
                      <td><span [class.mf-hot]="b.urgent > 0">{{ b.urgent }}</span> / {{ b.high }}</td>
                      <td data-testid="biz-oldest">{{ wait(b.oldest_awaiting_at, d.as_of) }}</td>
                      <td data-testid="biz-flow">{{ b.created_7d }} / {{ b.resolved_7d }}</td>
                      <td data-testid="biz-frt">{{ duration(b.median_first_response_seconds_7d) }}</td>
                    </tr>
                  }
                </tbody>
              </table>
            </div>
          </section>

          <p class="mf-note" data-testid="support-overview-asof">
            As of <time [attr.datetime]="d.as_of">{{ asOfLabel() }}</time>. "1st reply" is the median time to
            the first reply for tickets created in the last 7 days.
          </p>
        }
      } @else if (!error()) {
        <div class="mf-loading-row">
          <mf-spinner />
          <span>Loading support overview…</span>
        </div>
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
      .sov-block {
        margin-top: 4px;
      }
      .sov-block h2 {
        font-size: inherit;
      }
      /* Eleven numeric columns don't fit a narrow card; scroll the table, not the page. */
      .sov-scroll {
        overflow-x: auto;
      }
      .sov-table {
        width: 100%;
        border-collapse: collapse;
        background: var(--mf-surface);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
      }
      .sov-table th,
      .sov-table td {
        padding: 10px 8px;
        border-bottom: 1px solid var(--mf-border);
        text-align: right;
        white-space: nowrap;
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-sm);
        font-variant-numeric: tabular-nums;
      }
      .sov-table thead th {
        background: var(--mf-surface-2);
        font: var(--mf-text-eyebrow);
        letter-spacing: var(--mf-tracking-label);
        text-transform: uppercase;
        color: var(--mf-text-faint);
      }
      .sov-table tbody tr:last-child > * {
        border-bottom: 0;
      }
      .sov-table .sov-name {
        text-align: left;
        width: 100%;
      }
      .sov-row {
        cursor: pointer;
      }
      .sov-row:hover {
        background: var(--mf-surface-2);
      }
      .sov-open {
        background: none;
        border: 0;
        padding: 0;
        color: inherit;
        font: inherit;
        font-family: var(--mf-font);
        font-size: var(--mf-fs-base);
        font-weight: 500;
        cursor: pointer;
        text-align: left;
      }
      .sov-row:hover .sov-open,
      .sov-open:focus-visible {
        color: var(--mf-accent-text);
      }
      .mf-count {
        font-family: var(--mf-font-mono);
        font-variant-numeric: tabular-nums;
        color: var(--mf-text);
      }
      .mf-row-link {
        text-decoration: none;
        color: inherit;
        width: 100%;
        background: none;
        border: 0;
        border-bottom: 1px solid var(--mf-border);
        font: inherit;
        text-align: left;
      }
      .mf-muted {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-xs);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .mf-hot {
        color: var(--mf-danger-text);
        font-weight: 600;
      }
      .mf-warn-text {
        color: var(--mf-warn-text);
      }
      .sov-subject {
        display: flex;
        flex-direction: column;
        min-width: 0;
        padding: 6px 0;
      }
      .sov-narrow {
        display: none;
      }
      /* Phones keep subject, priority and wait; business moves under the subject. */
      @media (max-width: 640px) {
        .sov-wide {
          display: none;
        }
        .sov-narrow {
          display: block;
        }
      }
      .mf-note {
        margin: 8px 0 0;
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
    `,
  ],
})
export class SupportOverviewComponent implements OnInit {
  private api = inject(TicketService);
  private current = inject(CurrentBusinessService);
  private router = inject(Router);
  private destroyRef = inject(DestroyRef);

  data = signal<SupportOverview | null>(null);
  loading = signal(false);
  error = signal('');
  // Only the newest request may write state (a slow poll must not overwrite a manual refresh).
  private reqToken = 0;

  tiles = computed<StatTile[]>(() => {
    const d = this.data();
    if (!d) return [];
    const t = d.totals;
    return [
      { label: 'Waiting on a reply', value: t.awaiting_reply, detail: `oldest ${this.wait(t.oldest_awaiting_at, d.as_of)}`, testid: 'kpi-awaiting' },
      { label: 'Open tickets', value: t.open, detail: `${t.new} new · ${t.pending} pending`, testid: 'kpi-open' },
      { label: 'Unassigned', value: t.unassigned, testid: 'kpi-unassigned' },
      { label: 'Urgent / high', value: `${t.urgent} / ${t.high}`, testid: 'kpi-priority' },
      { label: 'Created / resolved (7d)', value: `${t.created_7d} / ${t.resolved_7d}`, testid: 'kpi-flow' },
      { label: 'Median first reply (7d)', value: formatDuration(t.median_first_response_seconds_7d), testid: 'kpi-frt' },
    ];
  });

  asOfLabel = computed(() => {
    const d = this.data();
    return d ? new Date(d.as_of).toLocaleString() : '';
  });

  ngOnInit(): void {
    this.reload();
    timer(OVERVIEW_REFRESH_MS, OVERVIEW_REFRESH_MS)
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe(() => this.reload());
  }

  reload(): void {
    const token = ++this.reqToken;
    this.loading.set(true);
    this.api
      .overview()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (r) => {
          if (token !== this.reqToken) return;
          this.data.set(r);
          this.error.set('');
          this.loading.set(false);
        },
        error: () => {
          if (token !== this.reqToken) return;
          // Keep the last good numbers on screen; a transient poll failure shouldn't blank them.
          this.error.set("We couldn't load the support overview.");
          this.loading.set(false);
        },
      });
  }

  openBusiness(b: SupportOverviewBusiness): void {
    this.current.set(b.business_id);
    void this.router.navigate(['/support']);
  }

  wait(since: string | null, asOf: string): string {
    return formatDuration(waitedSeconds(since, asOf));
  }

  duration(seconds: number | null): string {
    return formatDuration(seconds);
  }

  priorityTone = ticketPriorityTone;
  statusTone = ticketStatusTone;
}
