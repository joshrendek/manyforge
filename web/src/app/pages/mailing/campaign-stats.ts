import { DatePipe } from '@angular/common';
import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import {
  MailingCampaignStats,
  MailingDelivery,
  MailingDeliveryStatus,
  MailingService,
} from '../../core/mailing.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { mailingCampaignStatusTone, mailingDeliveryStatusTone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { StatTile, StatTiles } from '../../ui/stat-tiles/stat-tiles';

@Component({
  selector: 'app-mailing-campaign-stats',
  standalone: true,
  imports: [
    DatePipe,
    FormsModule,
    RouterLink,
    EmptyState,
    PageHeader,
    Spinner,
    StatusPill,
    StatTiles,
  ],
  template: `
    <div class="mf-card page" data-testid="mailing-campaign-stats-page">
      <mf-page-header
        [title]="stats()?.campaign?.name || 'Campaign statistics'"
        [subtitle]="stats()?.campaign?.subject || 'Delivery and engagement performance'"
      >
        @if (stats(); as current) {
          <mf-status-pill
            actions
            [tone]="campaignTone(current.campaign.status)"
            [label]="current.campaign.status"
            data-testid="campaign-stats-status"
          />
        }
        <a
          actions
          class="mf-btn mf-btn-ghost mf-btn-sm"
          [routerLink]="['/mailing', businessId, 'campaigns', campaignId]"
          data-testid="campaign-stats-edit"
          >Campaign</a
        >
        <a
          actions
          class="mf-btn mf-btn-ghost mf-btn-sm"
          routerLink="/mailing/campaigns"
          data-testid="campaign-stats-back"
          >All campaigns</a
        >
      </mf-page-header>

      @if (loadingStats()) {
        <p class="loading" data-testid="campaign-stats-loading"><mf-spinner /> Loading stats…</p>
      } @else if (stats(); as current) {
        <mf-stat-tiles [tiles]="tiles()" data-testid="campaign-stats-tiles" />

        <section class="section" aria-labelledby="campaign-link-stats-heading">
          <h2 id="campaign-link-stats-heading">Link performance</h2>
          <div class="mf-table" data-testid="campaign-link-stats">
            <div class="mf-tr mf-th">
              <span class="url">Destination</span><span>Unique clicks</span
              ><span>Total clicks</span>
            </div>
            @for (link of current.links; track link.url) {
              <div class="mf-tr" data-testid="campaign-link-row">
                <span class="url mf-ellipsis" [title]="link.url">{{ link.url }}</span>
                <span>{{ link.unique_click_count.toLocaleString() }}</span>
                <span>{{ link.click_count.toLocaleString() }}</span>
              </div>
            }
            @if (!current.links.length) {
              <mf-empty-state title="No link clicks yet" data-testid="campaign-links-empty">
                Tracked links appear here after recipients click them.
              </mf-empty-state>
            }
          </div>
        </section>

        <section class="section" aria-labelledby="campaign-deliveries-heading">
          <div class="section-header">
            <h2 id="campaign-deliveries-heading">Deliveries</h2>
            <div class="mf-field filter-field">
              <label for="campaign-delivery-status">Status</label>
              <select
                id="campaign-delivery-status"
                class="mf-select"
                data-testid="campaign-delivery-status"
                [ngModel]="deliveryStatus()"
                (ngModelChange)="setDeliveryStatus($event)"
              >
                <option value="">All statuses</option>
                @for (status of deliveryStatuses; track status) {
                  <option [value]="status">{{ status }}</option>
                }
              </select>
            </div>
          </div>
          <div class="mf-table" data-testid="campaign-deliveries-table">
            <div class="mf-tr mf-th">
              <span class="email">Recipient</span><span>Status</span><span>Engagement</span
              ><span>Created</span>
            </div>
            @for (delivery of deliveries(); track delivery.id) {
              <div class="mf-tr" data-testid="campaign-delivery-row">
                <span class="email mf-ellipsis" [title]="delivery.email">{{ delivery.email }}</span>
                <span class="status-cell">
                  <mf-status-pill
                    [tone]="deliveryTone(delivery.status)"
                    [label]="delivery.status"
                  />
                  @if (delivery.last_error) {
                    <small class="delivery-error" [title]="delivery.last_error">{{
                      delivery.last_error
                    }}</small>
                  }
                </span>
                <span>{{ engagementLabel(delivery) }}</span>
                <span>{{ delivery.created_at | date: 'medium' }}</span>
              </div>
            }
            @if (!deliveries().length && !loadingDeliveries()) {
              <mf-empty-state title="No deliveries" data-testid="campaign-deliveries-empty">
                No recipients match this status.
              </mf-empty-state>
            }
          </div>
          @if (loadingDeliveries()) {
            <p class="loading" data-testid="campaign-deliveries-loading">
              <mf-spinner /> Loading deliveries…
            </p>
          }
          @if (nextCursor()) {
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm load-more"
              data-testid="campaign-deliveries-load-more"
              [disabled]="loadingDeliveries()"
              (click)="loadMore()"
            >
              Load more
            </button>
          }
          @if (deliveryError()) {
            <p class="mf-err" data-testid="campaign-deliveries-error">{{ deliveryError() }}</p>
          }
        </section>
      }

      @if (error()) {
        <p class="mf-err" data-testid="campaign-stats-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .page {
        max-width: 1180px;
      }
      .loading,
      .section-header {
        display: flex;
        align-items: center;
      }
      .loading {
        gap: 8px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .section {
        margin-top: 28px;
      }
      .section h2 {
        margin: 0 0 12px;
        font-size: var(--mf-fs-lg);
      }
      .section-header {
        justify-content: space-between;
        gap: 16px;
      }
      .filter-field {
        min-width: 190px;
      }
      .url,
      .email {
        flex: 3;
        min-width: 0;
      }
      .mf-tr > span:not(.url):not(.email) {
        flex: 1;
      }
      .delivery-error {
        color: var(--mf-danger);
      }
      .status-cell {
        align-items: flex-start;
        display: flex;
        flex-direction: column;
        gap: 4px;
        min-width: 0;
      }
      .load-more {
        margin-top: 12px;
      }
      @media (max-width: 720px) {
        .section-header {
          align-items: stretch;
          flex-direction: column;
        }
      }
    `,
  ],
})
export class MailingCampaignStatsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private mailing = inject(MailingService);
  private deliveryLoadSeq = 0;

  businessId = '';
  campaignId = '';
  stats = signal<MailingCampaignStats | null>(null);
  deliveries = signal<MailingDelivery[]>([]);
  deliveryStatus = signal<MailingDeliveryStatus | ''>('');
  nextCursor = signal<string | null>(null);
  loadingStats = signal(false);
  loadingDeliveries = signal(false);
  error = signal('');
  deliveryError = signal('');
  readonly campaignTone = mailingCampaignStatusTone;
  readonly deliveryTone = mailingDeliveryStatusTone;
  readonly deliveryStatuses: MailingDeliveryStatus[] = [
    'queued',
    'sending',
    'sent',
    'delivered',
    'bounced',
    'complained',
    'failed',
    'suppressed',
    'cancelled',
  ];

  tiles = computed<StatTile[]>(() => {
    const campaign = this.stats()?.campaign;
    if (!campaign) return [];
    return [
      {
        label: 'Recipients',
        value: this.number(campaign.recipient_count),
        testid: 'stat-recipients',
      },
      { label: 'Sent', value: this.number(campaign.sent_count), testid: 'stat-sent' },
      {
        label: 'Delivered',
        value: this.number(campaign.delivered_count),
        detail: this.rate(campaign.delivered_count, campaign.recipient_count, 'of recipients'),
        testid: 'stat-delivered',
      },
      {
        label: 'Opened',
        value: this.number(campaign.opened_count),
        detail: this.rate(campaign.opened_count, campaign.delivered_count, 'of delivered'),
        testid: 'stat-opened',
      },
      {
        label: 'Clicked',
        value: this.number(campaign.clicked_count),
        detail: this.rate(campaign.clicked_count, campaign.delivered_count, 'of delivered'),
        testid: 'stat-clicked',
      },
      { label: 'Bounced', value: this.number(campaign.bounced_count), testid: 'stat-bounced' },
      {
        label: 'Complaints',
        value: this.number(campaign.complained_count),
        testid: 'stat-complained',
      },
      {
        label: 'Unsubscribed',
        value: this.number(campaign.unsubscribed_count),
        testid: 'stat-unsubscribed',
      },
      { label: 'Failed', value: this.number(campaign.failed_count), testid: 'stat-failed' },
    ];
  });

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.campaignId = this.route.snapshot.paramMap.get('campaignId') ?? '';
    if (!this.businessId || !this.campaignId) {
      this.error.set('Campaign not found');
      return;
    }
    this.loadStats();
    this.loadDeliveries();
  }

  setDeliveryStatus(status: MailingDeliveryStatus | ''): void {
    this.deliveryStatus.set(status);
    this.deliveries.set([]);
    this.nextCursor.set(null);
    this.loadingDeliveries.set(false);
    this.loadDeliveries();
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (cursor) this.loadDeliveries(cursor);
  }

  engagementLabel(delivery: MailingDelivery): string {
    if (delivery.first_clicked_at) return 'Clicked';
    if (delivery.opened_at) return 'Opened';
    return '—';
  }

  private loadStats(): void {
    this.loadingStats.set(true);
    this.mailing.getCampaignStats(this.businessId, this.campaignId).subscribe({
      next: (stats) => {
        this.stats.set(stats);
        this.loadingStats.set(false);
        this.error.set('');
      },
      error: () => {
        this.loadingStats.set(false);
        this.error.set('Could not load campaign statistics');
      },
    });
  }

  private loadDeliveries(cursor?: string): void {
    if (this.loadingDeliveries()) return;
    const seq = ++this.deliveryLoadSeq;
    const status = this.deliveryStatus() || undefined;
    this.loadingDeliveries.set(true);
    this.mailing
      .listCampaignDeliveries(this.businessId, this.campaignId, { status, cursor, limit: 50 })
      .subscribe({
        next: (page) => {
          if (seq !== this.deliveryLoadSeq) return;
          this.deliveries.update((items) =>
            cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
          );
          this.nextCursor.set(page.next_cursor ?? null);
          this.loadingDeliveries.set(false);
          this.deliveryError.set('');
        },
        error: () => {
          if (seq !== this.deliveryLoadSeq) return;
          this.loadingDeliveries.set(false);
          this.deliveryError.set('Could not load campaign deliveries');
        },
      });
  }

  private number(value: number): string {
    return value.toLocaleString();
  }

  private rate(value: number, total: number, suffix: string): string {
    return total > 0 ? `${((value / total) * 100).toFixed(1)}% ${suffix}` : `0% ${suffix}`;
  }
}
