import { CurrencyPipe, DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AccountingService, RunSummary } from '../../core/accounting.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { runStatusTone } from '../../ui/status';

@Component({
  selector: 'app-agent-runs',
  imports: [RouterLink, DatePipe, CurrencyPipe, PageHeader, StatusPill, EmptyState],
  template: `
    <div class="mf-card">
      <mf-page-header title="Agent runs" subtitle="Per-run token and cost breakdown.">
        <a routerLink="/accounting" data-testid="back-to-accounting" class="mf-btn mf-btn-ghost mf-btn-sm" actions>Back to accounting</a>
      </mf-page-header>

      @if (loading()) {
        <p style="color:var(--mf-text-muted)">Loading runs…</p>
      } @else if (loadFailed()) {
        <div style="color:var(--mf-text-muted)">
          <p>We couldn't load these runs.</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else {
        <div class="mf-table" data-testid="run-list">
          @for (r of runs(); track r.id) {
            <div class="mf-tr" data-testid="run-row" [attr.data-run-id]="r.id">
              <div style="display:flex;align-items:center;gap:10px;flex:1">
                <mf-status-pill [tone]="runStatusTone(r.status)" [label]="r.status" data-testid="run-status" />
                <span data-testid="run-cost">{{ r.cost_cents / 100 | currency }}</span>
              </div>
              <div style="display:flex;gap:12px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">
                <span data-testid="run-tokens">{{ r.tokens_in }} in / {{ r.tokens_out }} out</span>
                <span>{{ r.created_at | date: 'short' }}</span>
              </div>
            </div>
          } @empty {
            <mf-empty-state title="No runs" data-testid="run-empty">No runs in this window.</mf-empty-state>
          }
        </div>
        @if (nextCursor()) {
          <button class="mf-btn mf-btn-ghost" data-testid="load-more" [disabled]="busy()" (click)="loadMore()">
            {{ busy() ? 'Loading…' : 'Load more' }}
          </button>
        }
      }

      @if (error()) {
        <p class="mf-err" data-testid="list-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class AgentRunsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(AccountingService);

  readonly runStatusTone = runStatusTone;

  businessId = '';
  agentId = '';
  runs = signal<RunSummary[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  loadFailed = signal(false);
  busy = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.agentId = this.route.snapshot.paramMap.get('agentId') ?? '';
    this.reload();
  }

  reload(): void {
    if (!this.businessId || !this.agentId) return;
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.listRuns(this.businessId, this.agentId, { limit: 50 }).subscribe({
      next: (page) => {
        this.runs.set(page.items ?? []);
        this.nextCursor.set(page.next_cursor);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(e.status === 403 || e.status === 404 ? "You don't have access to do that." : 'Could not load runs. Please try again.');
      },
    });
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.busy()) return;
    this.busy.set(true);
    this.api.listRuns(this.businessId, this.agentId, { limit: 50, cursor }).subscribe({
      next: (page) => {
        this.runs.update((cur) => [...cur, ...(page.items ?? [])]);
        this.nextCursor.set(page.next_cursor);
        this.busy.set(false);
      },
      error: () => this.busy.set(false),
    });
  }
}
