import { HttpErrorResponse } from '@angular/common/http';
import { DatePipe } from '@angular/common';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { Agent, AgentsService } from '../../core/agents.service';
import { BusinessService } from '../../core/business.service';
import {
  CodeReview,
  CodeReviewService,
  CreateRepoConnectorBody,
  RepoConnector,
} from '../../core/code-review.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { runStatusTone } from '../../ui/status';

// Code Review page: manage GitHub repo connectors per business, trigger a PR review,
// and show the review history with live polling. While any review is pending/running,
// listReviews is polled every 3 s; polling stops when all rows are terminal (succeeded/
// failed) and is always cleared in ngOnDestroy to prevent leaks.
@Component({
  selector: 'app-code-review-list',
  imports: [DatePipe, FormsModule, PageHeader, EmptyState, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="code-review-page">
      <mf-page-header title="Code Review" subtitle="GitHub PR reviews powered by your agents">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="code-review-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <!-- Business selector -->
      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="cr-biz-select">Business</label>
          <select id="cr-biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Add connector' }}
          </button>
        </div>
      </div>

      <!-- Add-connector inline form -->
      @if (showAdd() && businessId()) {
        <div class="mf-card" style="margin:8px 0" data-testid="connector-add-form">
          <div class="mf-filters" style="flex-wrap:wrap;gap:8px">
            <div class="mf-field" style="flex:1 1 180px">
              <label for="add-display-name">Display name</label>
              <input id="add-display-name" class="mf-input" data-testid="connector-form-display-name"
                     [(ngModel)]="addForm.display_name" name="display_name" placeholder="My org/repo" />
            </div>
            <div class="mf-field" style="flex:1 1 180px">
              <label for="add-repo">Repo (owner/name)</label>
              <input id="add-repo" class="mf-input" data-testid="connector-form-repo"
                     [(ngModel)]="addForm.repo" name="repo" placeholder="acme/api" />
            </div>
            <div class="mf-field" style="flex:1 1 260px">
              <label for="add-base-url">Base URL</label>
              <input id="add-base-url" class="mf-input" data-testid="connector-form-base-url"
                     [(ngModel)]="addForm.base_url" name="base_url" />
            </div>
            <div class="mf-field" style="flex:1 1 200px">
              <label for="add-api-token">API token</label>
              <input id="add-api-token" class="mf-input" data-testid="connector-form-api-token"
                     [(ngModel)]="addForm.api_token" name="api_token" type="password" placeholder="ghp_…" />
            </div>
            <div class="mf-field" style="flex:0 0 auto;justify-content:flex-end;padding-top:24px">
              <label style="display:flex;align-items:center;gap:6px;cursor:pointer">
                <input type="checkbox" data-testid="connector-form-allow-private"
                       [(ngModel)]="addForm.allow_private_base_url" name="allow_private" />
                Allow private base URL
              </label>
            </div>
          </div>
          @if (addError()) {
            <p class="mf-err" data-testid="connector-add-error">{{ addError() }}</p>
          }
          <div style="display:flex;gap:8px;margin-top:8px">
            <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-form-submit"
                    (click)="createConnector()" [disabled]="adding()">
              {{ adding() ? 'Saving…' : 'Save connector' }}
            </button>
            <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-form-cancel"
                    (click)="cancelAdd()">Cancel</button>
          </div>
        </div>
      }

      <!-- Connectors table -->
      <h3 style="margin:16px 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
        Connectors
      </h3>
      <div class="mf-table" data-testid="connectors-table">
        <div class="mf-tr mf-th">
          <span style="flex:1">Name</span>
          <span style="flex:1">Repo</span>
          <span style="width:80px">Status</span>
          <span style="width:220px"></span>
        </div>
        @for (c of connectors(); track c.id) {
          <div class="mf-tr" data-testid="connector-row" [attr.data-connector-id]="c.id">
            <span style="flex:1">{{ c.display_name }}</span>
            <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.repo }}</span>
            <span style="width:80px;font-size:var(--mf-fs-sm)">{{ c.status }}</span>
            <span style="width:220px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (confirmDeleteConnectorId() === c.id) {
                <span class="mf-err" data-testid="connector-delete-confirm"
                      style="font-size:var(--mf-fs-xs);align-self:center">
                  Delete {{ c.display_name }}? This also deletes its reviews.
                </span>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-delete-no"
                        (click)="confirmDeleteConnectorId.set('')">Cancel</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete-yes"
                        (click)="deleteConnector(c)">Delete</button>
              } @else {
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete"
                        (click)="confirmDeleteConnectorId.set(c.id)">Delete</button>
              }
            </span>
          </div>
        }
        @if (!connectors().length && businessId() && !loading()) {
          <mf-empty-state title="No connectors yet" data-testid="connectors-empty">
            Add one to start reviewing PRs.
          </mf-empty-state>
        }
      </div>

      @if (connectorsError()) {
        <p class="mf-err" data-testid="connectors-error">{{ connectorsError() }}</p>
      }

      <!-- Review a PR section -->
      <h3 style="margin:24px 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
        Review a PR
      </h3>
      <div style="display:flex;flex-wrap:wrap;gap:12px;align-items:flex-end" data-testid="trigger-form">
        <div class="mf-field" style="flex:1 1 180px">
          <label for="cr-agent-select">Agent</label>
          <select id="cr-agent-select" class="mf-select" data-testid="cr-agent"
                  [(ngModel)]="triggerForm.agent_id" name="cr_agent">
            <option value="" disabled>Choose agent…</option>
            @for (a of agents(); track a.id) {
              <option [value]="a.id">{{ a.name }}</option>
            }
          </select>
        </div>
        <div class="mf-field" style="flex:1 1 180px">
          <label for="cr-connector-select">Connector</label>
          <select id="cr-connector-select" class="mf-select" data-testid="cr-connector"
                  [(ngModel)]="triggerForm.repo_connector_id" name="cr_connector">
            <option value="" disabled>Choose connector…</option>
            @for (c of connectors(); track c.id) {
              <option [value]="c.id">{{ c.display_name }}</option>
            }
          </select>
        </div>
        <div class="mf-field" style="flex:0 1 120px">
          <label for="cr-pr-num">PR number</label>
          <input id="cr-pr-num" class="mf-input" data-testid="cr-pr-number" type="number" min="1"
                 [(ngModel)]="triggerForm.pr_number" name="cr_pr_number" placeholder="123" />
        </div>
        <div style="padding-bottom:1px">
          <button class="mf-btn mf-btn-primary" data-testid="cr-submit"
                  (click)="triggerReview()"
                  [disabled]="!triggerForm.agent_id || !triggerForm.repo_connector_id || !triggerForm.pr_number || triggering()">
            {{ triggering() ? 'Queueing…' : 'Review PR' }}
          </button>
        </div>
      </div>
      @if (triggerError()) {
        <p class="mf-err" data-testid="trigger-error">{{ triggerError() }}</p>
      }
      @if (triggerSuccess()) {
        <p class="mf-success" style="color:var(--mf-success,#22c55e);margin-top:8px" data-testid="trigger-success">
          Review queued
        </p>
      }

      <!-- History: live-polling review list. Polling runs every 3 s while any row is
           pending/running and stops automatically when all rows are terminal. -->
      @if (reviews().length) {
        <h3 style="margin:24px 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
          Recent Reviews
        </h3>
        <div class="mf-table" data-testid="reviews-table">
          <div class="mf-tr mf-th">
            <span style="width:56px">PR #</span>
            <span style="width:96px">Status</span>
            <span style="flex:1">Repo</span>
            <span style="flex:1">Model</span>
            <span style="width:64px">Findings</span>
            <span style="width:72px">Cost</span>
            <span style="width:132px">Created</span>
          </div>
          @for (r of reviews(); track r.id) {
            <div class="mf-tr" data-testid="review-row" [attr.data-review-id]="r.id"
                 style="cursor:pointer" (click)="openDetail(r)">
              <span style="width:56px">#{{ r.pr_number }}</span>
              <span style="width:96px">
                <mf-status-pill [tone]="reviewTone(r.status)" [label]="r.status" />
                @if (r.status === 'running' && r.progress?.phase) {
                  <span data-testid="review-phase" style="display:block;color:var(--mf-text-muted);font-size:var(--mf-fs-xs);margin-top:2px">{{ r.progress?.phase }}</span>
                }
              </span>
              <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm);overflow:hidden;text-overflow:ellipsis;white-space:nowrap"
                    data-testid="review-repo" [attr.title]="r.repo || null">{{ r.repo || '—' }}</span>
              <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm);overflow:hidden;text-overflow:ellipsis;white-space:nowrap"
                    data-testid="review-model"
                    [title]="r.model === 'panel' ? 'Multi-model panel — see per-dimension models in the review detail' : r.model"
                    >{{ r.model === 'panel' ? 'Panel' : (r.model || '—') }}</span>
              <span style="width:64px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ r.findings_count }}</span>
              <span style="width:72px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)"
                    data-testid="review-cost">{{ formatCost(r.cost_cents) }}</span>
              <span style="width:132px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ r.created_at | date:'short' }}</span>
              <span style="width:76px;text-align:right" role="cell" (click)="$event.stopPropagation()">
                @if (isTerminal(r.status)) {
                  <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" [attr.data-testid]="'review-retry-' + r.id"
                          [disabled]="retrying().has(r.id)" (click)="retry(r)"
                          [attr.aria-label]="'Retry review of PR #' + r.pr_number">Retry</button>
                }
              </span>
            </div>
          }
        </div>
      }

      @if (error()) {
        <p class="mf-err" data-testid="code-review-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class CodeReviewListComponent implements OnInit, OnDestroy {
  private bizApi = inject(BusinessService);
  private api = inject(CodeReviewService);
  private agentsSvc = inject(AgentsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);
  private router = inject(Router);

  readonly reviewTone = runStatusTone;

  // Polling: fires every 3 s while any review is pending/running.
  private pollTimer: ReturnType<typeof setInterval> | undefined;

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  connectors = signal<RepoConnector[]>([]);
  reviews = signal<CodeReview[]>([]);
  retrying = signal<Set<string>>(new Set());
  agents = signal<Agent[]>([]);
  loading = signal(false);
  // pending counts the in-flight init loads (connectors/reviews/agents) so the
  // spinner only clears once ALL have settled — see reload()/settle().
  private pending = signal(0);
  error = signal('');
  connectorsError = signal('');
  showAdd = signal(false);
  adding = signal(false);
  addError = signal('');
  confirmDeleteConnectorId = signal<string>('');

  triggering = signal(false);
  triggerError = signal('');
  triggerSuccess = signal(false);

  addForm: CreateRepoConnectorBody = {
    type: 'github',
    display_name: '',
    repo: '',
    base_url: 'https://api.github.com',
    api_token: '',
    allow_private_base_url: false,
  };

  triggerForm = {
    agent_id: '',
    repo_connector_id: '',
    pr_number: null as number | null,
  };

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  ngOnDestroy(): void {
    this.stopPolling();
  }

  // Returns true when at least one review is in a non-terminal state.
  private hasNonTerminal(): boolean {
    return this.reviews().some((r) => r.status === 'pending' || r.status === 'running');
  }

  private startPolling(): void {
    if (this.pollTimer !== undefined) return; // already running
    this.pollTimer = setInterval(() => this.pollReviews(), 3000);
  }

  private stopPolling(): void {
    if (this.pollTimer !== undefined) {
      clearInterval(this.pollTimer);
      this.pollTimer = undefined;
    }
  }

  private pollReviews(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.api.listReviews(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.reviews.set(r.items ?? []);
        if (!this.hasNonTerminal()) this.stopPolling();
      },
      error: () => {
        // On poll error, keep polling — transient network issue should not stop updates.
      },
    });
  }

  // Formats LLM cost cents as USD; 0 (not yet priced / unpriceable) shows as a dash.
  formatCost(cents: number): string {
    return cents ? '$' + (cents / 100).toFixed(2) : '—';
  }

  // Navigate to the detail page for a review row click.
  openDetail(r: CodeReview): void {
    void this.router.navigate(['/code-review', this.businessId(), r.id]);
  }

  // isTerminal is true for a review that has finished (so it can be retried); pending/running
  // reviews are still in flight.
  isTerminal(status: string): boolean {
    return status !== 'pending' && status !== 'running';
  }

  // retry re-runs a review: POSTs a forced re-run (bypasses the same-head dedup), prepends the
  // new pending review, and resumes polling so it updates live.
  retry(r: CodeReview): void {
    if (this.retrying().has(r.id)) return;
    this.retrying.update((s) => new Set(s).add(r.id));
    this.api.retry(this.businessId(), r.id).subscribe({
      next: (fresh) => {
        this.clearRetrying(r.id);
        this.reviews.set([fresh, ...this.reviews()]);
        this.startPolling();
        this.toast.success('Review re-queued');
      },
      error: (e: HttpErrorResponse) => {
        this.clearRetrying(r.id);
        this.toast.error(e.status === 404 ? 'Review not found' : 'Retry failed');
      },
    });
  }

  private clearRetrying(id: string): void {
    this.retrying.update((s) => {
      const n = new Set(s);
      n.delete(id);
      return n;
    });
  }

  selectBusiness(id: string): void {
    this.stopPolling();
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteConnectorId.set('');
    this.showAdd.set(false);
    this.addError.set('');
    this.triggerError.set('');
    this.triggerSuccess.set(false);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();

    // The three loads are independent — one failing must not cancel the others
    // (so no forkJoin). Track them with a pending-counter so the spinner only
    // clears once ALL three have settled, regardless of order or per-call error.
    this.loading.set(true);
    this.pending.set(3);

    this.api.listConnectors(biz).subscribe({
      next: (r) => {
        if (this.businessId() === biz) {
          this.connectors.set(r.items ?? []);
          this.connectorsError.set('');
        }
        this.settle();
      },
      error: () => {
        if (this.businessId() === biz) {
          this.connectors.set([]);
          this.connectorsError.set('Could not load connectors');
        }
        this.settle();
      },
    });

    this.api.listReviews(biz).subscribe({
      next: (r) => {
        if (this.businessId() === biz) {
          this.reviews.set(r.items ?? []);
          if (this.hasNonTerminal()) this.startPolling();
        }
        this.settle();
      },
      error: () => {
        if (this.businessId() === biz) this.reviews.set([]);
        this.settle();
      },
    });

    this.agentsSvc.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() === biz) this.agents.set(r.items ?? []);
        this.settle();
      },
      error: () => {
        if (this.businessId() === biz) this.agents.set([]);
        this.settle();
      },
    });
  }

  // settle decrements the pending-load counter and clears the spinner once all
  // in-flight loads have finished (counter reaches 0).
  private settle(): void {
    const remaining = this.pending() - 1;
    this.pending.set(remaining);
    if (remaining <= 0) this.loading.set(false);
  }

  cancelAdd(): void {
    this.showAdd.set(false);
    this.addError.set('');
    this.resetAddForm();
  }

  private resetAddForm(): void {
    this.addForm = {
      type: 'github',
      display_name: '',
      repo: '',
      base_url: 'https://api.github.com',
      api_token: '',
      allow_private_base_url: false,
    };
  }

  createConnector(): void {
    if (!this.businessId()) return;
    this.adding.set(true);
    this.addError.set('');
    this.api.createConnector(this.businessId(), this.addForm).subscribe({
      next: () => {
        this.adding.set(false);
        this.showAdd.set(false);
        this.resetAddForm();
        this.toast.success('Connector added');
        this.reloadConnectors();
      },
      error: (e: HttpErrorResponse) => {
        this.adding.set(false);
        this.addError.set(
          e.status === 400 ? (e.error?.error ?? 'Invalid connector — check egress allowlist') :
          e.status === 404 ? 'Not found' :
          'Could not save connector',
        );
      },
    });
  }

  deleteConnector(c: RepoConnector): void {
    this.api.deleteConnector(this.businessId(), c.id).subscribe({
      next: () => {
        this.connectors.update((xs) => xs.filter((x) => x.id !== c.id));
        this.confirmDeleteConnectorId.set('');
        this.toast.success('Connector deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteConnectorId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }

  triggerReview(): void {
    if (!this.businessId() || !this.triggerForm.agent_id || !this.triggerForm.repo_connector_id || !this.triggerForm.pr_number) return;
    this.triggering.set(true);
    this.triggerError.set('');
    this.triggerSuccess.set(false);
    this.api.trigger(this.businessId(), {
      agent_id: this.triggerForm.agent_id,
      repo_connector_id: this.triggerForm.repo_connector_id,
      pr_number: this.triggerForm.pr_number,
    }).subscribe({
      next: (resp) => {
        this.triggering.set(false);
        this.triggerSuccess.set(true);
        // Insert an optimistic pending row so the user sees feedback immediately.
        // Task 4 replaces this with a polling history table.
        const optimistic: CodeReview = {
          id: resp.id,
          status: resp.status,
          summary: '',
          review_url: resp.review_url,
          pr_number: this.triggerForm.pr_number!,
          model: this.agents().find((a) => a.id === this.triggerForm.agent_id)?.model ?? '',
          findings: [],
          findings_count: 0,
          cost_cents: 0,
          created_at: new Date().toISOString(),
          posted_at: null,
        };
        this.reviews.update((xs) => [optimistic, ...xs]);
        this.triggerForm = { agent_id: '', repo_connector_id: '', pr_number: null };
        // Start polling immediately so the new pending row refreshes to its real state.
        this.startPolling();
      },
      error: (e: HttpErrorResponse) => {
        this.triggering.set(false);
        this.triggerError.set(
          e.status === 400 ? (e.error?.error ?? 'Request blocked — check egress allowlist') :
          e.status === 404 ? 'Agent or connector not found' :
          'Could not queue review',
        );
      },
    });
  }

  // reloadConnectors refreshes only the connectors table (after an add). It toggles
  // the spinner for UX consistency with the init/error paths in reload().
  private reloadConnectors(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.listConnectors(biz).subscribe({
      next: (r) => {
        if (this.businessId() === biz) this.connectors.set(r.items ?? []);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
      },
    });
  }
}
