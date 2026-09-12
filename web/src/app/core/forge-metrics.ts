import { HttpClient, HttpErrorResponse, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable, catchError, forkJoin, map, of } from 'rxjs';
import { AccountingService } from './accounting.service';
import { AnalyticsService } from './analytics.service';
import { ApprovalItem } from './approvals.service';
import { CodeReviewService } from './code-review.service';
import { CrmService } from './crm.service';
import { MailingReport, MailingService } from './mailing.service';
import { Page, TicketService } from './ticket.service';

export type MetricState = 'ready' | 'loading' | 'pending' | 'denied' | 'error';
export interface ForgeMetric {
  state: MetricState;
  value: number | null;
  note: string;
  observed: number;
}
export type WorkKey = 'tickets' | 'urgent' | 'approvals' | 'failed' | 'spend';
export type ForgeWork = Record<WorkKey, ForgeMetric>;
export interface ForgeAuditEntry {
  id: string;
  business_id: string | null;
  actor_principal_id: string | null;
  action: string;
  target_type: string | null;
  target_id: string | null;
  correlation_id: string | null;
  created_at: string;
}
export interface ForgeAuditPage extends Page<ForgeAuditEntry> {
  state: MetricState;
  note: string;
}
export interface ForgeMailingReport {
  state: MetricState;
  report: MailingReport | null;
  note: string;
}
export const pendingMetric = (note: string): ForgeMetric => ({ state: 'pending', value: null, observed: 0, note });
export const loadingMetric = (): ForgeMetric => ({ state: 'loading', value: null, observed: 0, note: 'Loading authorized data…' });
export const readyMetric = (value: number, note: string): ForgeMetric => ({ state: 'ready', value, observed: value, note });
export function metricFailure(error: unknown): ForgeMetric {
  const denied = error instanceof HttpErrorResponse && (error.status === 403 || error.status === 404);
  return { state: denied ? 'denied' : 'error', value: null, observed: 0,
    note: denied ? "You don't have access to do that." : 'Could not load this metric. Refresh to try again.' };
}
export function sumMetrics(metrics: ForgeMetric[]): ForgeMetric {
  if (!metrics.length) return pendingMetric('No active businesses in this scope.');
  const incomplete = metrics.find(m => m.state === 'denied') ?? metrics.find(m => m.state === 'error')
    ?? metrics.find(m => m.state === 'loading') ?? metrics.find(m => m.state === 'pending');
  const observed = metrics.reduce((sum, metric) => sum + metric.observed, 0);
  return incomplete ? { ...incomplete, observed } : readyMetric(observed, 'Across the visible businesses only.');
}
export function emptyWork(): ForgeWork {
  return { tickets: loadingMetric(), urgent: loadingMetric(), approvals: loadingMetric(), failed: loadingMetric(), spend: loadingMetric() };
}
function boundedCount(count: number, complete: boolean, note: string): ForgeMetric {
  return complete ? readyMetric(count, note) : { ...pendingMetric('Exact total Pending: this source is paginated or capped. ' + note), observed: count };
}

@Injectable()
export class ForgeMetricsService {
  private readonly http = inject(HttpClient);
  private readonly tickets = inject(TicketService);
  private readonly reviews = inject(CodeReviewService);
  private readonly accounting = inject(AccountingService);
  private readonly analytics = inject(AnalyticsService);
  private readonly crm = inject(CrmService);
  private readonly mailing = inject(MailingService);

  // One bounded read per source, never a crawl through customer data for a dashboard total.
  work(businessId: string): Observable<ForgeWork> {
    const to = new Date();
    const from = new Date(to.getTime() - 7 * 86400000);
    return forkJoin({
      tickets: this.tickets.listTickets(businessId, { limit: 100 }).pipe(map(page => {
        const open = (page.items ?? []).filter(t => t.status === 'new' || t.status === 'open' || t.status === 'pending');
        return {
          tickets: boundedCount(open.length, page.next_cursor === null, 'New, open and pending tickets.'),
          urgent: boundedCount(open.filter(t => t.priority === 'urgent').length, page.next_cursor === null, 'Urgent unresolved tickets.'),
        };
      }), catchError(error => of({ tickets: metricFailure(error), urgent: metricFailure(error) }))),
      // Do not use ApprovalsService.listPending: its badge side effect would race business context.
      approvals: this.http.get<{ items: ApprovalItem[] }>(`/api/v1/businesses/${businessId}/approvals`).pipe(
        map(page => boundedCount(page.items.length, page.items.length < 50, 'Pending approval queue; server cap 50.')),
        catchError(error => of(metricFailure(error)))),
      failed: this.reviews.listReviews(businessId).pipe(
        map(page => boundedCount(page.items.filter(r => r.status === 'failed').length, page.items.length < 200, 'Failed review records; server cap 200.')),
        catchError(error => of(metricFailure(error)))),
      spend: this.accounting.getSummary(businessId, 'custom', from.toISOString(), to.toISOString()).pipe(
        map(summary => readyMetric(summary.totals.cost_cents, 'Recorded AI cost · trailing 7d. Unpriced provider usage may be excluded; not a billing invoice.')),
        catchError(error => of(metricFailure(error)))),
    }).pipe(map(result => ({ ...result, ...result.tickets })));
  }

  contacts(businessId: string): Observable<{ contacts: ForgeMetric; companies: ForgeMetric }> {
    const since = Date.now() - 7 * 86400000;
    return forkJoin({
      contacts: this.crm.listContacts(businessId).pipe(map(page => boundedCount(
        page.items.filter(c => Date.parse(c.created_at) >= since).length, page.next_cursor === null, 'New tenant contacts · trailing 7d.')),
        catchError(error => of(metricFailure(error)))),
      companies: this.crm.listCompanies(businessId).pipe(map(page => boundedCount(
        page.items.filter(c => Date.parse(c.created_at) >= since).length, page.next_cursor === null, 'New tenant companies · trailing 7d.')),
        catchError(error => of(metricFailure(error)))),
    });
  }

  mailingReport(): Observable<ForgeMailingReport> {
    return this.mailing.reporting().pipe(
      map(report => ({
        state: 'ready' as const,
        report,
        note: `${report.business_count} mailing-readable active businesses across ${report.tenant_count} tenants.`,
      })),
      catchError(error => of({ ...metricFailure(error), report: null })),
    );
  }

  visitors(): Observable<ForgeMetric> {
    return this.analytics.overview(7).pipe(map(overview => {
      if (!overview.data_as_of) return pendingMetric('Analytics rollups Pending: no completed data watermark.');
      if (overview.sites.length >= 200) return pendingMetric('Portfolio total Pending: analytics overview reaches its 200-site cap.');
      if (!overview.sites.length) return pendingMetric('No readable measured sites. Configure a site in Analytics.');
      return readyMetric(overview.sites.reduce((sum, site) => sum + site.average_daily_visitors, 0),
        `${overview.sites.length} readable sites · site-visitors, not deduplicated people · as of ${new Date(overview.data_as_of).toLocaleString()}. Comparison Pending.`);
    }), catchError(error => of(metricFailure(error))));
  }

  audit(businessId: string, limit = 3, cursor?: string): Observable<ForgeAuditPage> {
    let params = new HttpParams().set('limit', limit);
    if (cursor) params = params.set('cursor', cursor);
    return this.http.get<Page<ForgeAuditEntry>>(`/api/v1/businesses/${businessId}/audit`, { params }).pipe(
      map(page => ({ ...page, state: 'ready' as const, note: 'Authorized metadata only.' })),
      catchError(error => { const failure = metricFailure(error); return of({ items: [], next_cursor: null, state: failure.state, note: failure.note }); }),
    );
  }
}
