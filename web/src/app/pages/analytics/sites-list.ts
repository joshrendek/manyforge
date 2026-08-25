import { HttpErrorResponse } from '@angular/common/http';
import {
  Component,
  ElementRef,
  OnInit,
  inject,
  signal,
  viewChild,
  viewChildren,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import {
  AnalyticsPropertyRuleInput,
  AnalyticsService,
  TelemetryClient,
  TelemetryMoveTarget,
} from '../../core/analytics.service';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { Tone } from '../../ui/status';

// Analytics sites: register a site, get an embed snippet, revoke when done.
//
// The embed snippet is the whole point of this screen, so it is shown inline on every active
// site rather than hidden behind a detail page — the common task is "copy the tag onto another
// business's site", and that should be one click from the list.
@Component({
  selector: 'app-analytics-sites-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="analytics-sites-page">
      <mf-page-header title="Analytics sites" subtitle="Register a site, then paste its embed tag">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="sites-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="an-biz-select">Business</label>
          <select
            id="an-biz-select"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
            [disabled]="moving()"
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
        <form class="mf-filters" data-testid="site-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:1 1 260px">
            <label for="an-new-name">New site name</label>
            <input
              id="an-new-name"
              class="mf-input"
              type="text"
              name="newName"
              placeholder="e.g. garden.gg"
              [(ngModel)]="newName"
            />
          </div>
          <div class="mf-field" style="flex:1 1 300px">
            <label for="an-new-origin">Allowed origin</label>
            <input
              id="an-new-origin"
              class="mf-input"
              type="url"
              name="newOrigin"
              placeholder="https://garden.gg"
              [(ngModel)]="newOrigin"
            />
            <span class="mf-field-hint">Exact site origin; no path or wildcard.</span>
          </div>
          <div style="display:flex;align-items:flex-end">
            <button
              #createBtn
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="site-create"
              [disabled]="!newName.trim() || !newOrigin.trim() || creating()"
            >
              {{ creating() ? 'Creating…' : 'Add site' }}
            </button>
          </div>
        </form>
      }

      <div class="mf-table" data-testid="sites-list" role="table" aria-label="Analytics sites">
        <div class="mf-tr mf-th" role="row">
          <span style="flex:2" role="columnheader">Site</span>
          <span style="flex:3" role="columnheader">Embed tag</span>
          <span style="flex:2" role="columnheader">Status</span>
          <span style="flex:2" role="columnheader">Actions</span>
        </div>
        @for (c of sites(); track c.id) {
          <div class="mf-tr" role="row" data-testid="site-row" [attr.data-site-id]="c.id">
            <span style="flex:2" role="cell" data-testid="site-name-cell">
              @if (c.status === 'active') {
                <a [routerLink]="['/analytics', businessId(), c.id]">{{ c.name }}</a>
              } @else {
                {{ c.name }}
              }
              @if (c.allowed_origins.length) {
                <small class="mf-origin-summary" data-testid="site-allowed-origins">
                  {{ c.allowed_origins.join(', ') }}
                </small>
              } @else {
                <small
                  class="mf-origin-summary mf-origin-unrestricted"
                  data-testid="site-origin-unrestricted"
                >
                  Legacy site: any origin can send data
                </small>
              }
            </span>
            <span style="flex:3" role="cell">
              @if (c.status === 'active') {
                <code class="mf-embed" data-testid="site-embed">{{ embed(c) }}</code>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  data-testid="site-manage-origins"
                  [attr.aria-label]="'Manage allowed origins for ' + c.name"
                  (click)="startOrigins(c)"
                >
                  Origins
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  data-testid="site-manage-properties"
                  [attr.aria-label]="'Manage retained event properties for ' + c.name"
                  (click)="startProperties(c)"
                >
                  Properties
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  data-testid="site-embed-copy"
                  [attr.aria-label]="'Copy embed tag for ' + c.name"
                  (click)="copyEmbed(c)"
                >
                  Copy
                </button>
              } @else {
                <span class="mf-muted">—</span>
              }
            </span>
            <span
              #statusCell
              style="flex:2"
              role="cell"
              tabindex="-1"
              data-testid="site-status-cell"
              [attr.data-site-id]="c.id"
            >
              @if (c.status === 'revoked') {
                <mf-status-pill tone="neutral" label="Revoked" />
              } @else {
                <mf-status-pill
                  [tone]="healthTone(c)"
                  [label]="healthLabel(c)"
                  [ariaLabel]="c.name + ' installation health: ' + healthLabel(c)"
                />
              }
            </span>
            <span style="flex:2" role="cell">
              @if (c.status === 'active') {
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  data-testid="site-move"
                  [attr.aria-label]="'Move ' + c.name"
                  (click)="startMove(c)"
                >
                  Move
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm mf-btn-danger"
                  data-testid="site-revoke"
                  [attr.aria-label]="'Revoke ' + c.name"
                  (click)="revoke(c)"
                >
                  Revoke
                </button>
              }
            </span>
          </div>
          @if (c.status === 'active') {
            <section
              class="mf-health-panel"
              data-testid="site-health"
              [attr.aria-labelledby]="'site-health-title-' + c.id"
            >
              <h2 class="mf-sr-only" [id]="'site-health-title-' + c.id">
                Installation health for {{ c.name }}
              </h2>
              <div class="mf-health-summary" role="status" aria-live="polite" aria-atomic="true">
                <strong data-testid="site-health-message">{{ healthMessage(c) }}</strong>
                @if (c.analytics_health?.last_accepted_at; as lastAcceptedAt) {
                  <span>
                    Last accepted event
                    <time [attr.datetime]="lastAcceptedAt">{{ lastAcceptedAt }}</time
                    >.
                  </span>
                } @else if (c.analytics_health?.status === 'stale') {
                  <span>Earlier activity predates exact health tracking.</span>
                }
                @if (c.analytics_health?.data_as_of; as dataAsOf) {
                  <span>
                    Analytics current through <time [attr.datetime]="dataAsOf">{{ dataAsOf }}</time
                    >.
                  </span>
                } @else {
                  <span>Analytics freshness is not available yet.</span>
                }
              </div>
              @if (needsHealthAction(c)) {
                <div class="mf-install-check" data-testid="site-install-checklist">
                  <ol>
                    <li>Copy the embed tag above.</li>
                    <li>Paste it into the site's HTML and publish the change.</li>
                    <li>Visit or reload the site once.</li>
                  </ol>
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm mf-btn-primary"
                    data-testid="site-check-installation"
                    [attr.aria-label]="
                      'Check installation status for ' + c.name + ', currently ' + healthLabel(c)
                    "
                    [disabled]="verifyingSiteId() === c.id"
                    (click)="checkInstallation(c)"
                  >
                    {{ verifyingSiteId() === c.id ? 'Checking…' : 'Check installation' }}
                  </button>
                </div>
              }
            </section>
          }
          @if (editingOriginsSiteId() === c.id) {
            <div class="mf-origin-panel" data-testid="site-origin-panel">
              <div class="mf-field mf-origin-field">
                <label [for]="'allowed-origins-' + c.id">Allowed origins for {{ c.name }}</label>
                <textarea
                  [id]="'allowed-origins-' + c.id"
                  class="mf-input"
                  data-testid="site-origin-input"
                  [(ngModel)]="originsDraft"
                  [name]="'allowedOrigins-' + c.id"
                  rows="3"
                ></textarea>
                <span class="mf-field-hint">
                  One exact HTTPS origin per line (up to 10). Origin is an integrity guard, not
                  authentication. Localhost may use HTTP.
                </span>
              </div>
              <div class="mf-origin-actions">
                <button
                  type="button"
                  class="mf-btn mf-btn-sm mf-btn-primary"
                  data-testid="site-origin-save"
                  [disabled]="!parsedOrigins().length || savingOrigins()"
                  (click)="saveOrigins(c)"
                >
                  {{ savingOrigins() ? 'Saving…' : 'Save origins' }}
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  [disabled]="savingOrigins()"
                  (click)="cancelOrigins()"
                >
                  Cancel
                </button>
              </div>
            </div>
          }
          @if (editingPropertiesSiteId() === c.id) {
            <section class="mf-property-panel" data-testid="site-property-panel">
              <div class="mf-property-heading">
                <div>
                  <strong>Retained custom-event properties for {{ c.name }}</strong>
                  <p class="mf-property-privacy">
                    Only saved event/key pairs are retained. Sensitive data, secrets, and
                    persistent identifiers are prohibited. Existing raw properties expire after
                    90 days and are never enabled retroactively.
                  </p>
                </div>
                <span class="mf-muted">{{ propertyDrafts().length }}/20</span>
              </div>
              @if (loadingProperties()) {
                <span class="mf-loading-row"><mf-spinner /> Loading properties…</span>
              } @else {
                @for (rule of propertyDrafts(); track $index; let i = $index) {
                  <div class="mf-property-row" data-testid="site-property-row">
                    <div class="mf-field">
                      <label [for]="'property-event-' + c.id + '-' + i">Event name</label>
                      <input
                        [id]="'property-event-' + c.id + '-' + i"
                        class="mf-input"
                        data-testid="site-property-event"
                        [ngModel]="rule.event_name"
                        (ngModelChange)="updateProperty(i, 'event_name', $event)"
                        [name]="'propertyEvent-' + c.id + '-' + i"
                        placeholder="checkout_completed"
                      />
                    </div>
                    <div class="mf-field">
                      <label [for]="'property-key-' + c.id + '-' + i">Property key</label>
                      <input
                        [id]="'property-key-' + c.id + '-' + i"
                        class="mf-input"
                        data-testid="site-property-key"
                        [ngModel]="rule.property_key"
                        (ngModelChange)="updateProperty(i, 'property_key', $event)"
                        [name]="'propertyKey-' + c.id + '-' + i"
                        placeholder="plan"
                      />
                    </div>
                    <div class="mf-field">
                      <label [for]="'property-label-' + c.id + '-' + i">Dashboard label</label>
                      <input
                        [id]="'property-label-' + c.id + '-' + i"
                        class="mf-input"
                        data-testid="site-property-label"
                        [ngModel]="rule.label"
                        (ngModelChange)="updateProperty(i, 'label', $event)"
                        [name]="'propertyLabel-' + c.id + '-' + i"
                        placeholder="Plan"
                      />
                    </div>
                    <button
                      type="button"
                      class="mf-btn mf-btn-sm"
                      [attr.aria-label]="'Remove property rule ' + (i + 1)"
                      (click)="removeProperty(i)"
                    >
                      Remove
                    </button>
                  </div>
                }
                @if (!propertyDrafts().length) {
                  <p class="mf-muted" data-testid="site-properties-empty">
                    No custom-event properties are retained.
                  </p>
                }
                <div class="mf-property-actions">
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm"
                    data-testid="site-property-add"
                    [disabled]="propertyDrafts().length >= 20 || savingProperties()"
                    (click)="addProperty()"
                  >
                    Add property
                  </button>
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm mf-btn-primary"
                    data-testid="site-property-save"
                    [disabled]="!propertyDraftsValid() || savingProperties()"
                    (click)="saveProperties(c)"
                  >
                    {{ savingProperties() ? 'Saving…' : 'Save properties' }}
                  </button>
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm"
                    [disabled]="savingProperties()"
                    (click)="cancelProperties()"
                  >
                    Cancel
                  </button>
                </div>
              }
            </section>
          }
          @if (movingSiteId() === c.id) {
            <div class="mf-move-panel" data-testid="site-move-panel">
              @if (loadingTargets()) {
                <span class="mf-loading-row"><mf-spinner /> Loading eligible businesses…</span>
              } @else if (!moveTargets().length) {
                <span class="mf-muted">No other eligible businesses are available.</span>
                <button type="button" class="mf-btn mf-btn-sm" (click)="cancelMove()">Close</button>
              } @else {
                <div class="mf-field">
                  <label [for]="'move-target-' + c.id">Move {{ c.name }} to</label>
                  <select
                    [id]="'move-target-' + c.id"
                    class="mf-select"
                    data-testid="site-move-target"
                    [(ngModel)]="moveTargetId"
                    name="moveTarget"
                  >
                    <option value="" disabled>Choose a destination…</option>
                    @for (target of moveTargets(); track target.id) {
                      <option [value]="target.id">
                        {{ target.is_tenant_root ? target.name + ' (master)' : target.name }}
                      </option>
                    }
                  </select>
                </div>
                @if (moveTargetId) {
                  <p class="mf-move-confirm" data-testid="site-move-confirmation">
                    Move this site to <strong>{{ selectedTargetName() }}</strong
                    >? Its embed tag, publishable key, and complete analytics history will stay
                    unchanged.
                  </p>
                }
                <div class="mf-move-actions">
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm mf-btn-primary"
                    data-testid="site-move-confirm"
                    [disabled]="!moveTargetId || moving()"
                    (click)="confirmMove(c)"
                  >
                    {{ moving() ? 'Moving…' : 'Move site' }}
                  </button>
                  <button
                    type="button"
                    class="mf-btn mf-btn-sm"
                    [disabled]="moving()"
                    (click)="cancelMove()"
                  >
                    Cancel
                  </button>
                </div>
              }
            </div>
          }
        }
        @if (!sites().length && businessId() && !loading()) {
          <mf-empty-state title="No sites yet" data-testid="sites-empty">
            Add one above, then paste its embed tag into your site's HTML.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="sites-error">{{ error() }}</p>
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
      .mf-embed {
        display: inline-block;
        max-width: 100%;
        /* Wrap rather than truncate: an embed tag the user cannot fully see is one they cannot
           verify before pasting it into their own site. */
        word-break: break-all;
        overflow-wrap: anywhere;
        font-size: var(--mf-fs-xs);
        background: var(--mf-surface-2);
        padding: 2px 6px;
        border-radius: 4px;
        margin-right: 8px;
      }
      .mf-muted {
        color: var(--mf-text-muted);
      }
      .mf-field-hint,
      .mf-origin-summary {
        display: block;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-xs);
        overflow-wrap: anywhere;
      }
      .mf-origin-unrestricted {
        color: var(--mf-warn-text);
      }
      .mf-health-panel {
        display: flex;
        flex-wrap: wrap;
        gap: 16px;
        padding: 12px 16px;
        border-top: 1px solid var(--mf-border);
        background: var(--mf-surface-2);
      }
      .mf-health-summary {
        display: grid;
        gap: 4px;
        flex: 1 1 320px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-health-summary strong {
        color: var(--mf-text);
      }
      .mf-install-check {
        display: flex;
        flex: 1 1 420px;
        flex-wrap: wrap;
        align-items: center;
        justify-content: space-between;
        gap: 12px;
      }
      .mf-install-check ol {
        margin: 0;
        padding-left: 20px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-move-panel {
        display: flex;
        flex-wrap: wrap;
        align-items: end;
        gap: 12px;
        padding: 12px 16px;
        border-top: 1px solid var(--mf-border);
        background: var(--mf-surface-2);
      }
      .mf-origin-panel {
        display: flex;
        align-items: end;
        flex-wrap: wrap;
        gap: 12px;
        padding: 12px 16px;
        border-top: 1px solid var(--mf-border);
        background: var(--mf-surface-2);
      }
      .mf-property-panel {
        display: grid;
        gap: 12px;
        padding: 12px 16px;
        border-top: 1px solid var(--mf-border);
        background: var(--mf-surface-2);
      }
      .mf-property-heading,
      .mf-property-row,
      .mf-property-actions {
        display: flex;
        align-items: end;
        flex-wrap: wrap;
        gap: 12px;
      }
      .mf-property-heading {
        align-items: start;
        justify-content: space-between;
      }
      .mf-property-heading p {
        margin: 4px 0 0;
      }
      .mf-property-privacy {
        max-width: 760px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-property-row .mf-field {
        flex: 1 1 180px;
      }
      .mf-origin-field {
        flex: 1 1 420px;
      }
      .mf-origin-field textarea {
        resize: vertical;
      }
      .mf-origin-actions {
        display: flex;
        gap: 8px;
      }
      .mf-move-panel .mf-field {
        min-width: 220px;
      }
      .mf-move-confirm {
        flex: 1 1 320px;
        margin: 0;
        color: var(--mf-text-muted);
      }
      .mf-move-actions {
        display: flex;
        gap: 8px;
      }
    `,
  ],
})
export class AnalyticsSitesListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AnalyticsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  private createBtn = viewChild<ElementRef<HTMLButtonElement>>('createBtn');
  private statusCells = viewChildren<ElementRef<HTMLElement>>('statusCell');

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  sites = signal<TelemetryClient[]>([]);
  loading = signal(false);
  error = signal('');
  newName = '';
  newOrigin = '';
  creating = signal(false);
  movingSiteId = signal('');
  moveTargets = signal<TelemetryMoveTarget[]>([]);
  loadingTargets = signal(false);
  moving = signal(false);
  moveTargetId = '';
  verifyingSiteId = signal('');
  editingOriginsSiteId = signal('');
  originsDraft = '';
  savingOrigins = signal(false);
  editingPropertiesSiteId = signal('');
  propertyDrafts = signal<AnalyticsPropertyRuleInput[]>([]);
  loadingProperties = signal(false);
  savingProperties = signal(false);

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
    this.cancelMove();
    this.cancelOrigins();
    this.cancelProperties();
    this.verifyingSiteId.set('');
    this.businessId.set(id);
    this.current.set(id);
    this.reload();
  }

  startOrigins(c: TelemetryClient): void {
    if (this.savingOrigins()) return;
    this.cancelProperties();
    this.editingOriginsSiteId.set(c.id);
    this.originsDraft = c.allowed_origins.join('\n');
  }

  parsedOrigins(): string[] {
    return this.originsDraft
      .split(/[\n,]/)
      .map((value) => value.trim())
      .filter(Boolean);
  }

  cancelOrigins(): void {
    if (this.savingOrigins()) return;
    this.editingOriginsSiteId.set('');
    this.originsDraft = '';
  }

  saveOrigins(c: TelemetryClient): void {
    const origins = this.parsedOrigins();
    if (!origins.length || this.savingOrigins()) return;
    this.savingOrigins.set(true);
    this.api.setAllowedOrigins(this.businessId(), c.id, origins).subscribe({
      next: (updated) => {
        this.sites.update((sites) =>
          sites.map((site) =>
            site.id === c.id ? { ...site, allowed_origins: updated.allowed_origins } : site,
          ),
        );
        this.savingOrigins.set(false);
        this.editingOriginsSiteId.set('');
        this.originsDraft = '';
        this.toast.success('Allowed origins updated — the embed key is unchanged');
      },
      error: (e: HttpErrorResponse) => {
        this.savingOrigins.set(false);
        this.toast.error(
          e.status === 400
            ? 'Use 1–10 exact HTTPS origins with no path or wildcard'
            : 'Could not update allowed origins',
        );
      },
    });
  }

  startProperties(c: TelemetryClient): void {
    if (this.savingProperties()) return;
    this.cancelOrigins();
    this.cancelMove();
    this.editingPropertiesSiteId.set(c.id);
    this.propertyDrafts.set([]);
    this.loadingProperties.set(true);
    const businessId = this.businessId();
    this.api.propertyRules(businessId, c.id).subscribe({
      next: ({ rules }) => {
        if (this.editingPropertiesSiteId() !== c.id || this.businessId() !== businessId) return;
        this.propertyDrafts.set(
          (rules ?? []).map(({ event_name, property_key, label }) => ({
            event_name,
            property_key,
            label,
          })),
        );
        this.loadingProperties.set(false);
      },
      error: () => {
        this.loadingProperties.set(false);
        this.editingPropertiesSiteId.set('');
        this.toast.error('Could not load retained properties');
      },
    });
  }

  addProperty(): void {
    if (this.propertyDrafts().length >= 20) return;
    this.propertyDrafts.update((rules) => [
      ...rules,
      { event_name: '', property_key: '', label: '' },
    ]);
  }

  updateProperty(index: number, field: keyof AnalyticsPropertyRuleInput, value: string): void {
    this.propertyDrafts.update((rules) =>
      rules.map((rule, i) => (i === index ? { ...rule, [field]: value } : rule)),
    );
  }

  removeProperty(index: number): void {
    this.propertyDrafts.update((rules) => rules.filter((_, i) => i !== index));
  }

  propertyDraftsValid(): boolean {
    const rules = this.propertyDrafts();
    if (rules.length > 20) return false;
    return rules.every(
      (rule) => rule.event_name.trim() && rule.property_key.trim() && rule.label.trim(),
    );
  }

  saveProperties(c: TelemetryClient): void {
    if (!this.propertyDraftsValid() || this.savingProperties()) return;
    const rules = this.propertyDrafts().map((rule) => ({
      event_name: rule.event_name.trim(),
      property_key: rule.property_key.trim(),
      label: rule.label.trim(),
    }));
    this.savingProperties.set(true);
    this.api.replacePropertyRules(this.businessId(), c.id, rules).subscribe({
      next: ({ rules: saved }) => {
        this.savingProperties.set(false);
        this.editingPropertiesSiteId.set('');
        this.propertyDrafts.set([]);
        this.toast.success(
          saved.length
            ? `${saved.length} retained ${saved.length === 1 ? 'property' : 'properties'} saved`
            : 'Property retention cleared',
        );
      },
      error: (e: HttpErrorResponse) => {
        this.savingProperties.set(false);
        this.toast.error(
          e.status === 400
            ? 'Use unique event/key pairs and exclude sensitive data or persistent identifiers'
            : 'Could not save retained properties',
        );
      },
    });
  }

  cancelProperties(): void {
    if (this.savingProperties()) return;
    this.editingPropertiesSiteId.set('');
    this.propertyDrafts.set([]);
    this.loadingProperties.set(false);
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.listClients(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        // This screen is analytics-only; crash clients belong to a different surface.
        this.sites.set((r.clients ?? []).filter((c) => c.kind === 'analytics'));
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.sites.set([]);
        this.error.set('Could not load sites');
        this.loading.set(false);
      },
    });
  }

  embed(c: TelemetryClient): string {
    return this.api.embedSnippet(c.publishable_key);
  }

  copyEmbed(c: TelemetryClient): void {
    const tag = this.embed(c);
    navigator.clipboard?.writeText(tag).then(
      () => this.toast.success('Embed tag copied'),
      () => this.toast.error('Could not copy — select the tag and copy manually'),
    );
  }

  healthLabel(c: TelemetryClient): string {
    switch (c.status === 'revoked' ? 'revoked' : c.analytics_health?.status) {
      case 'healthy':
        return 'Healthy';
      case 'never_seen':
        return 'Never seen';
      case 'stale':
        return 'Stale';
      case 'revoked':
        return 'Revoked';
      default:
        return 'Checking';
    }
  }

  healthTone(c: TelemetryClient): Tone {
    switch (c.analytics_health?.status) {
      case 'healthy':
        return 'success';
      case 'never_seen':
      case 'stale':
        return 'warn';
      default:
        return 'neutral';
    }
  }

  healthMessage(c: TelemetryClient): string {
    switch (c.analytics_health?.status) {
      case 'healthy':
        return 'Data is arriving from this site.';
      case 'never_seen':
        return 'No accepted event yet. Complete the setup check.';
      case 'stale':
        return 'No event arrived in the last 24 hours. Verify the tag if the site has traffic.';
      case 'revoked':
        return 'This embed tag no longer collects data.';
      default:
        return 'Installation health is still processing. Try again shortly.';
    }
  }

  needsHealthAction(c: TelemetryClient): boolean {
    return c.analytics_health?.status !== 'healthy';
  }

  checkInstallation(c: TelemetryClient): void {
    if (this.verifyingSiteId()) return;
    const businessId = this.businessId();
    this.verifyingSiteId.set(c.id);
    this.api.listClients(businessId).subscribe({
      next: (r) => {
        if (this.businessId() !== businessId) return;
        const sites = (r.clients ?? []).filter((client) => client.kind === 'analytics');
        this.sites.set(sites);
        this.verifyingSiteId.set('');
        const status = sites.find((site) => site.id === c.id)?.analytics_health?.status;
        if (status === 'healthy') {
          this.toast.success('Installation verified — data is arriving');
          // The successful state removes the button that initiated the check. Move focus to the
          // persistent status cell after Angular renders the replacement so keyboard users do not
          // lose their place; the adjacent polite live region announces the updated health copy.
          setTimeout(() => {
            this.statusCells()
              .find((cell) => cell.nativeElement.dataset['siteId'] === c.id)
              ?.nativeElement.focus();
          });
        } else if (status === 'checking') {
          this.toast.error('Health processing is catching up — try again shortly');
        } else {
          this.toast.error('No recent event yet — visit the site, then check again');
        }
      },
      error: () => {
        this.verifyingSiteId.set('');
        this.toast.error('Could not check installation');
      },
    });
  }

  // A site is always created WITHOUT a signing secret. The mfs_ secret is server-to-server only
  // and would have to be embedded in a public web page to be used here, which would leak it to
  // every visitor — so the signed tier is deliberately not offered on this screen.
  create(): void {
    const name = this.newName.trim();
    const origin = this.newOrigin.trim();
    if (!name || !origin || this.creating()) return;
    this.creating.set(true);
    this.api
      .createClient(this.businessId(), {
        kind: 'analytics',
        name,
        require_signature: false,
        allowed_origins: [origin],
      })
      .subscribe({
        next: () => {
          this.newName = '';
          this.newOrigin = '';
          this.creating.set(false);
          this.toast.success('Site added — copy its embed tag');
          this.reload();
          this.createBtn()?.nativeElement.focus();
        },
        error: (e: HttpErrorResponse) => {
          this.creating.set(false);
          this.toast.error(
            e.status === 400
              ? 'Use a valid site name and exact HTTPS origin (HTTP is allowed for localhost)'
              : 'Could not add site',
          );
        },
      });
  }

  revoke(c: TelemetryClient): void {
    this.api.revokeClient(this.businessId(), c.id).subscribe({
      next: () => {
        this.toast.success('Site revoked — its embed tag will stop collecting');
        this.reload();
      },
      error: () => this.toast.error('Could not revoke site'),
    });
  }

  startMove(c: TelemetryClient): void {
    this.cancelOrigins();
    this.cancelProperties();
    this.movingSiteId.set(c.id);
    this.moveTargets.set([]);
    this.moveTargetId = '';
    this.loadingTargets.set(true);
    const sourceBusinessId = this.businessId();
    this.api.moveTargets(sourceBusinessId, c.id).subscribe({
      next: (r) => {
        if (this.movingSiteId() !== c.id || this.businessId() !== sourceBusinessId) return;
        this.moveTargets.set(r.targets ?? []);
        this.loadingTargets.set(false);
      },
      error: () => {
        if (this.movingSiteId() !== c.id) return;
        this.loadingTargets.set(false);
        this.cancelMove();
        this.toast.error('Could not load eligible businesses');
      },
    });
  }

  selectedTargetName(): string {
    return this.moveTargets().find((target) => target.id === this.moveTargetId)?.name ?? '';
  }

  cancelMove(): void {
    if (this.moving()) return;
    this.movingSiteId.set('');
    this.moveTargets.set([]);
    this.moveTargetId = '';
    this.loadingTargets.set(false);
  }

  confirmMove(c: TelemetryClient): void {
    const sourceBusinessId = this.businessId();
    const targetBusinessId = this.moveTargetId;
    if (!targetBusinessId || this.moving()) return;
    this.moving.set(true);
    this.api.moveClient(sourceBusinessId, c.id, targetBusinessId).subscribe({
      next: () => {
        this.moving.set(false);
        this.movingSiteId.set('');
        this.moveTargets.set([]);
        this.moveTargetId = '';
        // Switch to and reload the destination. This refreshes both affected lists in one visible
        // transition: the source row disappears and the unchanged site/link appears under target.
        this.businessId.set(targetBusinessId);
        this.current.set(targetBusinessId);
        this.toast.success('Site moved — its embed tag and analytics history are unchanged');
        this.reload();
      },
      error: (e: HttpErrorResponse) => {
        this.moving.set(false);
        this.toast.error(
          e.status === 409 ? 'That business cannot receive this site' : 'Could not move site',
        );
      },
    });
  }
}
