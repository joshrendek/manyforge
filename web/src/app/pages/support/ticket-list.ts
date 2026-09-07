import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject, takeUntil } from 'rxjs';
import { Component, OnInit, computed, inject, signal, DestroyRef, effect, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import {
  Ticket,
  TicketListFilters,
  TicketPriority,
  TicketService,
  TicketStatus,
} from '../../core/ticket.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { Spinner } from '../../ui/spinner/spinner';
import { ticketStatusTone, ticketPriorityTone } from '../../ui/status';

const STATUSES: TicketStatus[] = ['new', 'open', 'pending', 'solved', 'closed'];
const PRIORITIES: TicketPriority[] = ['low', 'normal', 'high', 'urgent'];

// Support tickets follow the global business context; filters and pagination remain page-local.
@Component({
  selector: 'app-ticket-list',
  imports: [FormsModule, RouterLink, DatePipe, PageHeader, StatusPill, EmptyState, Spinner],
  template: `
    <div class="mf-card">
      <mf-page-header [eyebrow]="businessName()" title="Support" subtitle="Inbound conversations for the selected business.">
        <ng-container actions>
          @if (businessId()) {
            <a
              class="mf-btn mf-btn-ghost mf-btn-sm"
              [routerLink]="['/support', businessId(), 'settings', 'inbox']"
              data-testid="inbox-settings-link"
              >Inbox settings</a
            >
          }
          <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/dashboard" data-testid="back-to-dashboard"
            >Back to dashboard</a
          >
        </ng-container>
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:2 1 240px">
          <label for="ticket-search">Search</label>
          <input id="ticket-search" class="mf-input" placeholder="Subject contains…" [ngModel]="search()" (ngModelChange)="search.set($event)" />
          @if (search() && nextCursor()) {
            <span class="mf-field-hint">Searching loaded tickets. Load more to search earlier conversations.</span>
          }
        </div>
        
        <div class="mf-field" style="flex:1 1 160px">
          <label for="status-filter">Status</label>
          <select
            id="status-filter"
            class="mf-select"
            data-testid="status-filter"
            [ngModel]="status()"
            (ngModelChange)="setStatus($event)"
          >
            <option value="">All statuses</option>
            @for (s of statuses; track s) {
              <option [value]="s">{{ s }}</option>
            }
          </select>
        </div>
        <div class="mf-field" style="flex:1 1 160px">
          <label for="priority-filter">Priority</label>
          <select
            id="priority-filter"
            class="mf-select"
            data-testid="priority-filter"
            [ngModel]="priority()"
            (ngModelChange)="setPriority($event)"
          >
            <option value="">All priorities</option>
            @for (p of priorities; track p) {
              <option [value]="p">{{ p }}</option>
            }
          </select>
        </div>
      </div>

      @if (!businessId()) {
        <p class="mf-empty-inline" data-testid="no-business">
          Select a business to view its support tickets.
        </p>
      } @else if (loading()) {
        <div class="mf-loading-row">
          <mf-spinner />
          <span>Loading tickets…</span>
        </div>
      } @else if (loadFailed()) {
        <div class="mf-empty-inline">
          <p>We couldn't load these tickets.</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else {
        @if (visibleTickets().length) {
          <div class="mf-table" data-testid="ticket-list">
            <div class="mf-tr mf-th">
              <span style="flex:3">Subject</span>
              <span style="width:90px">Status</span>
              <span style="width:90px">Priority</span>
              <span style="width:150px">Requester</span>
              <span class="mf-td-num" style="width:52px">Msgs</span>
              <span style="flex:1">Tags</span>
              <span style="width:140px">Last message</span>
            </div>
            @for (t of visibleTickets(); track t.id) {
              <div
                class="mf-tr mf-clickable"
                data-testid="ticket-row"
                [attr.data-ticket-id]="t.id"
                (click)="open(t)"
              >
                <span class="mf-tr-name" style="flex:3;font-weight:500" data-testid="ticket-subject">{{
                  t.subject || '(no subject)'
                }}</span>
                <span style="width:90px">
                  <mf-status-pill
                    [tone]="ticketStatusTone(t.status)"
                    [label]="t.status"
                    data-testid="ticket-status"
                  />
                </span>
                <span style="width:90px">
                  <mf-status-pill
                    [tone]="ticketPriorityTone(t.priority)"
                    [label]="t.priority"
                    data-testid="ticket-priority"
                  />
                </span>
                <span style="width:150px" data-testid="ticket-requester">{{
                  t.requester.display_name || t.requester.email
                }}</span>
                <span class="mf-td-num" style="width:52px" data-testid="ticket-message-count"
                  >{{ t.message_count }}</span
                >
                <span style="flex:1">
                  @if (t.tags.length) {
                    <span class="mf-tags" data-testid="ticket-tags">
                      @for (tag of t.tags; track tag) {
                        <span class="mf-pill mf-pill-neutral">{{ tag }}</span>
                      }
                    </span>
                  }
                </span>
                <span class="mf-td-data" style="width:140px">
                  @if (t.last_message_at) {
                    {{ t.last_message_at | date: 'short' }}
                  }
                </span>
              </div>
            }
          </div>
        } @else {
          <mf-empty-state title="No tickets" data-testid="ticket-empty">
            The anvil is quiet — nothing matches these filters.
            <button action class="mf-btn mf-btn-ghost mf-btn-sm" (click)="clearFilters()">Clear filters</button>
          </mf-empty-state>
        }

        @if (nextCursor()) {
          <button
            class="mf-btn mf-btn-ghost"
            data-testid="load-more"
            [disabled]="busy()"
            (click)="loadMore()"
          >
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
export class TicketListComponent implements OnInit {
  private readonly destroyRef = inject(DestroyRef);
  private readonly businessChanged = new Subject<void>();
  private readonly followBusiness = effect(() => {
    const id = this.currentBiz.businessId() ?? '';
    untracked(() => {
      if (id !== this.businessId()) this.selectBusiness(id);
    });
  });
  businessName(): string {
    return this.businesses().find((business) => business.id === this.businessId())?.name ?? '';
  }

  private bizApi = inject(BusinessService);
  private api = inject(TicketService);
  private router = inject(Router);
  private currentBiz = inject(CurrentBusinessService);

  readonly statuses = STATUSES;
  readonly priorities = PRIORITIES;

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  search = signal('');
  readonly visibleTickets = computed(() => {
    const query = this.search().trim().toLocaleLowerCase();
    return query ? this.tickets().filter((ticket) => ticket.subject.toLocaleLowerCase().includes(query)) : this.tickets();
  });
  clearFilters(): void {
    this.search.set('');
    this.status.set('');
    this.priority.set('');
    this.reload();
  }
  status = signal<TicketStatus | ''>('');
  priority = signal<TicketPriority | ''>('');

  tickets = signal<Ticket[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  loadFailed = signal(false);
  busy = signal(false);
  error = signal('');

  readonly filters = computed<TicketListFilters>(() => {
    const f: TicketListFilters = {};
    if (this.status()) f.status = this.status() as TicketStatus;
    if (this.priority()) f.priority = this.priority() as TicketPriority;
    return f;
  });

  ngOnInit(): void {
    // The current business is chosen from the same list the dashboard renders;
    // we default to the first one so the page is useful on first load.
    this.bizApi.list().pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        if (items.length && !this.businessId()) {
          // Prefer the shared current business (set on other pages, e.g. approvals) when it
          // is one the caller can see; else default to the first. Seed the shared service
          // either way so the approvals nav badge tracks the business shown here (crm).
          const shared = this.currentBiz.businessId();
          const initial = items.some((b) => b.id === shared) ? (shared as string) : items[0].id;
          this.selectBusiness(initial);
        }
      },
      error: () => this.loadFailed.set(true),
    });
  }

  selectBusiness(id: string): void {
    if (id === this.businessId()) return;
    this.businessChanged.next();
    this.tickets.set([]);
    this.nextCursor.set(null);
    this.loading.set(false);
    this.loadFailed.set(false);
    this.busy.set(false);
    this.error.set('');
    this.businessId.set(id);
    if (id) this.currentBiz.set(id); // keep the approvals nav badge in sync with the viewed business (crm)
    if (id) this.reload();
  }

  setStatus(s: TicketStatus | ''): void {
    this.status.set(s);
    this.reload();
  }

  setPriority(p: TicketPriority | ''): void {
    this.priority.set(p);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.listTickets(this.businessId(), this.filters()).pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (page) => {
        this.tickets.set(page.items ?? []);
        this.nextCursor.set(page.next_cursor);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeError(e));
      },
    });
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    this.api.listTickets(this.businessId(), { ...this.filters(), cursor }).pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (page) => {
        this.tickets.update((cur) => [...cur, ...(page.items ?? [])]);
        this.nextCursor.set(page.next_cursor);
        this.busy.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.busy.set(false);
        this.error.set(this.describeError(e));
      },
    });
  }

  open(t: Ticket): void {
    void this.router.navigate(['/support', this.businessId(), t.id]);
  }

  // Template helpers — delegate to pure status functions so the template stays clean.
  readonly ticketStatusTone = ticketStatusTone;
  readonly ticketPriorityTone = ticketPriorityTone;

  // No-oracle: 403/404 both map to a generic message (mirrors dashboard.ts).
  private describeError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not load the tickets. Please try again.';
  }
}
