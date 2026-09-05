import { HttpErrorResponse } from '@angular/common/http';
import { Component, ElementRef, Input, OnInit, ViewChild, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  Automation,
  Enrollment,
  EnrollmentStatus,
  AutomationsService,
} from '../../../core/automations.service';
import { MailingService, MailingSubscriber } from '../../../core/mailing.service';
import { EmptyState } from '../../../ui/empty-state/empty-state';
import { Spinner } from '../../../ui/spinner/spinner';
import { StatusPill } from '../../../ui/status-pill/status-pill';
import { enrollmentStatusTone } from '../../../ui/status';
import { ToastService } from '../../../ui/toast/toast.service';

@Component({
  selector: 'app-enrollments-tab',
  standalone: true,
  imports: [FormsModule, EmptyState, Spinner, StatusPill],
  template: `
    <section class="tab-panel" data-testid="enrollments-tab">
      <div class="toolbar">
        <div class="mf-field">
          <label for="enrollment-status-filter">Status</label>
          <select
            id="enrollment-status-filter"
            class="mf-input"
            data-testid="enrollment-status-filter"
            [ngModel]="statusFilter"
            (ngModelChange)="setStatusFilter($event)"
          >
            <option value="">All</option>
            <option value="active">active</option>
            <option value="completed">completed</option>
            <option value="exited">exited</option>
            <option value="errored">errored</option>
          </select>
        </div>
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="enrollment-filter-clear"
          (click)="clearFilter()"
        >
          Clear
        </button>
        <span class="spacer"></span>
        <button
          type="button"
          class="mf-btn mf-btn-secondary mf-btn-sm"
          data-testid="enrollment-enroll"
          [disabled]="!canEnroll()"
          (click)="openDialog()"
        >
          {{ enrolling() ? 'Enrolling…' : 'Enroll subscriber' }}
        </button>
      </div>
      @if (loading()) {
        <div class="loading" data-testid="enrollments-loading">
          <mf-spinner /> Loading enrollments…
        </div>
      }
      @if (error()) {
        <p class="mf-err" data-testid="enrollments-error">{{ error() }}</p>
      }
      <div class="mf-table" data-testid="enrollments-table" role="table" aria-label="Automation enrollments">
        <div class="mf-tr mf-th" role="row">
          <span class="sub-col" role="columnheader">Subscriber</span><span role="columnheader">Status</span><span role="columnheader">Current node</span
          ><span role="columnheader">Next run</span><span role="columnheader">Enrolled</span><span role="columnheader">Detail</span><span role="columnheader">Actions</span>
        </div>
        @for (enrollment of enrollments(); track enrollment.id) {
          <div class="mf-tr" data-testid="enrollment-row">
            <span class="sub-col"
              >@if (subscriber(enrollment.subscriber_id); as sub) {
                <strong data-testid="enrollment-row-subscriber">{{ sub.email }}</strong
                ><small>{{ sub.first_name }} {{ sub.last_name }}</small>
              } @else {
                <span class="mono" data-testid="enrollment-row-subscriber">{{ shortId(enrollment.subscriber_id) }}</span>
              }
            </span>
            <span
              ><mf-status-pill
                [tone]="enrollmentTone(enrollment.status)"
                [label]="enrollment.status"
              /></span
            >
            <span class="mono">{{ enrollment.current_node_id || '—' }}</span>
            <span>{{ formatTime(enrollment.wake_at) }}</span>
            <span>{{ formatTime(enrollment.enrolled_at) }}</span>
            <span class="detail" [title]="detailLabel(enrollment)">{{ detailLabel(enrollment) }}</span>
            <span>
              @if (enrollment.status === 'active') {
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="enrollment-exit"
                  [disabled]="exiting() !== ''"
                  (click)="exitEnrollment(enrollment)"
                >
                  {{ exiting() === enrollment.id ? 'Exiting…' : 'Exit' }}
                </button>
              }
            </span>
          </div>
        }
        @if (!enrollments().length && !loading()) {
          <mf-empty-state title="No enrollments" data-testid="enrollments-empty"
            >Enroll a subscriber or wait for the trigger to fire.</mf-empty-state
          >
        }
      </div>
      @if (cursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm more"
          data-testid="enrollments-load-more"
          [disabled]="loading()"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
    </section>
    @if (dialogOpen()) {
      <div class="dialog-backdrop" data-testid="enroll-dialog-backdrop" role="dialog" aria-modal="true" aria-labelledby="enroll-dialog-title" (click)="closeDialog()" (keydown)="onDialogKeydown($event)">
        <div class="mf-card dialog" #dialogCard (click)="$event.stopPropagation()">
          <h3 id="enroll-dialog-title">Enroll a subscriber</h3>
          <p class="hint" data-testid="enroll-dialog-hint">
            The subscriber must be active on the trigger list.
            @if (automation && automation.allow_reenroll) { Re-enrollment of existing subscribers is allowed. }
          </p>
          <div class="mf-field">
            <label for="enroll-search">Search by email or name</label>
            <input
              #dialogSearch
              id="enroll-search"
              type="search"
              class="mf-input"
              data-testid="enroll-search"
              placeholder="Type at least 2 characters"
              (input)="scheduleSearch($event)"
            />
          </div>
          @if (searching()) {
            <div class="loading" data-testid="enroll-search-loading"><mf-spinner /></div>
          }
          @for (candidate of candidates(); track candidate.id) {
            <div class="candidate" data-testid="enroll-candidate">
              <span class="sub-col"
                ><strong>{{ candidate.email }}</strong
                ><small>{{ candidate.first_name }} {{ candidate.last_name }}</small></span
              >
              <button
                type="button"
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="enroll-candidate-select"
                [disabled]="enrolling()"
                (click)="enrollCandidate(candidate)"
              >
                Enroll
              </button>
            </div>
          }
          @if (!searching() && searched() && !candidates().length) {
            <p class="hint" data-testid="enroll-search-empty">No active subscribers match.</p>
          }
          <div class="dialog-actions">
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm"
              data-testid="enroll-dialog-close"
              (click)="closeDialog()"
            >
              Cancel
            </button>
          </div>
        </div>
      </div>
    }
  `,
  styles: [`
    :host{display:block}.tab-panel{padding:16px 20px;display:flex;flex-direction:column;gap:12px}.toolbar{display:flex;align-items:flex-end;gap:10px}.spacer{flex:1}.loading{display:flex;align-items:center;gap:8px;padding:14px;color:var(--mf-text-muted)}.more{align-self:flex-start}.sub-col{display:flex;flex-direction:column;min-width:160px}.sub-col small{color:var(--mf-text-muted)}.mono{font-family:var(--mf-font-mono,monospace);font-size:var(--mf-fs-xs)}.detail{color:var(--mf-text-muted);max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
    .dialog-backdrop{position:fixed;inset:0;z-index:50;display:flex;align-items:center;justify-content:center;background:rgba(0,0,0,.4)}.dialog{width:min(520px,92vw);padding:18px;display:flex;flex-direction:column;gap:10px}.dialog h3{margin:0;font-size:var(--mf-fs-lg)}.hint{margin:0;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)}.candidate{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:8px 10px;border:1px solid var(--mf-border);border-radius:var(--mf-radius-sm)}.dialog-actions{display:flex;justify-content:flex-end}
  `],
})
export class EnrollmentsTabComponent implements OnInit {
  @Input({ required: true }) businessId = '';
  @Input({ required: true }) automationId = '';
  @Input() automation: Automation | null = null;
  @Input() triggerListId: string | null = null;

  @ViewChild('dialogCard') dialogCard?: ElementRef<HTMLElement>;
  @ViewChild('dialogSearch') dialogSearch?: ElementRef<HTMLInputElement>;
  private readonly automations = inject(AutomationsService);
  private readonly mailing = inject(MailingService);
  private readonly toast = inject(ToastService);

  readonly enrollments = signal<Enrollment[]>([]);
  readonly loading = signal(false);
  readonly error = signal('');
  readonly cursor = signal<string | null>(null);
  readonly enrolling = signal(false);
  readonly exiting = signal('');
  readonly dialogOpen = signal(false);
  readonly searching = signal(false);
  readonly candidates = signal<MailingSubscriber[]>([]);
  readonly searched = signal(false);
  readonly enrollmentTone = enrollmentStatusTone;
  statusFilter: EnrollmentStatus | '' = '';
  private subscribersById = signal<Map<string, MailingSubscriber>>(new Map());
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private searchSeq = 0;
  private lastFocused: HTMLElement | null = null;

  readonly canEnroll = () =>
    this.automation?.status === 'active' && !!this.triggerListId && !this.enrolling();

  ngOnInit(): void {
    this.reload();
  }

  reload(): void {
    this.cursor.set(null);
    this.enrollments.set([]);
    this.error.set('');
    this.loadPage();
  }

  loadMore(): void {
    if (this.cursor()) this.loadPage(this.cursor()!);
  }

  setStatusFilter(value: EnrollmentStatus | ''): void {
    this.statusFilter = value;
    this.reload();
  }

  clearFilter(): void {
    if (this.statusFilter === '') return;
    this.statusFilter = '';
    this.reload();
  }

  openDialog(): void {
    if (!this.canEnroll()) return;
    this.lastFocused = (document.activeElement as HTMLElement) ?? null;
    this.dialogOpen.set(true);
    this.candidates.set([]);
    this.searched.set(false);
    setTimeout(() => this.dialogSearch?.nativeElement.focus());
  }

  closeDialog(): void {
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.searchTimer = null;
    this.dialogOpen.set(false);
    const restore = this.lastFocused;
    this.lastFocused = null;
    restore?.focus();
  }

  onDialogKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.stopPropagation();
      this.closeDialog();
      return;
    }
    if (event.key !== 'Tab') return;
    const card = this.dialogCard?.nativeElement;
    if (!card) return;
    const focusables = Array.from(
      card.querySelectorAll<HTMLElement>('button, input, select, [href], [tabindex]:not([tabindex="-1"])'),
    ).filter((el) => !el.hasAttribute('disabled'));
    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (event.shiftKey && (active === first || active === card)) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && active === last) {
      event.preventDefault();
      first.focus();
    }
  }

  scheduleSearch(event: Event): void {
    const term = (event.target as HTMLInputElement).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    const value = term.trim();
    if (value.length < 2) {
      this.candidates.set([]);
      this.searching.set(false);
      this.searched.set(false);
      return;
    }
    this.searching.set(true);
    this.searchTimer = setTimeout(() => this.runSearch(value), 300);
  }

  private runSearch(term: string): void {
    const listId = this.triggerListId;
    const seq = ++this.searchSeq;
    if (!listId) {
      this.searching.set(false);
      return;
    }
    this.mailing
      .listSubscribers(this.businessId, listId, { q: term, status: 'active', limit: 25 })
      .subscribe({
        next: (page) => {
          if (seq !== this.searchSeq) return;
          this.candidates.set(page.items ?? []);
          this.searching.set(false);
          this.searched.set(true);
        },
        error: () => {
          if (seq !== this.searchSeq) return;
          this.searching.set(false);
          this.searched.set(true);
          this.toast.error('Could not search subscribers');
        },
      });
  }

  enrollCandidate(candidate: MailingSubscriber): void {
    if (this.enrolling() || !this.automation || !this.automationId) return;
    this.enrolling.set(true);
    this.automations.enroll(this.businessId, this.automationId, candidate.id).subscribe({
      next: () => {
        this.enrolling.set(false);
        this.closeDialog();
        this.toast.success('Subscriber enrolled');
        this.reload();
      },
      error: (response: HttpErrorResponse) => {
        this.enrolling.set(false);
        if (response.status === 409) this.toast.error('This subscriber is already enrolled');
        else if (response.status === 404) this.toast.error('Only active subscribers on the trigger list can be enrolled');
        else this.toast.error('Could not enroll subscriber');
        if (response.status === 409 || response.status === 404) this.reload();
      },
    });
  }

  exitEnrollment(enrollment: Enrollment): void {
    if (this.exiting() || !this.automationId) return;
    if (!globalThis.confirm('Remove this subscriber from the automation? Their journey stops.')) return;
    this.exiting.set(enrollment.id);
    this.automations.exitEnrollment(this.businessId, this.automationId, enrollment.id).subscribe({
      next: () => {
        this.exiting.set('');
        this.toast.success('Subscriber removed from automation');
        this.reload();
      },
      error: (response: HttpErrorResponse) => {
        this.exiting.set('');
        this.toast.error('Could not remove subscriber from automation');
        if (response.status === 409) this.reload();
      },
    });
  }
  private mapSubscribers(): void {
    const listId = this.triggerListId;
    const ids = [...new Set(this.enrollments().map((item) => item.subscriber_id))];
    if (!listId || !ids.length) return;
    const missing = new Set(ids.filter((id) => !this.subscribersById().has(id)));
    if (!missing.size) return;
    this.fetchSubscribersPage(listId, undefined, missing, new Map(), 0);
  }

  private fetchSubscribersPage(
    listId: string,
    cursor: string | undefined,
    missing: Set<string>,
    found: Map<string, MailingSubscriber>,
    depth: number,
  ): void {
    if (depth >= 5 || !missing.size) {
      this.commitSubscribers(found);
      return;
    }
    this.mailing.listSubscribers(this.businessId, listId, { limit: 100, cursor }).subscribe({
      next: (page) => {
        for (const item of page.items ?? []) {
          if (missing.has(item.id)) {
            missing.delete(item.id);
            found.set(item.id, item);
          }
        }
        if (page.next_cursor && missing.size) {
          this.fetchSubscribersPage(listId, page.next_cursor, missing, found, depth + 1);
        } else {
          this.commitSubscribers(found);
        }
      },
      error: () => this.commitSubscribers(found),
    });
  }

  private commitSubscribers(found: Map<string, MailingSubscriber>): void {
    if (!found.size) return;
    this.subscribersById.update((current) => {
      const next = new Map(current);
      for (const [id, value] of found) if (!next.has(id)) next.set(id, value);
      return next;
    });
  }

  subscriber(id: string): MailingSubscriber | undefined {
    return this.subscribersById().get(id);
  }

  shortId(id: string): string {
    return id.slice(0, 8) + '…';
  }

  detailLabel(enrollment: Enrollment): string {
    if (enrollment.status === 'errored' && enrollment.last_error) return enrollment.last_error;
    if (enrollment.status === 'exited' && enrollment.exit_reason) return enrollment.exit_reason;
    return '—';
  }

  formatTime(value: string | null): string {
    return value ? new Date(value).toLocaleString() : '—';
  }

  private loadPage(cursor?: string): void {
    if (this.loading()) return;
    this.loading.set(true);
    this.automations
      .listEnrollments(this.businessId, this.automationId, { status: this.statusFilter, cursor })
      .subscribe({
        next: (page) => {
          this.enrollments.update((items) => (cursor ? [...items, ...(page.items ?? [])] : (page.items ?? [])));
          this.cursor.set(page.next_cursor ?? null);
          this.loading.set(false);
          this.mapSubscribers();
        },
        error: () => {
          this.loading.set(false);
          this.error.set('Could not load enrollments');
        },
      });
  }

}
