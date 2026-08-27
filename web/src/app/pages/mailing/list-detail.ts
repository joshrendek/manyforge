import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import {
  MailingList,
  MailingListKey,
  MailingService,
  MailingSubscriber,
  SubscriberStatus,
} from '../../core/mailing.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { Tone } from '../../ui/status';
import { TagChipInput } from '../../ui/tag-chip-input/tag-chip-input';
import { ToastService } from '../../ui/toast/toast.service';
import { ContactsPickerComponent } from './contacts-picker';
import { SubscriberImportComponent } from './subscriber-import';

@Component({
  selector: 'app-mailing-list-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    EmptyState,
    PageHeader,
    Spinner,
    StatusPill,
    TagChipInput,
    ContactsPickerComponent,
    SubscriberImportComponent,
  ],
  template: `
    <div data-testid="mailing-list-detail">
      <div class="mf-card">
        <mf-page-header
          [title]="list()?.name || 'Mailing list'"
          subtitle="Manage subscribers and signup access"
        >
          <a
            routerLink="/mailing/lists"
            class="mf-btn mf-btn-ghost mf-btn-sm"
            data-testid="mailing-list-back"
            actions
            >Back to lists</a
          >
        </mf-page-header>

        @if (loadingList()) {
          <div class="loading" data-testid="mailing-list-loading"><mf-spinner /> Loading list…</div>
        } @else if (list(); as currentList) {
          <form class="settings" data-testid="mailing-list-settings" (ngSubmit)="saveList()">
            <div class="settings-fields">
              <div class="mf-field">
                <label for="list-detail-name">Name</label
                ><input
                  id="list-detail-name"
                  class="mf-input"
                  name="name"
                  data-testid="mailing-list-settings-name"
                  [(ngModel)]="listName"
                />
              </div>
              <div class="mf-field">
                <label for="list-detail-description">Description</label
                ><input
                  id="list-detail-description"
                  class="mf-input"
                  name="description"
                  data-testid="mailing-list-settings-description"
                  [(ngModel)]="listDescription"
                />
              </div>
            </div>
            <div class="settings-actions">
              <label class="mf-check"
                ><input
                  type="checkbox"
                  name="doubleOptIn"
                  data-testid="mailing-list-settings-double-opt-in"
                  [(ngModel)]="doubleOptIn"
                />
                Double opt-in</label
              >
              <mf-status-pill
                [tone]="currentList.status === 'active' ? 'success' : 'neutral'"
                [label]="currentList.status"
              />
              <button
                type="submit"
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="mailing-list-settings-save"
                [disabled]="!listName.trim() || savingList()"
              >
                {{ savingList() ? 'Saving…' : 'Save settings' }}
              </button>
              @if (currentList.status === 'active') {
                <button
                  type="button"
                  class="mf-btn mf-btn-danger mf-btn-sm"
                  data-testid="mailing-list-archive"
                  [disabled]="archiving()"
                  (click)="archiveList()"
                >
                  {{ archiving() ? 'Archiving…' : 'Archive list' }}
                </button>
              }
            </div>
          </form>
        }
        @if (error()) {
          <p class="mf-err" data-testid="mailing-list-error">{{ error() }}</p>
        }
      </div>

      @if (list()) {
        <section class="mf-card access" data-testid="mailing-list-access">
          <div class="section-title">
            <div>
              <h2>Signup access</h2>
              <p>Create a public key for hosted and embedded signup forms.</p>
            </div>
            <button
              type="button"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="mailing-key-create"
              [disabled]="creatingKey() || list()?.status !== 'active'"
              (click)="createKey()"
            >
              {{ creatingKey() ? 'Creating…' : 'Create key' }}
            </button>
          </div>
          @if (createdSecret()) {
            <div class="secret" data-testid="mailing-key-secret">
              <strong>Copy this signing secret now.</strong><code>{{ createdSecret() }}</code
              ><button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                data-testid="mailing-key-secret-copy"
                (click)="copy(createdSecret(), 'Secret copied')"
              >
                Copy secret</button
              ><button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                data-testid="mailing-key-secret-dismiss"
                (click)="createdSecret.set('')"
              >
                I saved it
              </button>
            </div>
          }
          @if (activeKey(); as key) {
            <div class="access-grid">
              <div class="mf-field">
                <label>Publishable key</label>
                <div class="copy-row">
                  <code data-testid="mailing-publishable-key">{{ key.publishable_key }}</code
                  ><button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="mailing-key-copy"
                    (click)="copy(key.publishable_key, 'Key copied')"
                  >
                    Copy
                  </button>
                </div>
              </div>
              <div class="mf-field">
                <label>Hosted signup URL</label>
                <div class="copy-row">
                  <code data-testid="mailing-hosted-url">{{ hostedUrl(key) }}</code
                  ><button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="mailing-hosted-url-copy"
                    (click)="copy(hostedUrl(key), 'URL copied')"
                  >
                    Copy
                  </button>
                </div>
              </div>
              <div class="mf-field embed-field">
                <label>Embed form</label>
                <div class="copy-row">
                  <pre data-testid="mailing-embed-snippet">{{ embedSnippet(key) }}</pre>
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="mailing-embed-copy"
                    (click)="copy(embedSnippet(key), 'Embed copied')"
                  >
                    Copy
                  </button>
                </div>
              </div>
            </div>
          } @else if (!loadingKeys()) {
            <p class="muted" data-testid="mailing-keys-empty">No enabled signup key.</p>
          }
          @if (keys().length) {
            <div class="key-list" data-testid="mailing-key-list">
              @for (key of keys(); track key.id) {
                <div class="key-row" data-testid="mailing-key-row">
                  <span>{{ key.label || 'Signup key' }}</span
                  ><code>{{ key.publishable_key }}</code
                  ><mf-status-pill
                    [tone]="key.status === 'enabled' ? 'success' : 'neutral'"
                    [label]="key.status"
                  />
                  @if (key.status === 'enabled') {
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="mailing-key-revoke"
                      (click)="revokeKey(key)"
                    >
                      Revoke
                    </button>
                  }
                </div>
              }
            </div>
          }
        </section>

        @if (list()?.status === 'active') {
          <div class="add-grid">
            <section class="mf-card" data-testid="subscriber-manual-add">
              <h3>Add subscriber</h3>
              <form class="manual-form" (ngSubmit)="addSubscriber()">
                <div class="mf-field">
                  <label for="subscriber-email">Email</label
                  ><input
                    id="subscriber-email"
                    type="email"
                    class="mf-input"
                    name="email"
                    data-testid="subscriber-email"
                    [(ngModel)]="newEmail"
                    required
                  />
                </div>
                <div class="name-grid">
                  <div class="mf-field">
                    <label for="subscriber-first">First name</label
                    ><input
                      id="subscriber-first"
                      class="mf-input"
                      name="first"
                      data-testid="subscriber-first-name"
                      [(ngModel)]="newFirstName"
                    />
                  </div>
                  <div class="mf-field">
                    <label for="subscriber-last">Last name</label
                    ><input
                      id="subscriber-last"
                      class="mf-input"
                      name="last"
                      data-testid="subscriber-last-name"
                      [(ngModel)]="newLastName"
                    />
                  </div>
                </div>
                <div class="mf-field">
                  <label>Tags</label
                  ><mf-tag-chip-input [tags]="newTags" (tagsChange)="newTags = $event" />
                </div>
                <label class="mf-check"
                  ><input
                    type="checkbox"
                    name="skip"
                    data-testid="subscriber-skip-confirmation"
                    [(ngModel)]="newSkipConfirmation"
                  />
                  Add as active without confirmation</label
                >
                <button
                  type="submit"
                  class="mf-btn mf-btn-primary mf-btn-sm"
                  data-testid="subscriber-add"
                  [disabled]="!newEmail.trim() || addingSubscriber()"
                >
                  {{ addingSubscriber() ? 'Adding…' : 'Add subscriber' }}
                </button>
              </form>
            </section>
            <app-mailing-subscriber-import
              [businessId]="businessId"
              [listId]="listId"
              (imported)="reloadSubscribers()"
            />
            <app-mailing-contacts-picker
              [businessId]="businessId"
              [listId]="listId"
              (added)="reloadSubscribers()"
            />
          </div>
        }

        <section class="mf-card" data-testid="mailing-subscribers">
          <div class="section-title">
            <div>
              <h2>Subscribers</h2>
              <p>Search by email or name and filter by status or tag.</p>
            </div>
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm"
              data-testid="subscribers-export"
              [disabled]="exporting()"
              (click)="exportCsv()"
            >
              {{ exporting() ? 'Exporting…' : 'Export CSV' }}
            </button>
          </div>
          <form class="mf-filters" data-testid="subscriber-filters" (ngSubmit)="applyFilters()">
            <div class="mf-field filter-grow">
              <label for="subscriber-search">Search</label
              ><input
                id="subscriber-search"
                class="mf-input"
                name="q"
                data-testid="subscriber-search"
                [(ngModel)]="filterQ"
              />
            </div>
            <div class="mf-field">
              <label for="subscriber-status">Status</label
              ><select
                id="subscriber-status"
                class="mf-select"
                name="status"
                data-testid="subscriber-status-filter"
                [(ngModel)]="filterStatus"
              >
                <option value="">All</option>
                @for (status of statuses; track status) {
                  <option [value]="status">{{ status }}</option>
                }
              </select>
            </div>
            <div class="mf-field">
              <label for="subscriber-tag">Tag</label
              ><input
                id="subscriber-tag"
                class="mf-input"
                name="tag"
                data-testid="subscriber-tag-filter"
                [(ngModel)]="filterTag"
              />
            </div>
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="subscriber-filter-apply"
            >
              Apply
            </button>
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm"
              data-testid="subscriber-filter-clear"
              (click)="clearFilters()"
            >
              Clear
            </button>
          </form>
          @if (loadingSubscribers()) {
            <div class="loading" data-testid="subscribers-loading">
              <mf-spinner /> Loading subscribers…
            </div>
          }
          <div class="mf-table" data-testid="subscribers-table">
            <div class="mf-tr mf-th">
              <span class="email-col">Subscriber</span><span>Status</span><span>Tags</span
              ><span>Source</span><span>Actions</span>
            </div>
            @for (subscriber of subscribers(); track subscriber.id) {
              <div class="mf-tr" data-testid="subscriber-row">
                <span class="email-col"
                  ><strong data-testid="subscriber-row-email">{{ subscriber.email }}</strong
                  ><small>{{ subscriber.first_name }} {{ subscriber.last_name }}</small></span
                >
                <span
                  ><mf-status-pill
                    [tone]="subscriberTone(subscriber.status)"
                    [label]="subscriber.status"
                /></span>
                <span class="tag-list" data-testid="subscriber-row-tags">
                  @for (tag of subscriber.tags; track tag) {
                    <span class="mf-pill mf-pill-neutral">{{ tag }}</span>
                  }
                </span>
                <span>{{ subscriber.consent_source }}</span>
                <span>
                  @if (subscriber.status !== 'unsubscribed' && list()?.status === 'active') {
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="subscriber-unsubscribe"
                      (click)="unsubscribe(subscriber)"
                    >
                      Unsubscribe
                    </button>
                  }
                </span>
              </div>
            }
            @if (!subscribers().length && !loadingSubscribers()) {
              <mf-empty-state title="No subscribers found" data-testid="subscribers-empty"
                >Add subscribers or adjust the filters.</mf-empty-state
              >
            }
          </div>
          @if (subscriberCursor()) {
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm more"
              data-testid="subscribers-load-more"
              [disabled]="loadingSubscribers()"
              (click)="loadMoreSubscribers()"
            >
              Load more
            </button>
          }
        </section>
      }
    </div>
  `,
  styles: [
    `
      :host {
        display: grid;
        gap: 18px;
      }
      .settings,
      .manual-form,
      .access {
        display: grid;
        gap: 14px;
      }
      .settings-fields,
      .name-grid,
      .access-grid {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
        gap: 14px;
      }
      .settings-actions,
      .loading,
      .section-title,
      .copy-row,
      .key-row,
      .mf-check {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .section-title {
        justify-content: space-between;
      }
      .section-title h2,
      .section-title p,
      h3,
      .muted {
        margin: 0;
      }
      .section-title p,
      .muted {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .secret {
        display: grid;
        gap: 8px;
        padding: 12px;
        border: 1px solid var(--mf-warn);
        border-radius: var(--mf-radius-sm);
      }
      code,
      pre {
        overflow-wrap: anywhere;
        white-space: pre-wrap;
        font-family: var(--mf-mono, ui-monospace, monospace);
        font-size: var(--mf-fs-xs);
      }
      .copy-row {
        align-items: flex-start;
      }
      .copy-row code,
      .copy-row pre {
        flex: 1;
        margin: 0;
        padding: 9px;
        background: var(--mf-surface-inset);
        border-radius: var(--mf-radius-sm);
      }
      .embed-field {
        grid-column: 1 / -1;
      }
      .key-list {
        display: grid;
        border-top: 1px solid var(--mf-border);
      }
      .key-row {
        padding: 10px 0;
        border-bottom: 1px solid var(--mf-border);
      }
      .key-row code {
        flex: 1;
      }
      .add-grid {
        display: grid;
        grid-template-columns: repeat(3, minmax(0, 1fr));
        gap: 16px;
        margin: 18px 0;
      }
      .mf-check {
        font-size: var(--mf-fs-sm);
      }
      .filter-grow,
      .email-col {
        flex: 2 !important;
      }
      .mf-filters .mf-btn {
        align-self: end;
        min-height: 36px;
      }
      .mf-tr > span {
        flex: 1;
      }
      .email-col {
        display: grid;
        gap: 3px;
      }
      .email-col small {
        color: var(--mf-text-muted);
      }
      .tag-list {
        display: flex;
        gap: 4px;
        flex-wrap: wrap;
      }
      .more {
        margin-top: 12px;
      }
      @media (max-width: 1000px) {
        .add-grid {
          grid-template-columns: 1fr;
        }
      }
      @media (max-width: 760px) {
        .settings-fields,
        .name-grid,
        .access-grid {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingListDetailComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private mailing = inject(MailingService);
  private toast = inject(ToastService);

  readonly statuses: SubscriberStatus[] = [
    'pending',
    'active',
    'unsubscribed',
    'bounced',
    'complained',
  ];
  businessId = '';
  listId = '';
  list = signal<MailingList | null>(null);
  loadingList = signal(true);
  savingList = signal(false);
  archiving = signal(false);
  error = signal('');
  listName = '';
  listDescription = '';
  doubleOptIn = true;

  keys = signal<MailingListKey[]>([]);
  loadingKeys = signal(true);
  creatingKey = signal(false);
  createdSecret = signal('');

  subscribers = signal<MailingSubscriber[]>([]);
  subscriberCursor = signal<string | null>(null);
  loadingSubscribers = signal(false);
  filterQ = '';
  filterStatus: SubscriberStatus | '' = '';
  filterTag = '';
  newEmail = '';
  newFirstName = '';
  newLastName = '';
  newTags: string[] = [];
  newSkipConfirmation = false;
  addingSubscriber = signal(false);
  exporting = signal(false);

  activeKey(): MailingListKey | undefined {
    return this.keys().find((key) => key.status === 'enabled');
  }

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.listId = this.route.snapshot.paramMap.get('listId') ?? '';
    if (!this.businessId || !this.listId) {
      this.loadingList.set(false);
      this.error.set('Mailing list route is invalid');
      return;
    }
    this.loadList();
    this.loadKeys();
    this.reloadSubscribers();
  }

  private loadList(): void {
    this.mailing.getList(this.businessId, this.listId).subscribe({
      next: (list) => {
        this.list.set(list);
        this.listName = list.name;
        this.listDescription = list.description ?? '';
        this.doubleOptIn = list.double_opt_in;
        this.loadingList.set(false);
      },
      error: () => {
        this.loadingList.set(false);
        this.error.set('Could not load mailing list');
      },
    });
  }

  saveList(): void {
    if (!this.listName.trim() || this.savingList()) return;
    this.savingList.set(true);
    this.mailing
      .updateList(this.businessId, this.listId, {
        name: this.listName.trim(),
        description: this.listDescription.trim() || null,
        double_opt_in: this.doubleOptIn,
      })
      .subscribe({
        next: (list) => {
          this.list.set(list);
          this.savingList.set(false);
          this.toast.success('List settings saved');
        },
        error: () => {
          this.savingList.set(false);
          this.toast.error('Could not save list settings');
        },
      });
  }

  archiveList(): void {
    if (this.archiving()) return;
    this.archiving.set(true);
    this.mailing.archiveList(this.businessId, this.listId).subscribe({
      next: () => {
        this.toast.success('Mailing list archived');
        void this.router.navigate(['/mailing/lists']);
      },
      error: () => {
        this.archiving.set(false);
        this.toast.error('Could not archive mailing list');
      },
    });
  }

  private loadKeys(): void {
    this.loadingKeys.set(true);
    this.mailing.listKeys(this.businessId, this.listId).subscribe({
      next: (page) => {
        this.keys.set(page.items ?? []);
        this.loadingKeys.set(false);
      },
      error: () => {
        this.loadingKeys.set(false);
        this.toast.error('Could not load signup keys');
      },
    });
  }

  createKey(): void {
    if (this.creatingKey() || this.list()?.status !== 'active') return;
    this.creatingKey.set(true);
    this.mailing.createKey(this.businessId, this.listId, 'Hosted signup').subscribe({
      next: (key) => {
        this.creatingKey.set(false);
        this.createdSecret.set(key.secret ?? '');
        const { secret: _secret, ...safeKey } = key;
        this.keys.update((keys) => [safeKey, ...keys]);
        this.toast.success('Signup key created');
      },
      error: () => {
        this.creatingKey.set(false);
        this.toast.error('Could not create signup key');
      },
    });
  }

  revokeKey(key: MailingListKey): void {
    this.mailing.revokeKey(this.businessId, this.listId, key.id).subscribe({
      next: () => {
        this.keys.update((keys) =>
          keys.map((item) => (item.id === key.id ? { ...item, status: 'revoked' } : item)),
        );
        this.toast.success('Signup key revoked');
      },
      error: () => this.toast.error('Could not revoke signup key'),
    });
  }

  hostedUrl(key: MailingListKey): string {
    return `${globalThis.location?.origin ?? ''}/m/s/${key.publishable_key}`;
  }
  embedSnippet(key: MailingListKey): string {
    return `<form method="post" action="${globalThis.location?.origin ?? ''}/api/v1/mailing/public/${key.publishable_key}/subscribe">\n  <input type="email" name="email" required>\n  <button type="submit">Subscribe</button>\n</form>`;
  }
  copy(value: string, success: string): void {
    const pending = navigator.clipboard?.writeText(value);
    if (!pending) {
      this.toast.error('Clipboard is unavailable');
      return;
    }
    void pending.then(
      () => this.toast.success(success),
      () => this.toast.error('Could not copy'),
    );
  }

  applyFilters(): void {
    this.reloadSubscribers();
  }
  clearFilters(): void {
    this.filterQ = '';
    this.filterStatus = '';
    this.filterTag = '';
    this.reloadSubscribers();
  }
  reloadSubscribers(): void {
    this.subscribers.set([]);
    this.subscriberCursor.set(null);
    this.loadSubscribers();
  }
  loadMoreSubscribers(): void {
    if (this.subscriberCursor()) this.loadSubscribers(this.subscriberCursor()!);
  }
  private loadSubscribers(cursor?: string): void {
    if (this.loadingSubscribers()) return;
    this.loadingSubscribers.set(true);
    this.mailing
      .listSubscribers(this.businessId, this.listId, {
        q: this.filterQ.trim() || undefined,
        status: this.filterStatus || undefined,
        tag: this.filterTag.trim() || undefined,
        cursor,
      })
      .subscribe({
        next: (page) => {
          this.subscribers.update((items) =>
            cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
          );
          this.subscriberCursor.set(page.next_cursor ?? null);
          this.loadingSubscribers.set(false);
        },
        error: () => {
          this.loadingSubscribers.set(false);
          this.toast.error('Could not load subscribers');
        },
      });
  }

  addSubscriber(): void {
    const email = this.newEmail.trim();
    if (!email || this.addingSubscriber() || this.list()?.status !== 'active') return;
    this.addingSubscriber.set(true);
    this.mailing
      .createSubscriber(this.businessId, this.listId, {
        email,
        first_name: this.newFirstName.trim() || null,
        last_name: this.newLastName.trim() || null,
        tags: this.newTags,
        skip_confirmation: this.newSkipConfirmation,
      })
      .subscribe({
        next: () => {
          this.addingSubscriber.set(false);
          this.newEmail = '';
          this.newFirstName = '';
          this.newLastName = '';
          this.newTags = [];
          this.toast.success('Subscriber added');
          this.reloadSubscribers();
        },
        error: () => {
          this.addingSubscriber.set(false);
          this.toast.error('Could not add subscriber');
        },
      });
  }

  unsubscribe(subscriber: MailingSubscriber): void {
    if (this.list()?.status !== 'active') return;
    this.mailing.unsubscribeSubscriber(this.businessId, this.listId, subscriber.id).subscribe({
      next: () => {
        this.subscribers.update((items) =>
          items.map((item) =>
            item.id === subscriber.id ? { ...item, status: 'unsubscribed' } : item,
          ),
        );
        this.toast.success('Subscriber unsubscribed');
      },
      error: () => this.toast.error('Could not unsubscribe subscriber'),
    });
  }

  exportCsv(): void {
    if (this.exporting()) return;
    this.exporting.set(true);
    this.mailing.exportSubscribers(this.businessId, this.listId).subscribe({
      next: (blob) => {
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = `${this.list()?.slug || 'subscribers'}.csv`;
        link.click();
        URL.revokeObjectURL(url);
        this.exporting.set(false);
      },
      error: () => {
        this.exporting.set(false);
        this.toast.error('Could not export subscribers');
      },
    });
  }

  subscriberTone(status: SubscriberStatus): Tone {
    if (status === 'active') return 'success';
    if (status === 'pending') return 'warn';
    if (status === 'bounced' || status === 'complained') return 'danger';
    return 'neutral';
  }
}
