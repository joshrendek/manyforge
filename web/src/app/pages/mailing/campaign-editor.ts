import { DatePipe } from '@angular/common';
import { Component, HostListener, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { catchError, forkJoin, of, switchMap } from 'rxjs';
import { CurrentBusinessService } from '../../core/current-business.service';
import {
  MailingCampaign,
  MailingList,
  MailingSendingProfile,
  MailingService,
} from '../../core/mailing.service';
import { HasUnsavedChanges, protectBeforeUnload } from '../../core/unsaved-changes.guard';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { mailingCampaignStatusTone } from '../../ui/status';
import { TagChipInput } from '../../ui/tag-chip-input/tag-chip-input';
import { ToastService } from '../../ui/toast/toast.service';
import { MailingContentDraft, MailingContentEditorComponent } from './content-editor';
import { MailingPreviewPaneComponent } from './preview-pane';

@Component({
  selector: 'app-mailing-campaign-editor',
  standalone: true,
  imports: [
    DatePipe,
    FormsModule,
    RouterLink,
    PageHeader,
    Spinner,
    StatusPill,
    TagChipInput,
    MailingContentEditorComponent,
    MailingPreviewPaneComponent,
  ],
  template: `
    <div class="mf-card" data-testid="mailing-campaign-editor">
      <mf-page-header title="Campaign editor" subtitle="Compose and deliver an email broadcast">
        <a
          routerLink="/mailing/campaigns"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="campaign-editor-back"
          actions
          >Back to campaigns</a
        >
      </mf-page-header>

      @if (loading()) {
        <div class="loading" data-testid="campaign-editor-loading">
          <mf-spinner /> Loading campaign…
        </div>
      } @else if (campaign()) {
        <div class="status-row">
          <mf-status-pill [tone]="statusTone(campaign()!.status)" [label]="campaign()!.status" />
          @if (campaign()!.scheduled_at) {
            <span class="mf-hint">Scheduled {{ campaign()!.scheduled_at | date: 'medium' }}</span>
          }
        </div>

        @if (!profileVerified()) {
          <div class="mf-banner warn" data-testid="campaign-profile-warning">
            A verified sending profile is required before this campaign can be saved or sent.
            <a routerLink="/mailing/sending" data-testid="campaign-profile-link"
              >Configure sending</a
            >
          </div>
        }

        <div class="workspace">
          <section class="form-pane">
            <div class="meta-grid">
              <div class="mf-field">
                <label for="campaign-editor-name">Campaign name</label>
                <input
                  id="campaign-editor-name"
                  class="mf-input"
                  name="campaignName"
                  data-testid="campaign-editor-name"
                  [(ngModel)]="name"
                  [readonly]="formDisabled()"
                />
              </div>
              <div class="mf-field">
                <label for="campaign-editor-list">Mailing list</label>
                <select
                  id="campaign-editor-list"
                  class="mf-select"
                  name="campaignList"
                  data-testid="campaign-editor-list"
                  [(ngModel)]="listId"
                  [disabled]="formDisabled()"
                >
                  @for (list of lists(); track list.id) {
                    <option [value]="list.id">{{ list.name }}</option>
                  }
                </select>
              </div>
            </div>

            <div class="mf-field">
              <span class="field-label">From</span>
              <div class="from-value" data-testid="campaign-editor-from">
                @if (profile()) {
                  {{ profile()!.from_name }} &lt;{{ profile()!.from_email }}&gt;
                } @else {
                  No sending profile configured
                }
              </div>
            </div>

            <div class="mf-field">
              <span class="field-label">Only send to subscribers tagged with all of</span>
              <mf-tag-chip-input
                [tags]="tagFilter"
                [disabled]="formDisabled()"
                placeholder="add filter tag…"
                inputTestId="campaign-tag-filter-input"
                chipTestId="campaign-tag-filter-chip"
                removeTestId="campaign-tag-filter-remove"
                (tagsChange)="tagFilter = $event"
              />
              <p class="mf-hint">Leave empty to include every active subscriber on the list.</p>
            </div>

            <app-mailing-content-editor
              [value]="content()"
              [readOnly]="formDisabled()"
              (valueChange)="content.set($event)"
            />
          </section>

          <app-mailing-preview-pane
            [businessId]="businessId"
            kind="campaigns"
            [content]="content()"
            [fromName]="profile()?.from_name ?? null"
            [postalAddress]="profile()?.postal_address ?? null"
          />
        </div>

        <section class="actions" data-testid="campaign-editor-actions">
          @if (campaign()!.status === 'draft') {
            <button
              type="button"
              class="mf-btn mf-btn-primary"
              data-testid="campaign-save"
              [disabled]="!canSave()"
              (click)="save()"
            >
              {{ busy() === 'save' ? 'Saving…' : 'Save' }}
            </button>

            <div class="inline-action">
              <label for="campaign-test-to">Test recipients</label>
              <input
                id="campaign-test-to"
                type="email"
                multiple
                class="mf-input"
                data-testid="campaign-test-to"
                placeholder="you@example.com, teammate@example.com"
                [(ngModel)]="testRecipients"
              />
              <button
                type="button"
                class="mf-btn mf-btn-ghost"
                data-testid="campaign-test-send"
                [disabled]="!canDeliver()"
                (click)="sendTest()"
              >
                {{ busy() === 'test' ? 'Sending…' : 'Send test' }}
              </button>
            </div>

            <div class="inline-action">
              <label for="campaign-schedule-at">Schedule ({{ timeZone }})</label>
              <input
                id="campaign-schedule-at"
                type="datetime-local"
                class="mf-input"
                data-testid="campaign-schedule-at"
                [min]="minSchedule"
                [(ngModel)]="scheduleAt"
              />
              <button
                type="button"
                class="mf-btn mf-btn-ghost"
                data-testid="campaign-schedule"
                [disabled]="!canDeliver() || !scheduleAt"
                (click)="schedule()"
              >
                {{ busy() === 'schedule' ? 'Scheduling…' : 'Schedule' }}
              </button>
            </div>

            @if (!confirmSend()) {
              <button
                type="button"
                class="mf-btn mf-btn-danger"
                data-testid="campaign-send-now"
                [disabled]="!canDeliver()"
                (click)="beginSendConfirmation()"
              >
                Send now
              </button>
            } @else {
              <div class="send-confirm" data-testid="campaign-send-confirmation">
                <span>Send this campaign to its matching subscribers now?</span>
                <button
                  type="button"
                  class="mf-btn mf-btn-danger"
                  data-testid="campaign-send-confirm"
                  [disabled]="busy() !== null"
                  (click)="sendNow()"
                >
                  {{ busy() === 'send' ? 'Sending…' : 'Confirm send' }}
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost"
                  data-testid="campaign-send-back"
                  [disabled]="busy() !== null"
                  (click)="confirmSend.set(false)"
                >
                  Back
                </button>
              </div>
            }
          } @else if (campaign()!.status === 'scheduled') {
            <button
              type="button"
              class="mf-btn mf-btn-danger"
              data-testid="campaign-cancel"
              [disabled]="busy() !== null"
              (click)="cancel()"
            >
              {{ busy() === 'cancel' ? 'Cancelling…' : 'Cancel campaign' }}
            </button>
          } @else if (campaign()!.status === 'sending') {
            <button
              type="button"
              class="mf-btn mf-btn-danger"
              data-testid="campaign-cancel"
              disabled
            >
              <mf-spinner /> Sending — cancellation unavailable
            </button>
          } @else {
            <a
              class="mf-btn mf-btn-primary"
              data-testid="campaign-view-stats"
              [routerLink]="['/mailing', businessId, 'campaigns', campaignId, 'stats']"
            >
              View stats
            </a>
          }
        </section>

        @if (campaign()!.last_error) {
          <p class="mf-err" data-testid="campaign-last-error">{{ campaign()!.last_error }}</p>
        }
      }
      @if (error()) {
        <p class="mf-err" data-testid="campaign-editor-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .loading,
      .status-row,
      .actions,
      .inline-action,
      .send-confirm {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .status-row {
        margin-bottom: 16px;
      }
      .workspace {
        display: grid;
        grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
        gap: 24px;
      }
      .form-pane {
        display: grid;
        align-content: start;
        gap: 16px;
        min-width: 0;
      }
      .meta-grid {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
        gap: 16px;
      }
      .field-label,
      .inline-action label {
        display: block;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        font-weight: 500;
      }
      .from-value {
        padding: 10px 12px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface-2);
        color: var(--mf-text-muted);
      }
      .mf-banner {
        margin-bottom: 16px;
        padding: 12px 14px;
        border: 1px solid var(--mf-warn);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-warn-soft);
        color: var(--mf-warn-text);
      }
      .actions {
        flex-wrap: wrap;
        margin-top: 20px;
        padding-top: 16px;
        border-top: 1px solid var(--mf-border);
      }
      .inline-action {
        flex-wrap: wrap;
      }
      .inline-action label {
        flex-basis: 100%;
      }
      .inline-action .mf-input {
        width: min(330px, 70vw);
      }
      .send-confirm {
        flex-wrap: wrap;
        padding: 10px;
        border: 1px solid var(--mf-danger);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-danger-soft);
      }
      @media (max-width: 960px) {
        .workspace,
        .meta-grid {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingCampaignEditorComponent implements OnInit, HasUnsavedChanges {
  private route = inject(ActivatedRoute);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);
  private savedSnapshot = '';
  private confirmedSnapshot = '';

  businessId = '';
  campaignId = '';
  campaign = signal<MailingCampaign | null>(null);
  lists = signal<MailingList[]>([]);
  profile = signal<MailingSendingProfile | null>(null);
  content = signal<MailingContentDraft>({
    subject: '',
    preheader: '',
    body_markdown: '',
    track_opens: true,
    track_clicks: true,
  });
  loading = signal(true);
  busy = signal<'save' | 'test' | 'schedule' | 'send' | 'cancel' | null>(null);
  confirmSend = signal(false);
  error = signal('');
  name = '';
  listId = '';
  tagFilter: string[] = [];
  testRecipients = '';
  scheduleAt = '';
  readonly timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  readonly minSchedule = localDateTime(new Date(Date.now() + 60_000));
  readonly statusTone = mailingCampaignStatusTone;
  readonly readOnly = computed(() => this.campaign()?.status !== 'draft');
  readonly formDisabled = computed(() => this.readOnly() || this.busy() !== null);
  readonly profileVerified = computed(() => this.profile()?.status === 'verified');

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.campaignId = this.route.snapshot.paramMap.get('campaignId') ?? '';
    if (!this.businessId || !this.campaignId) {
      this.loading.set(false);
      this.error.set('Campaign route is invalid');
      return;
    }
    this.current.set(this.businessId);
    forkJoin({
      campaign: this.mailing.getCampaign(this.businessId, this.campaignId),
      lists: this.mailing.listAllLists(this.businessId),
      profile: this.mailing.getSendingProfile(this.businessId).pipe(catchError(() => of(null))),
    }).subscribe({
      next: ({ campaign, lists, profile }) => {
        this.lists.set(lists.filter((list) => list.status === 'active'));
        this.profile.set(profile);
        this.populate(campaign);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load campaign');
      },
    });
  }

  canSave(): boolean {
    return (
      this.profileVerified() &&
      !this.readOnly() &&
      this.busy() === null &&
      !!this.name.trim() &&
      !!this.listId &&
      this.hasUnsavedChanges()
    );
  }

  canDeliver(): boolean {
    const content = this.content();
    return (
      this.profileVerified() &&
      !this.readOnly() &&
      this.busy() === null &&
      !!this.name.trim() &&
      !!this.listId &&
      !!content.subject.trim() &&
      !!content.body_markdown.trim()
    );
  }

  hasUnsavedChanges(): boolean {
    return (
      !this.readOnly() &&
      !!this.savedSnapshot &&
      (this.snapshot() !== this.savedSnapshot || !!this.scheduleAt)
    );
  }

  beginSendConfirmation(): void {
    if (!this.canDeliver()) return;
    this.confirmedSnapshot = this.snapshot();
    this.confirmSend.set(true);
  }

  @HostListener('window:beforeunload', ['$event'])
  beforeUnload(event: BeforeUnloadEvent): void {
    protectBeforeUnload(event, this.hasUnsavedChanges());
  }

  save(): void {
    if (!this.canSave()) return;
    this.busy.set('save');
    this.persist().subscribe({
      next: (campaign) => {
        this.populate(campaign);
        this.busy.set(null);
        this.toast.success('Campaign saved');
      },
      error: () => this.fail('Could not save campaign'),
    });
  }

  sendTest(): void {
    if (!this.canDeliver()) return;
    const recipients = parseRecipients(this.testRecipients);
    if (!recipients) {
      this.toast.error('Enter between 1 and 5 valid email addresses');
      return;
    }
    this.busy.set('test');
    this.persist()
      .pipe(
        switchMap((campaign) => {
          this.populate(campaign);
          return this.mailing.testCampaign(this.businessId, this.campaignId, recipients);
        }),
      )
      .subscribe({
        next: () => {
          this.busy.set(null);
          this.toast.success('Test campaign sent');
        },
        error: () => this.fail('Could not send test campaign'),
      });
  }

  schedule(): void {
    if (!this.canDeliver() || !this.scheduleAt) return;
    const scheduled = new Date(this.scheduleAt);
    if (!Number.isFinite(scheduled.getTime()) || scheduled.getTime() <= Date.now()) {
      this.toast.error('Choose a future schedule time');
      return;
    }
    this.busy.set('schedule');
    this.persist()
      .pipe(
        switchMap((campaign) => {
          this.populate(campaign);
          return this.mailing.sendCampaign(
            this.businessId,
            this.campaignId,
            scheduled.toISOString(),
          );
        }),
      )
      .subscribe({
        next: (campaign) => {
          this.populate(campaign);
          this.busy.set(null);
          this.toast.success('Campaign scheduled');
        },
        error: () => this.fail('Could not schedule campaign'),
      });
  }

  sendNow(): void {
    if (!this.canDeliver() || !this.confirmSend()) return;
    if (this.snapshot() !== this.confirmedSnapshot) {
      this.confirmSend.set(false);
      this.confirmedSnapshot = '';
      this.toast.error('Campaign changed; review it before confirming again');
      return;
    }
    this.busy.set('send');
    this.persist()
      .pipe(
        switchMap((campaign) => {
          this.populate(campaign);
          return this.mailing.sendCampaign(this.businessId, this.campaignId, null);
        }),
      )
      .subscribe({
        next: (campaign) => {
          this.populate(campaign);
          this.confirmSend.set(false);
          this.confirmedSnapshot = '';
          this.busy.set(null);
          this.toast.success('Campaign queued for sending');
        },
        error: () => this.fail('Could not send campaign'),
      });
  }

  cancel(): void {
    if (this.campaign()?.status !== 'scheduled' || this.busy() !== null) return;
    this.busy.set('cancel');
    this.mailing.cancelCampaign(this.businessId, this.campaignId).subscribe({
      next: (campaign) => {
        this.populate(campaign);
        this.busy.set(null);
        this.toast.success('Campaign cancelled');
      },
      error: () => this.fail('Could not cancel campaign'),
    });
  }

  private persist() {
    const content = this.content();
    return this.mailing.updateCampaign(this.businessId, this.campaignId, {
      list_id: this.listId,
      name: this.name.trim(),
      subject: content.subject.trim(),
      preheader: content.preheader.trim() || null,
      body_markdown: content.body_markdown,
      tag_filter: this.tagFilter,
      track_opens: content.track_opens,
      track_clicks: content.track_clicks,
    });
  }

  private populate(campaign: MailingCampaign): void {
    this.campaign.set(campaign);
    this.name = campaign.name;
    this.listId = campaign.list_id;
    this.tagFilter = [...campaign.tag_filter];
    this.content.set({
      subject: campaign.subject,
      preheader: campaign.preheader ?? '',
      body_markdown: campaign.body_markdown,
      track_opens: campaign.track_opens,
      track_clicks: campaign.track_clicks,
    });
    this.savedSnapshot = this.snapshot();
  }

  private snapshot(): string {
    return JSON.stringify({
      name: this.name,
      list_id: this.listId,
      tag_filter: this.tagFilter,
      content: this.content(),
    });
  }

  private fail(message: string): void {
    this.busy.set(null);
    this.toast.error(message);
  }
}

function parseRecipients(value: string): string[] | null {
  const recipients = [
    ...new Set(
      value
        .split(',')
        .map((item) => item.trim().toLowerCase())
        .filter(Boolean),
    ),
  ];
  if (recipients.length < 1 || recipients.length > 5) return null;
  return recipients.every((email) => /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) ? recipients : null;
}

function localDateTime(date: Date): string {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}
