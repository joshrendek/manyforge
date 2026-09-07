import { Component, ElementRef, ViewChild, computed, effect, inject, input, output, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { Router } from '@angular/router';
import { forkJoin, from, mergeMap, Subscription, tap } from 'rxjs';
import { Business } from '../core/tree';
import { CurrentBusinessService } from '../core/current-business.service';
import { ForgeAuditEntry, ForgeAuditPage, ForgeMetric, ForgeMetricsService, ForgeWork, WorkKey, emptyWork, loadingMetric, pendingMetric, sumMetrics } from '../core/forge-metrics';
import { PageHeader } from '../ui/page-header/page-header';
import { StatusPill } from '../ui/status-pill/status-pill';

interface FloorChild { business: Business; parentName: string; depth: number }
interface FloorGroup { business: Business; children: FloorChild[]; ids: string[] }
interface FloorTile { label: string; metric: ForgeMetric; route?: string; detail: string; tone?: string }

@Component({
  selector: 'mf-forge-floor',
  imports: [DatePipe, PageHeader, StatusPill],
  providers: [ForgeMetricsService],
  templateUrl: './forge-floor.html',
  styleUrl: './forge-floor.css',
})
export class ForgeFloorComponent {
  readonly businesses = input.required<Business[]>();
  readonly manage = output<void>();
  readonly light = output<void>();
  private readonly api = inject(ForgeMetricsService);
  private readonly current = inject(CurrentBusinessService);
  private readonly router = inject(Router);
  @ViewChild('auditPanel') set auditPanel(panel: ElementRef<HTMLElement> | undefined) {
    if (!panel) return;
    panel.nativeElement.scrollIntoView({ block: 'start' });
    panel.nativeElement.focus({ preventScroll: true });
  }
  readonly work = signal<Record<string, ForgeWork>>({});
  readonly crm = signal<Record<string, { contacts: ForgeMetric; companies: ForgeMetric }>>({});
  readonly visitors = signal<ForgeMetric>(loadingMetric());
  readonly auditPages = signal<Record<string, ForgeAuditPage>>({});
  readonly refreshKey = signal(0);
  readonly auditBusiness = signal<Business | null>(null);
  readonly fullAudit = signal<ForgeAuditPage | null>(null);
  readonly auditLoading = signal(false);
  private auditRequest?: Subscription;
  readonly active = computed(() => this.businesses().filter(b => b.status === 'active'));
  readonly groups = computed<FloorGroup[]>(() => {
    const all = this.active();
    const byId = new Map(all.map(b => [b.id, b]));
    const byParent = new Map<string, Business[]>();
    for (const b of all) if (b.parent_id && byId.has(b.parent_id)) {
      const children = byParent.get(b.parent_id) ?? []; children.push(b); byParent.set(b.parent_id, children);
    }
    const visited = new Set<string>();
    const groups: FloorGroup[] = [];
    // Visible orphans are real stations, never relabeled as tenant masters.
    const roots = [...all.filter(b => !b.parent_id || !byId.has(b.parent_id)), ...all];
    for (const root of roots) {
      if (visited.has(root.id)) continue;
      visited.add(root.id);
      const group: FloorGroup = { business: root, children: [], ids: [root.id] };
      const queue = (byParent.get(root.id) ?? []).map(business => ({ business, parentName: root.name, depth: 1 }));
      for (let i = 0; i < queue.length; i++) {
        const child = queue[i];
        if (visited.has(child.business.id)) continue;
        visited.add(child.business.id); group.children.push(child); group.ids.push(child.business.id);
        for (const business of byParent.get(child.business.id) ?? []) queue.push({ business, parentName: child.business.name, depth: child.depth + 1 });
      }
      groups.push(group);
    }
    return groups;
  });
  readonly totals = computed(() => this.rollup(this.active().map(b => b.id)));
  readonly hearthCount = computed(() => this.groups().filter(g => g.business.is_tenant_root).length);
  readonly lit = computed(() => this.groups().filter(g => g.business.is_tenant_root && ['hot', 'warm'].includes(this.heat(this.rollup(g.ids)))).length);
  readonly needs = computed<FloorTile[]>(() => [
    { label: 'Open tickets', metric: this.totals().tickets, route: '/support', tone: this.totals().urgent.observed > 0 ? 'danger' : '', detail: `${this.format(this.totals().urgent)} urgent · waiting over 24h Pending` },
    { label: 'Approvals', metric: this.totals().approvals, route: '/approvals', tone: 'warn', detail: 'Oldest approval age Pending' },
    { label: 'Done today', metric: pendingMetric('Completion event aggregates are not available yet.'), tone: 'success', detail: 'Tickets solved · reviews posted Pending' },
  ]);
  readonly growth = computed<FloorTile[]>(() => {
    const crm = Object.values(this.crm());
    return [
      { label: 'MRR', metric: pendingMetric('Billing revenue and currency source not connected.'), detail: 'Revenue change Pending' },
      { label: 'Users', metric: pendingMetric('Product login-user identity source not connected.'), detail: '7d signups Pending' },
      { label: "What's billed", metric: pendingMetric('Billable units, labels and usage periods not connected. Unlike units are never summed.'), detail: 'Labeled units Pending' },
      { label: 'Net churn', metric: pendingMetric('Billing churn and reporting denominator not available.'), detail: 'Period comparison Pending' },
      { label: 'New contacts', metric: sumMetrics(crm.map(c => c.contacts)), route: '/crm/contacts', detail: `${this.format(sumMetrics(crm.map(c => c.companies)))} new companies · tenant-deduplicated` },
      { label: 'Drips active', metric: this.totals().drips, route: '/mailing/automations', detail: 'Enrolled · open rate · click rate Pending' },
      { label: 'Subscribers', metric: pendingMetric('Deduplicated subscriber reporting across mailing lists is not available.'), route: '/mailing/lists', detail: 'Net additions · unsubscribe rate Pending' },
      { label: 'Visitors / day', metric: this.visitors(), route: '/analytics', detail: '7d average · readable sites · comparison Pending' },
    ];
  });
  readonly recentAudit = computed(() => Object.entries(this.auditPages()).flatMap(([businessId, page]) =>
    page.items.map(entry => ({ entry, business: this.businesses().find(b => b.id === businessId)! })))
    .sort((a, b) => Date.parse(b.entry.created_at) - Date.parse(a.entry.created_at)).slice(0, 3));
  readonly auditStatus = computed(() => {
    const pages = Object.values(this.auditPages());
    if (pages.length < this.active().length) return 'Loading authorized audit trails…';
    if (pages.some(p => p.state === 'denied')) return "Some audit trails are unavailable. You don't have access to do that.";
    if (pages.some(p => p.state === 'error')) return 'Some audit trails could not be loaded. Refresh to try again.';
    return this.recentAudit().length ? 'Latest authorized events' : 'No audit events in the visible businesses.';
  });

  constructor() {
    effect(onCleanup => {
      this.refreshKey();
      const businesses = this.active();
      this.work.set(Object.fromEntries(businesses.map(b => [b.id, emptyWork()])));
      this.auditPages.set({});
      const tenants = [...new Map(businesses.map(b => [b.tenant_root_id, b])).values()];
      this.crm.set(Object.fromEntries(tenants.map(b => [b.tenant_root_id, { contacts: loadingMetric(), companies: loadingMetric() }])));
      this.visitors.set(loadingMetric());
      const subscriptions = new Subscription();
      subscriptions.add(from(businesses).pipe(mergeMap(b => forkJoin({ work: this.api.work(b.id), audit: this.api.audit(b.id) }).pipe(
        tap(result => { this.work.update(all => ({ ...all, [b.id]: result.work })); this.auditPages.update(all => ({ ...all, [b.id]: result.audit })); }),
      ), 3)).subscribe());
      subscriptions.add(from(tenants).pipe(mergeMap(b => this.api.contacts(b.id).pipe(tap(result => {
        this.crm.update(all => ({ ...all, [b.tenant_root_id]: result }));
      })), 3)).subscribe());
      subscriptions.add(this.api.visitors().subscribe(metric => this.visitors.set(metric)));
      onCleanup(() => { subscriptions.unsubscribe(); this.auditRequest?.unsubscribe(); });
    });
  }

  rollup(ids: string[]): ForgeWork {
    const work = this.work();
    const values = ids.map(id => work[id] ?? emptyWork());
    return Object.fromEntries((['tickets', 'urgent', 'approvals', 'failed', 'spend', 'drips'] as WorkKey[])
      .map(key => [key, sumMetrics(values.map(value => value[key]))])) as ForgeWork;
  }
  heat(work: ForgeWork): 'hot' | 'warm' | 'cold' | 'unknown' {
    const h = work.tickets.observed + 2 * work.approvals.observed + 2 * work.failed.observed + 6 * work.urgent.observed;
    if (h >= 6) return 'hot';
    const complete = [work.tickets, work.approvals, work.failed, work.urgent].every(m => m.state === 'ready');
    if (h >= 2) return 'warm';
    return complete ? 'cold' : 'unknown';
  }
  glow(work: ForgeWork): number {
    return Math.min(1, .3 + (work.tickets.observed + 2 * work.approvals.observed + 2 * work.failed.observed + 6 * work.urgent.observed) / 12);
  }
  format(metric: ForgeMetric, money = false): string {
    if (metric.state === 'ready') return money ? '$' + ((metric.value ?? 0) / 100).toFixed(2)
      : (metric.value ?? 0).toLocaleString(undefined, { maximumFractionDigits: 1 });
    return { loading: 'Loading', pending: 'Pending', denied: 'No access', error: 'Error' }[metric.state];
  }
  open(business: Business, route?: string): void {
    const work = this.work()[business.id];
    this.current.set(business.id);
    void this.router.navigateByUrl(route ?? (work?.tickets.observed ? '/support' : work?.approvals.observed ? '/approvals' : work?.failed.observed ? '/code-review' : '/support'));
  }
  openTile(tile: FloorTile): void {
    const current = this.active().find(b => b.id === this.current.businessId()) ?? this.active()[0];
    if (current && tile.route) this.open(current, tile.route);
  }
  showAudit(business = this.active().find(b => b.id === this.current.businessId()) ?? this.active()[0]): void {
    if (!business) return;
    this.auditBusiness.set(business); this.current.set(business.id); this.fullAudit.set(null); this.loadAudit();
  }
  loadAudit(cursor?: string): void {
    const business = this.auditBusiness(); if (!business) return;
    this.auditRequest?.unsubscribe(); this.auditLoading.set(true);
    this.auditRequest = this.api.audit(business.id, 50, cursor).subscribe(page => {
      this.fullAudit.set(page); this.auditLoading.set(false);
    });
  }
  closeAudit(): void { this.auditRequest?.unsubscribe(); this.auditBusiness.set(null); this.auditLoading.set(false); }
  auditHot(entry: ForgeAuditEntry): boolean { return /failed|requested|error/.test(entry.action); }
}
