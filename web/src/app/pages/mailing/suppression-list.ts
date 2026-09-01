import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { MailingService, MailingSuppression } from '../../core/mailing.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { Tone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-suppression-list',
  standalone: true,
  imports: [DatePipe, FormsModule, RouterLink, EmptyState, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card page" data-testid="mailing-suppression-page">
      <mf-page-header
        title="Suppression list"
        subtitle="Prevent campaigns from sending to blocked recipients"
      >
        <a
          actions
          class="mf-btn mf-btn-ghost mf-btn-sm"
          routerLink="/mailing/campaigns"
          data-testid="suppression-campaigns-link"
          >Campaigns</a
        >
        <a
          actions
          class="mf-btn mf-btn-ghost mf-btn-sm"
          routerLink="/mailing/lists"
          data-testid="suppression-lists-link"
          >Lists</a
        >
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field business-field">
          <label for="suppression-business">Business</label>
          <select
            id="suppression-business"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
          >
            <option value="" disabled>Choose a business…</option>
            @for (business of businesses(); track business.id) {
              <option [value]="business.id">{{ business.name }}</option>
            }
          </select>
        </div>
        @if (loading()) {
          <span class="loading"><mf-spinner /> Loading suppressions…</span>
        }
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="suppression-new" (ngSubmit)="create()">
          <div class="mf-field grow">
            <label for="suppression-email">Email address</label>
            <input
              id="suppression-email"
              class="mf-input"
              type="email"
              name="email"
              data-testid="suppression-email"
              [(ngModel)]="newEmail"
              placeholder="recipient@example.com"
              required
            />
          </div>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="suppression-create"
            [disabled]="!newEmail.trim() || creating()"
          >
            {{ creating() ? 'Adding…' : 'Suppress address' }}
          </button>
        </form>
        <p class="mf-hint manual-hint">
          Addresses added here are marked as manual suppressions. Bounces, complaints, and
          unsubscribes are added automatically.
        </p>
      }

      <div class="mf-table" data-testid="suppression-table">
        <div class="mf-tr mf-th">
          <span class="email">Email</span><span>Reason</span><span>Source</span><span>Added</span
          ><span class="actions-col">Actions</span>
        </div>
        @for (suppression of items(); track suppression.id) {
          <div class="mf-tr" data-testid="suppression-row">
            <span class="email mf-ellipsis" [title]="suppression.email">{{
              suppression.email
            }}</span>
            <span>
              <mf-status-pill
                [tone]="reasonTone(suppression.reason)"
                [label]="suppression.reason"
              />
            </span>
            <span>{{ suppression.source }}</span>
            <span>{{ suppression.created_at | date: 'medium' }}</span>
            <span class="actions-col">
              @if (pendingDelete() === suppression.id) {
                <button
                  type="button"
                  class="mf-btn mf-btn-danger mf-btn-sm"
                  data-testid="suppression-delete-confirm"
                  [disabled]="deleting() === suppression.id"
                  (click)="remove(suppression)"
                >
                  {{ deleting() === suppression.id ? 'Removing…' : 'Confirm' }}
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="suppression-delete-cancel"
                  [disabled]="deleting() === suppression.id"
                  (click)="pendingDelete.set(null)"
                >
                  Cancel
                </button>
              } @else {
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="suppression-delete"
                  (click)="pendingDelete.set(suppression.id)"
                >
                  Remove
                </button>
              }
            </span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No suppressed addresses" data-testid="suppression-empty">
            This business has no tenant-level email suppressions.
          </mf-empty-state>
        }
      </div>

      @if (nextCursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm load-more"
          data-testid="suppression-load-more"
          [disabled]="loading()"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
      @if (error()) {
        <p class="mf-err" data-testid="suppression-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .page {
        max-width: 1120px;
      }
      .business-field,
      .grow,
      .email {
        flex: 2;
        min-width: 0;
      }
      .mf-tr > span:not(.email) {
        flex: 1;
      }
      .loading,
      .actions-col {
        display: flex;
        align-items: center;
        gap: 8px;
      }
      .loading {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-filters .mf-btn {
        align-self: end;
        min-height: 36px;
      }
      .manual-hint {
        margin: -4px 0 16px;
      }
      .load-more {
        margin-top: 12px;
      }
    `,
  ],
})
export class MailingSuppressionListComponent implements OnInit {
  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);
  private loadSeq = 0;

  businesses = signal<Business[]>([]);
  businessId = signal('');
  items = signal<MailingSuppression[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  creating = signal(false);
  pendingDelete = signal<string | null>(null);
  deleting = signal<string | null>(null);
  error = signal('');
  newEmail = '';

  reasonTone(reason: MailingSuppression['reason']): Tone {
    if (reason === 'complaint') return 'danger';
    if (reason === 'bounce') return 'warn';
    return 'neutral';
  }

  ngOnInit(): void {
    this.businessesApi.list().subscribe({
      next: (page) => {
        const items = page.items ?? [];
        this.businesses.set(items);
        const businessId = this.current.businessId() ?? items[0]?.id;
        if (businessId) this.selectBusiness(businessId);
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(businessId: string): void {
    this.businessId.set(businessId);
    this.current.set(businessId);
    this.items.set([]);
    this.nextCursor.set(null);
    this.pendingDelete.set(null);
    this.loading.set(false);
    this.load();
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (cursor) this.load(cursor);
  }

  create(): void {
    const email = this.newEmail.trim();
    if (!email || this.creating()) return;
    this.creating.set(true);
    this.mailing.createSuppression(this.businessId(), email, 'manual').subscribe({
      next: (suppression) => {
        this.items.update((items) => [suppression, ...items]);
        this.newEmail = '';
        this.creating.set(false);
        this.toast.success('Address suppressed');
      },
      error: (response: HttpErrorResponse) => {
        this.creating.set(false);
        this.toast.error(
          response.status === 409
            ? 'That address is already suppressed'
            : 'Could not suppress address',
        );
      },
    });
  }

  remove(suppression: MailingSuppression): void {
    if (this.deleting()) return;
    this.deleting.set(suppression.id);
    this.mailing.deleteSuppression(this.businessId(), suppression.id).subscribe({
      next: () => {
        this.items.update((items) => items.filter((item) => item.id !== suppression.id));
        this.deleting.set(null);
        this.pendingDelete.set(null);
        this.toast.success('Suppression removed');
      },
      error: () => {
        this.deleting.set(null);
        this.toast.error('Could not remove suppression');
      },
    });
  }

  private load(cursor?: string): void {
    const businessId = this.businessId();
    if (!businessId || this.loading()) return;
    const seq = ++this.loadSeq;
    this.loading.set(true);
    this.mailing.listSuppressions(businessId, cursor, 50).subscribe({
      next: (page) => {
        if (seq !== this.loadSeq || businessId !== this.businessId()) return;
        this.items.update((items) =>
          cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
        );
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
        this.error.set('');
      },
      error: () => {
        if (seq !== this.loadSeq || businessId !== this.businessId()) return;
        this.loading.set(false);
        this.error.set('Could not load suppressions');
      },
    });
  }
}
