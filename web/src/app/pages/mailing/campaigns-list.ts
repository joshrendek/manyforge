import { DatePipe } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { MailingCampaign, MailingList, MailingService } from '../../core/mailing.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { mailingCampaignStatusTone } from '../../ui/status';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-campaigns-list',
  standalone: true,
  imports: [DatePipe, FormsModule, RouterLink, EmptyState, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="mailing-campaigns-page">
      <mf-page-header title="Campaigns" subtitle="Write, schedule, and send email broadcasts">
        <a
          routerLink="/mailing/lists"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="campaigns-lists-link"
          actions
          >Lists</a
        >
        <a
          routerLink="/mailing/templates"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="campaigns-templates-link"
          actions
          >Templates</a
        >
        <a
          routerLink="/mailing/sending"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="campaigns-sending-link"
          actions
          >Sending profile</a
        >
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field grow">
          <label for="campaign-business">Business</label>
          <select
            id="campaign-business"
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
          <span class="loading"><mf-spinner /> Loading campaigns…</span>
        }
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="mailing-campaign-new" (ngSubmit)="create()">
          <div class="mf-field grow">
            <label for="campaign-name">Campaign name</label>
            <input
              id="campaign-name"
              class="mf-input"
              name="campaignName"
              data-testid="mailing-campaign-name"
              [(ngModel)]="newName"
              placeholder="September product update"
            />
          </div>
          <div class="mf-field grow">
            <label for="campaign-list">Mailing list</label>
            <select
              id="campaign-list"
              class="mf-select"
              name="campaignList"
              data-testid="mailing-campaign-list"
              [(ngModel)]="newListId"
            >
              <option value="" disabled>Choose a list…</option>
              @for (list of lists(); track list.id) {
                <option [value]="list.id">{{ list.name }}</option>
              }
            </select>
          </div>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="mailing-campaign-create"
            [disabled]="!newName.trim() || !newListId || creating()"
          >
            {{ creating() ? 'Creating…' : 'Create campaign' }}
          </button>
        </form>
        @if (!lists().length && !loadingLists()) {
          <p class="mf-hint no-lists" data-testid="mailing-campaign-no-lists">
            Create an active <a routerLink="/mailing/lists">mailing list</a> before starting a
            campaign.
          </p>
        }
      }

      <div class="mf-table" data-testid="mailing-campaigns-table">
        <div class="mf-tr mf-th">
          <span class="wide">Campaign</span><span>Subject</span><span>Status</span
          ><span>Scheduled / updated</span>
        </div>
        @for (campaign of items(); track campaign.id) {
          <div class="mf-tr" data-testid="mailing-campaign-row">
            <span class="wide"
              ><a
                [routerLink]="['/mailing', businessId(), 'campaigns', campaign.id]"
                data-testid="mailing-campaign-open"
                >{{ campaign.name }}</a
              ></span
            >
            <span>{{ campaign.subject || 'No subject' }}</span>
            <span
              ><mf-status-pill [tone]="statusTone(campaign.status)" [label]="campaign.status"
            /></span>
            <span>{{ campaign.scheduled_at || campaign.updated_at | date: 'medium' }}</span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No campaigns yet" data-testid="mailing-campaigns-empty"
            >Create a draft above.</mf-empty-state
          >
        }
      </div>
      @if (nextCursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm more"
          data-testid="mailing-campaigns-load-more"
          [disabled]="loading()"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
      @if (error()) {
        <p class="mf-err" data-testid="mailing-campaigns-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .grow,
      .wide {
        flex: 2;
      }
      .mf-tr > span:not(.wide) {
        flex: 1;
      }
      .loading {
        display: flex;
        align-items: center;
        gap: 8px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-filters .mf-btn {
        align-self: end;
        min-height: 36px;
      }
      .no-lists,
      .more {
        margin-top: 12px;
      }
    `,
  ],
})
export class MailingCampaignsListComponent implements OnInit {
  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private router = inject(Router);
  private toast = inject(ToastService);
  private campaignLoadSeq = 0;
  private listLoadSeq = 0;

  businesses = signal<Business[]>([]);
  businessId = signal('');
  lists = signal<MailingList[]>([]);
  items = signal<MailingCampaign[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  loadingLists = signal(false);
  creating = signal(false);
  error = signal('');
  newName = '';
  newListId = '';
  readonly statusTone = mailingCampaignStatusTone;

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
    // A previous business may still be loading. Its response is ignored below, while these
    // resets allow the newly selected business to start its own requests immediately.
    this.loading.set(false);
    this.loadingLists.set(false);
    this.items.set([]);
    this.lists.set([]);
    this.nextCursor.set(null);
    this.newListId = '';
    this.loadLists(businessId);
    this.load();
  }

  loadMore(): void {
    if (this.nextCursor()) this.load(this.nextCursor()!);
  }

  private loadLists(businessId: string): void {
    const seq = ++this.listLoadSeq;
    this.loadingLists.set(true);
    this.mailing.listAllLists(businessId).subscribe({
      next: (items) => {
        if (seq !== this.listLoadSeq || businessId !== this.businessId()) return;
        const active = items.filter((list) => list.status === 'active');
        this.lists.set(active);
        this.newListId = active[0]?.id ?? '';
        this.loadingLists.set(false);
      },
      error: () => {
        if (seq !== this.listLoadSeq || businessId !== this.businessId()) return;
        this.loadingLists.set(false);
        this.error.set('Could not load mailing lists');
      },
    });
  }

  private load(cursor?: string): void {
    const businessId = this.businessId();
    if (!businessId || this.loading()) return;
    const seq = ++this.campaignLoadSeq;
    this.loading.set(true);
    this.mailing.listCampaigns(businessId, cursor).subscribe({
      next: (page) => {
        if (seq !== this.campaignLoadSeq || businessId !== this.businessId()) return;
        this.items.update((items) =>
          cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
        );
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
        this.error.set('');
      },
      error: () => {
        if (seq !== this.campaignLoadSeq || businessId !== this.businessId()) return;
        this.loading.set(false);
        this.error.set('Could not load campaigns');
      },
    });
  }

  create(): void {
    const name = this.newName.trim();
    if (!name || !this.newListId || this.creating()) return;
    const businessId = this.businessId();
    const listId = this.newListId;
    this.creating.set(true);
    this.mailing
      .createCampaign(businessId, {
        list_id: listId,
        name,
        subject: '',
        body_markdown: '',
        tag_filter: [],
        track_opens: true,
        track_clicks: true,
      })
      .subscribe({
        next: (campaign) => {
          this.creating.set(false);
          this.toast.success('Campaign created');
          void this.router.navigate(['/mailing', businessId, 'campaigns', campaign.id]);
        },
        error: () => {
          this.creating.set(false);
          this.toast.error('Could not create campaign');
        },
      });
  }
}
