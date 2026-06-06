import { CurrencyPipe, DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AccountingService, RunSummary } from '../../core/accounting.service';

@Component({
  selector: 'app-agent-runs',
  imports: [RouterLink, DatePipe, CurrencyPipe],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Agent runs</h1>
          <p class="sub">Per-run token and cost breakdown.</p>
        </div>
        <a class="linklike" routerLink="/accounting" data-testid="back-to-accounting">Back to accounting</a>
      </div>

      @if (loading()) {
        <p class="empty">Loading runs…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p>We couldn't load these runs.</p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else {
        <ul class="tree" data-testid="run-list">
          @for (r of runs(); track r.id) {
            <li class="biz" data-testid="run-row" [attr.data-run-id]="r.id">
              <div class="biz-main">
                <span class="badge" data-testid="run-status">{{ r.status }}</span>
                <span class="name" data-testid="run-cost">{{ r.cost_cents / 100 | currency }}</span>
              </div>
              <div class="ticket-meta">
                <span data-testid="run-tokens">{{ r.tokens_in }} in / {{ r.tokens_out }} out</span>
                <span>{{ r.created_at | date: 'short' }}</span>
              </div>
            </li>
          } @empty {
            <li class="empty" data-testid="run-empty">No runs in this window.</li>
          }
        </ul>
        @if (nextCursor()) {
          <button class="ghost compact" data-testid="load-more" [disabled]="busy()" (click)="loadMore()">
            {{ busy() ? 'Loading…' : 'Load more' }}
          </button>
        }
      }

      @if (error()) {
        <p class="msg error" data-testid="list-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [`.ticket-meta { display: flex; gap: 12px; color: var(--muted); font-size: 12.5px; }`],
})
export class AgentRunsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(AccountingService);

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
