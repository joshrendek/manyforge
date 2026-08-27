import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { MailingList, MailingService } from '../../core/mailing.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-lists-list',
  standalone: true,
  imports: [FormsModule, RouterLink, EmptyState, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="mailing-lists-page">
      <mf-page-header title="Mailing lists" subtitle="Collect and manage opted-in subscribers">
        <a
          routerLink="/mailing/templates"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="mailing-templates-link"
          actions
          >Templates</a
        >
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field business-field">
          <label for="mailing-business">Business</label>
          <select
            id="mailing-business"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
          >
            <option value="" disabled>Choose a business…</option>
            @for (business of businesses(); track business.id) {
              <option [value]="business.id">
                {{ business.is_tenant_root ? business.name + ' (master)' : business.name }}
              </option>
            }
          </select>
        </div>
        @if (loading()) {
          <span class="loading"><mf-spinner /> Loading lists…</span>
        }
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="mailing-list-new" (ngSubmit)="create()">
          <div class="mf-field grow">
            <label for="mailing-list-name">New list name</label>
            <input
              id="mailing-list-name"
              class="mf-input"
              name="name"
              data-testid="mailing-list-name"
              [(ngModel)]="newName"
              placeholder="Product updates"
            />
          </div>
          <label class="mf-check">
            <input
              type="checkbox"
              name="doubleOptIn"
              data-testid="mailing-list-double-opt-in"
              [(ngModel)]="newDoubleOptIn"
            />
            Double opt-in
          </label>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="mailing-list-create"
            [disabled]="!newName.trim() || creating()"
          >
            {{ creating() ? 'Creating…' : 'Create list' }}
          </button>
        </form>
      }

      <div class="mf-table" data-testid="mailing-lists-table">
        <div class="mf-tr mf-th">
          <span class="wide">Name</span><span>Slug</span><span>Status</span><span>Opt-in</span>
        </div>
        @for (list of items(); track list.id) {
          <div class="mf-tr" data-testid="mailing-list-row">
            <span class="wide"
              ><a
                [routerLink]="['/mailing', businessId(), 'lists', list.id]"
                data-testid="mailing-list-open"
                >{{ list.name }}</a
              ></span
            >
            <span>{{ list.slug }}</span>
            <span
              ><mf-status-pill
                [tone]="list.status === 'active' ? 'success' : 'neutral'"
                [label]="list.status"
            /></span>
            <span>{{ list.double_opt_in ? 'Double' : 'Single' }}</span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No mailing lists yet" data-testid="mailing-lists-empty"
            >Create a list above to start collecting subscribers.</mf-empty-state
          >
        }
      </div>
      @if (nextCursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm load-more"
          data-testid="mailing-lists-load-more"
          [disabled]="loading()"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
      @if (error()) {
        <p class="mf-err" data-testid="mailing-lists-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .business-field {
        flex: 1 1 240px;
      }
      .grow,
      .wide {
        flex: 2;
      }
      .mf-tr > span:not(.wide) {
        flex: 1;
      }
      .mf-check,
      .loading {
        display: flex;
        align-items: center;
        gap: 8px;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
      }
      .mf-check,
      .mf-filters .mf-btn {
        align-self: end;
        min-height: 36px;
      }
      .load-more {
        margin-top: 12px;
      }
    `,
  ],
})
export class MailingListsListComponent implements OnInit {
  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal('');
  items = signal<MailingList[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  creating = signal(false);
  error = signal('');
  newName = '';
  newDoubleOptIn = true;

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
    this.reload();
  }

  reload(): void {
    this.items.set([]);
    this.nextCursor.set(null);
    this.load();
  }
  loadMore(): void {
    if (this.nextCursor()) this.load(this.nextCursor()!);
  }

  private load(cursor?: string): void {
    const businessId = this.businessId();
    if (!businessId || this.loading()) return;
    this.loading.set(true);
    this.mailing.listLists(businessId, cursor).subscribe({
      next: (page) => {
        if (businessId !== this.businessId()) return;
        this.items.update((items) =>
          cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
        );
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
        this.error.set('');
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load mailing lists');
      },
    });
  }

  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    this.creating.set(true);
    this.mailing
      .createList(this.businessId(), { name, double_opt_in: this.newDoubleOptIn })
      .subscribe({
        next: () => {
          this.newName = '';
          this.creating.set(false);
          this.toast.success('Mailing list created');
          this.reload();
        },
        error: (error: HttpErrorResponse) => {
          this.creating.set(false);
          this.toast.error(
            error.status === 409
              ? 'A list with that name already exists'
              : 'Could not create mailing list',
          );
        },
      });
  }
}
