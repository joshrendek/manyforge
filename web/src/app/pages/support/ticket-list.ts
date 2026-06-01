import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { Business } from '../../core/tree';
import {
  Ticket,
  TicketListFilters,
  TicketPriority,
  TicketService,
  TicketStatus,
} from '../../core/ticket.service';

const STATUSES: TicketStatus[] = ['new', 'open', 'pending', 'solved', 'closed'];
const PRIORITIES: TicketPriority[] = ['low', 'normal', 'high', 'urgent'];

// Support ticket list. Mirrors dashboard.ts: signals for state, a centralised
// load helper, generic no-oracle error copy. The "current business" is chosen
// from the same /api/v1/businesses list the dashboard uses (there is no selected-
// business service); the chosen id scopes every ticket call and seeds the thread
// route. Status/priority filters and keyset "load more" drive the list.
@Component({
  selector: 'app-ticket-list',
  imports: [FormsModule, RouterLink, DatePipe],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Support</h1>
          <p class="sub">Inbound conversations for the selected business.</p>
        </div>
        <a class="linklike" routerLink="/dashboard" data-testid="back-to-dashboard"
          >Back to dashboard</a
        >
      </div>

      <div class="row" style="margin-top:6px">
        <div style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select
            id="biz-select"
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
        <div style="flex:1 1 160px">
          <label for="status-filter">Status</label>
          <select
            id="status-filter"
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
        <div style="flex:1 1 160px">
          <label for="priority-filter">Priority</label>
          <select
            id="priority-filter"
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
        <p class="empty" data-testid="no-business">
          Select a business to view its support tickets.
        </p>
      } @else if (loading()) {
        <p class="empty">Loading tickets…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p>We couldn't load these tickets.</p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else {
        <ul class="tree" data-testid="ticket-list">
          @for (t of tickets(); track t.id) {
            <li
              class="biz ticket"
              data-testid="ticket-row"
              [attr.data-ticket-id]="t.id"
              (click)="open(t)"
            >
              <div class="biz-main">
                <span class="name" data-testid="ticket-subject">{{
                  t.subject || '(no subject)'
                }}</span>
                <span class="badge" data-testid="ticket-status">{{ t.status }}</span>
                <span class="pill" data-testid="ticket-priority">{{ t.priority }}</span>
              </div>
              <div class="ticket-meta">
                <span class="requester" data-testid="ticket-requester">{{
                  t.requester.display_name || t.requester.email
                }}</span>
                <span class="count" data-testid="ticket-message-count"
                  >{{ t.message_count }} msg</span
                >
                @if (t.last_message_at) {
                  <span class="when">{{ t.last_message_at | date: 'short' }}</span>
                }
                @if (t.tags.length) {
                  <span class="tags" data-testid="ticket-tags">
                    @for (tag of t.tags; track tag) {
                      <span class="badge">{{ tag }}</span>
                    }
                  </span>
                }
              </div>
            </li>
          } @empty {
            <li class="empty" data-testid="ticket-empty">No tickets match these filters.</li>
          }
        </ul>

        @if (nextCursor()) {
          <button
            class="ghost compact"
            data-testid="load-more"
            [disabled]="busy()"
            (click)="loadMore()"
          >
            {{ busy() ? 'Loading…' : 'Load more' }}
          </button>
        }
      }

      @if (error()) {
        <p class="msg error" data-testid="list-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [
    `
      .ticket {
        cursor: pointer;
      }
      .ticket-meta {
        display: flex;
        align-items: center;
        gap: 12px;
        flex-wrap: wrap;
        color: var(--muted);
        font-size: 12.5px;
      }
      .ticket-meta .requester {
        color: var(--text);
        font-weight: 500;
      }
      .ticket-meta .tags {
        display: inline-flex;
        gap: 6px;
        flex-wrap: wrap;
      }
      [data-testid='load-more'] {
        margin-top: 16px;
      }
    `,
  ],
})
export class TicketListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(TicketService);
  private router = inject(Router);

  readonly statuses = STATUSES;
  readonly priorities = PRIORITIES;

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
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
    this.api.listTickets(this.businessId(), this.filters()).subscribe({
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
    this.api.listTickets(this.businessId(), { ...this.filters(), cursor }).subscribe({
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

  // No-oracle: 403/404 both map to a generic message (mirrors dashboard.ts).
  private describeError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not load the tickets. Please try again.';
  }
}
