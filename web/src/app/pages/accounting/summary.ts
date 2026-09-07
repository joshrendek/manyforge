import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject, takeUntil } from 'rxjs';
import { CurrencyPipe, DecimalPipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal, DestroyRef, effect, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { AccountingService, AccountingSummary, WindowName } from '../../core/accounting.service';
import { Business } from '../../core/tree';
import { PageHeader } from '../../ui/page-header/page-header';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { StatTiles } from '../../ui/stat-tiles/stat-tiles';

const WINDOWS: WindowName[] = ['this_month', 'last_month', 'last_30_days'];

@Component({
  selector: 'app-accounting-summary',
  imports: [FormsModule, RouterLink, CurrencyPipe, DecimalPipe, PageHeader, EmptyState, StatTiles],
  template: `
    <div class="mf-card">
      <mf-page-header [eyebrow]="businessName()" title="Accounting" subtitle="Token and cost usage by agent for the selected business.">
        <a routerLink="/dashboard" data-testid="back-to-dashboard" class="mf-btn mf-btn-ghost mf-btn-sm" actions>Back to dashboard</a>
      </mf-page-header>

      <div class="mf-filters">
        
        <div class="mf-field" style="flex:1 1 160px">
          <label for="window-select">Window</label>
          <select id="window-select" class="mf-select" data-testid="window-select" [ngModel]="window()" (ngModelChange)="setWindow($event)">
            @for (w of windows; track w) {
              <option [value]="w">{{ w }}</option>
            }
          </select>
        </div>
      </div>

      @if (window() !== 'this_month') {
        <p style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm);margin:-4px 0 16px" data-testid="budget-hint">
          Budget % is shown only for the current month.
        </p>
      }

      @if (!businessId()) {
        <p style="color:var(--mf-text-muted)" data-testid="no-business">Select a business to view usage.</p>
      } @else if (loading()) {
        <p style="color:var(--mf-text-muted)">Loading usage…</p>
      } @else if (loadFailed()) {
        <div style="color:var(--mf-text-muted)">
          <p>We couldn't load usage.</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else if (summary(); as s) {
        <div data-testid="totals">
          <mf-stat-tiles [tiles]="[
            { label: 'Total cost', value: (s.totals.cost_cents / 100 | currency) ?? '—', testid: 'total-cost' },
            { label: 'Tokens in', value: (s.totals.tokens_in | number) ?? '—', testid: 'total-in' },
            { label: 'Tokens out', value: (s.totals.tokens_out | number) ?? '—', testid: 'total-out' },
            { label: 'Runs', value: (s.totals.run_count | number) ?? '—', testid: 'total-runs' }
          ]" />
        </div>

        <div class="mf-table" data-testid="agent-list">
          <div class="mf-tr mf-th">
            <span style="flex:2">Agent</span>
            <span class="mf-td-num" style="width:80px">Runs</span>
            <span class="mf-td-num" style="width:120px">Tokens in</span>
            <span class="mf-td-num" style="width:120px">Tokens out</span>
          </div>
          @for (a of s.agents; track a.agent_id) {
            <div class="mf-tr mf-clickable" data-testid="agent-row" [attr.data-agent-id]="a.agent_id" (click)="openAgent(a.agent_id)" style="cursor:pointer">
              <div style="display:flex;align-items:center;gap:8px;flex:2">
                <span data-testid="agent-name">{{ a.name }}</span>
                <span class="mf-pill mf-pill-neutral" data-testid="agent-cost">{{ a.cost_cents / 100 | currency }}</span>
                @if (a.budget_pct != null) {
                  <span class="mf-pill mf-pill-accent" data-testid="agent-budget-pct">{{ a.budget_pct }}% of budget</span>
                }
              </div>
              <span class="mf-td-num" style="width:80px" data-testid="agent-runs">{{ a.run_count | number }}</span>
              <span class="mf-td-num" style="width:120px">{{ a.tokens_in | number }}</span>
              <span class="mf-td-num" style="width:120px">{{ a.tokens_out | number }}</span>
            </div>
          } @empty {
            <mf-empty-state title="No agents" data-testid="agent-empty">No agents for this business.</mf-empty-state>
          }
        </div>
      }

      @if (error()) {
        <p class="mf-err" data-testid="list-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class AccountingSummaryComponent implements OnInit {
  private readonly destroyRef = inject(DestroyRef);
  private readonly businessChanged = new Subject<void>();
  private readonly followBusiness = effect(() => {
    const id = this.current.businessId() ?? '';
    untracked(() => {
      if (id !== this.businessId()) this.selectBusiness(id);
    });
  });
  businessName(): string {
    return this.businesses().find((business) => business.id === this.businessId())?.name ?? '';
  }

  private current = inject(CurrentBusinessService);
  private bizApi = inject(BusinessService);
  private api = inject(AccountingService);
  private router = inject(Router);

  readonly windows = WINDOWS;
  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  window = signal<WindowName>('this_month');
  summary = signal<AccountingSummary | null>(null);
  loading = signal(false);
  loadFailed = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.bizApi.list().pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        if (items.length && !this.businessId()) {
          this.selectBusiness(this.current.businessId() ?? items[0].id);
        }
      },
      error: () => this.loadFailed.set(true),
    });
  }

  selectBusiness(id: string): void {
    if (id === this.businessId()) return;
    this.businessChanged.next();
    this.summary.set(null);
    this.loading.set(false);
    this.loadFailed.set(false);
    this.error.set('');
    this.businessId.set(id);
    if (id) this.current.set(id);
    if (id) this.reload();
  }

  setWindow(w: WindowName): void {
    this.window.set(w);
    this.reload();
  }

  openAgent(agentId: string): void {
    this.router.navigate(['/accounting', this.businessId(), agentId]);
  }

  reload(): void {
    if (!this.businessId()) return;
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.getSummary(this.businessId(), this.window()).pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (s) => {
        this.summary.set(s);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(e.status === 403 || e.status === 404 ? "You don't have access to do that." : 'Could not load usage. Please try again.');
      },
    });
  }
}
