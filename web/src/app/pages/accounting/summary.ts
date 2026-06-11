import { CurrencyPipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { AccountingService, AccountingSummary, WindowName } from '../../core/accounting.service';
import { Business } from '../../core/tree';
import { PageHeader } from '../../ui/page-header/page-header';
import { EmptyState } from '../../ui/empty-state/empty-state';

const WINDOWS: WindowName[] = ['this_month', 'last_month', 'last_30_days'];

@Component({
  selector: 'app-accounting-summary',
  imports: [FormsModule, RouterLink, CurrencyPipe, PageHeader, EmptyState],
  template: `
    <div class="mf-card">
      <mf-page-header title="Accounting" subtitle="Token and cost usage by agent for the selected business.">
        <a routerLink="/dashboard" data-testid="back-to-dashboard" class="mf-btn mf-btn-ghost mf-btn-sm" actions>Back to dashboard</a>
      </mf-page-header>

      <div style="display:flex;gap:12px;flex-wrap:wrap;margin-bottom:16px">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select id="biz-select" class="mf-select" data-testid="business-select" [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div class="mf-field" style="flex:1 1 160px">
          <label for="window-select">Window</label>
          <select id="window-select" class="mf-select" data-testid="window-select" [ngModel]="window()" (ngModelChange)="setWindow($event)">
            @for (w of windows; track w) {
              <option [value]="w">{{ w }}</option>
            }
          </select>
        </div>
      </div>

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
        <div style="display:flex;gap:10px;flex-wrap:wrap;margin-bottom:16px" data-testid="totals">
          <div class="mf-card" style="flex:1 1 120px;display:flex;flex-direction:column;gap:4px;padding:10px 14px">
            <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Total cost</span>
            <strong data-testid="total-cost">{{ s.totals.cost_cents / 100 | currency }}</strong>
          </div>
          <div class="mf-card" style="flex:1 1 120px;display:flex;flex-direction:column;gap:4px;padding:10px 14px">
            <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Tokens in</span>
            <strong data-testid="total-in">{{ s.totals.tokens_in }}</strong>
          </div>
          <div class="mf-card" style="flex:1 1 120px;display:flex;flex-direction:column;gap:4px;padding:10px 14px">
            <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Tokens out</span>
            <strong data-testid="total-out">{{ s.totals.tokens_out }}</strong>
          </div>
          <div class="mf-card" style="flex:1 1 120px;display:flex;flex-direction:column;gap:4px;padding:10px 14px">
            <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Runs</span>
            <strong data-testid="total-runs">{{ s.totals.run_count }}</strong>
          </div>
        </div>

        <div class="mf-table" data-testid="agent-list">
          @for (a of s.agents; track a.agent_id) {
            <div class="mf-tr mf-clickable" data-testid="agent-row" [attr.data-agent-id]="a.agent_id" (click)="openAgent(a.agent_id)" style="cursor:pointer">
              <div style="display:flex;align-items:center;gap:8px;flex:1">
                <span data-testid="agent-name">{{ a.name }}</span>
                <span class="mf-pill mf-pill-neutral" data-testid="agent-cost">{{ a.cost_cents / 100 | currency }}</span>
                @if (a.budget_pct != null) {
                  <span class="mf-pill mf-pill-accent" data-testid="agent-budget-pct">{{ a.budget_pct }}% of budget</span>
                }
              </div>
              <div style="display:flex;gap:12px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">
                <span data-testid="agent-runs">{{ a.run_count }} runs</span>
                <span>{{ a.tokens_in }} in / {{ a.tokens_out }} out</span>
              </div>
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
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        if (items.length && !this.businessId()) {
          this.businessId.set(items[0].id);
          this.reload();
        }
      },
      error: () => this.loadFailed.set(true),
    });
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.reload();
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
    this.api.getSummary(this.businessId(), this.window()).subscribe({
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
