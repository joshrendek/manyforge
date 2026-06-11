import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApprovalItem, ApprovalsService } from '../../core/approvals.service';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { effectClassLabel, effectClassTone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

// Approvals queue: a human approves/denies queued agent actions for the selected
// business. Business selection mirrors ticket-list (same /api/v1/businesses list);
// the chosen id is persisted via CurrentBusinessService so the nav badge has a
// current business after first visit. A 20s interval keeps the list fresh; it is
// cleared in ngOnDestroy so no timer leaks. Decisions optimistically remove the
// row on success (no reload); a 409 means someone already decided, so we refresh.
@Component({
  selector: 'app-approvals-queue',
  imports: [FormsModule, DatePipe, PageHeader, StatusPill, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="approvals-page">
      <mf-page-header title="Approvals" [subtitle]="items().length + ' pending'">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="approvals-loading" actions>
            <mf-spinner />
          </span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select
            id="biz-select"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
          >
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
      </div>

      <div class="mf-table" data-testid="approvals-list">
        <div class="mf-tr mf-th">
          <span style="width:110px">Effect</span>
          <span style="flex:1">Summary</span>
          <span style="width:150px">Tool</span>
          <span style="width:90px">Run</span>
          <span style="width:80px">Expires</span>
          <span style="width:150px"></span>
        </div>
        @for (it of items(); track it.id) {
          <div class="mf-tr" data-testid="approval-row" [attr.data-approval-id]="it.id">
            <span data-testid="approval-effect" style="width:110px">
              <mf-status-pill [tone]="effectClassTone(it.effect_class)" [label]="effectClassLabel(it.effect_class)" />
            </span>
            <span style="flex:1" data-testid="approval-summary">{{ it.summary }}</span>
            <span
              data-testid="approval-tool"
              style="width:150px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)"
              >{{ it.tool }}</span
            >
            <span style="width:90px;color:var(--mf-text-faint);font-size:var(--mf-fs-sm)">{{
              it.agent_run_id.slice(0, 8)
            }}</span>
            <span data-testid="approval-expires" style="width:80px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{
              it.expires_at | date: 'short'
            }}</span>
            <span style="width:150px;display:flex;gap:8px;justify-content:flex-end">
              <button
                class="mf-btn mf-btn-ghost mf-btn-sm"
                data-testid="approval-deny"
                (click)="deny(it)"
              >
                Deny
              </button>
              <button
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="approval-approve"
                (click)="approve(it)"
              >
                Approve
              </button>
            </span>
          </div>
        }
        @if (!items().length) {
          <mf-empty-state title="No pending approvals" data-testid="approvals-empty">
            Nothing awaiting review.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="approvals-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class ApprovalsQueueComponent implements OnInit, OnDestroy {
  private bizApi = inject(BusinessService);
  private api = inject(ApprovalsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  readonly effectClassTone = effectClassTone;
  readonly effectClassLabel = effectClassLabel;

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<ApprovalItem[]>([]);
  loading = signal(false);
  error = signal('');

  private timer: ReturnType<typeof setInterval> | undefined;

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          // Persist so the nav badge (Task 8) has a current business after first visit.
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
    // Keep the queue fresh while it's open; cleared in ngOnDestroy.
    this.timer = setInterval(() => this.reload(), 20000);
  }

  ngOnDestroy(): void {
    clearInterval(this.timer);
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId(); // capture: a poll/in-flight load for B must not clobber a newer A
    this.loading.set(true);
    this.api.listPending(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return; // a newer business was selected — drop stale response
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: (_e: HttpErrorResponse) => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load approvals');
        this.loading.set(false);
      },
    });
  }

  approve(it: ApprovalItem): void {
    this.api.approve(this.businessId(), it.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== it.id));
        // Keep the shared badge count in sync with the optimistic removal (clamp at 0).
        this.api.pendingCount.update((n) => Math.max(0, n - 1));
        this.error.set('');
        this.toast.success('Approved');
      },
      error: (e: HttpErrorResponse) => this.handleDecisionError(e),
    });
  }

  deny(it: ApprovalItem): void {
    this.api.deny(this.businessId(), it.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== it.id));
        // Keep the shared badge count in sync with the optimistic removal (clamp at 0).
        this.api.pendingCount.update((n) => Math.max(0, n - 1));
        this.error.set('');
        this.toast.success('Denied');
      },
      error: (e: HttpErrorResponse) => this.handleDecisionError(e),
    });
  }

  private handleDecisionError(e: HttpErrorResponse): void {
    if (e.status === 409) {
      this.toast.error('Already decided — refreshing');
      this.reload();
    } else {
      this.toast.error('Action failed');
    }
  }
}
