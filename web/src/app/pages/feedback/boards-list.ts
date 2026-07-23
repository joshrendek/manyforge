import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Board, FeedbackService } from '../../core/feedback.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

// Feedback boards list: per-business boards reached through a business-scoped URL. Business
// selection mirrors the CRM contacts page (same /api/v1/businesses list, persisted via
// CurrentBusinessService). An inline new-board form posts a name (+ optional public flag)
// then reloads.
@Component({
  selector: 'app-feedback-boards-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="feedback-boards-page">
      <mf-page-header
        title="Feedback boards"
        subtitle="Collect feature requests and feedback per business"
      >
        @if (loading()) {
          <span class="mf-loading-row" data-testid="boards-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="fb-biz-select">Business</label>
          <select
            id="fb-biz-select"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
            name="biz"
          >
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="board-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:1 1 260px">
            <label for="fb-new-name">New board name</label>
            <input
              id="fb-new-name"
              class="mf-input"
              type="text"
              name="newName"
              placeholder="e.g. Mobile App"
              [(ngModel)]="newName"
            />
          </div>
          <div class="mf-field" style="align-self:flex-end">
            <label class="mf-check" for="fb-new-public">
              <input
                id="fb-new-public"
                type="checkbox"
                name="newPublic"
                data-testid="board-public-toggle"
                [(ngModel)]="newPublic"
              />
              Public (SDK/portal)
            </label>
          </div>
          <div style="display:flex;align-items:flex-end">
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="board-create"
              [disabled]="!newName.trim() || creating()"
            >
              {{ creating() ? 'Creating…' : 'Create board' }}
            </button>
          </div>
        </form>
      }

      <div class="mf-table" data-testid="boards-list">
        <div class="mf-tr mf-th">
          <span style="flex:2">Name</span>
          <span style="flex:2">Slug</span>
          <span style="flex:1">Visibility</span>
        </div>
        @for (b of items(); track b.id) {
          <div class="mf-tr" data-testid="board-row" [attr.data-board-id]="b.id">
            <span style="flex:2" data-testid="board-name-cell">
              <a [routerLink]="['/feedback', businessId(), b.id]">{{ b.name }}</a>
            </span>
            <span style="flex:2" data-testid="board-slug-cell">{{ b.slug }}</span>
            <span style="flex:1" data-testid="board-visibility-cell">
              @if (b.is_public) {
                <mf-status-pill tone="success" label="Public" />
              } @else {
                <mf-status-pill tone="neutral" label="Private" />
              }
            </span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No boards yet" data-testid="boards-empty">
            Create one with the form above.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="boards-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .mf-check {
        display: inline-flex;
        align-items: center;
        gap: 8px;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
        cursor: pointer;
      }
    `,
  ],
})
export class FeedbackBoardsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private feedback = inject(FeedbackService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Board[]>([]);
  loading = signal(false);
  error = signal('');
  newName = '';
  newPublic = false;
  creating = signal(false);

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

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.feedback.listBoards(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load boards');
        this.loading.set(false);
      },
    });
  }

  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    this.creating.set(true);
    this.feedback.createBoard(this.businessId(), { name, is_public: this.newPublic }).subscribe({
      next: () => {
        this.newName = '';
        this.newPublic = false;
        this.creating.set(false);
        this.toast.success('Board created');
        this.reload();
      },
      error: (e: HttpErrorResponse) => {
        this.creating.set(false);
        this.toast.error(
          e.status === 409 ? 'A board with that name already exists' : 'Could not create board',
        );
      },
    });
  }
}
