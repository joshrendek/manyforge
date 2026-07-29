import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { Subscription, timer } from 'rxjs';
import { switchMap } from 'rxjs/operators';
import { BusinessService } from '../core/business.service';
import {
  TenantMergeDestination,
  TenantMergeFinding,
  TenantMergeOperation,
  TenantMergeService,
  TenantMergeSourceOptions,
} from '../core/tenant-merge.service';
import { Row, buildTree, flatten } from '../core/tree';
import { PageHeader } from '../ui/page-header/page-header';
import { StatusPill } from '../ui/status-pill/status-pill';

@Component({
  selector: 'app-tenant-merge',
  imports: [FormsModule, RouterLink, PageHeader, StatusPill],
  template: `
    <div class="mf-card merge-shell">
      <mf-page-header
        title="Move an entire master"
        subtitle="Merge one tenant security boundary beneath another company."
      >
        <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/dashboard" actions
          >Back to businesses</a
        >
      </mf-page-header>

      <div class="merge-danger" role="alert">
        <strong>This is a tenant merge, not an ordinary hierarchy move.</strong>
        <span>
          All source users and data enter the destination tenant boundary. Existing source grants
          remain scoped to their original business or subtree.
        </span>
      </div>

      @if (loading()) {
        <p class="mf-text-muted" data-testid="merge-loading">Loading merge details…</p>
      } @else if (error()) {
        <div class="mf-err" data-testid="merge-error">
          <p>{{ error() }}</p>
          @if (operationId) {
            <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="loadOperation()">
              Reload status
            </button>
          } @else {
            <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/dashboard">Return to dashboard</a>
          }
        </div>
      }

      @if (!loading() && !operationId && sourceOptions(); as source) {
        <section class="merge-step" data-testid="target-step">
          <div class="step-number">1</div>
          <div>
            <h2>Choose the destination company</h2>
            <p>
              <strong>{{ source.source_root_name }}</strong> and every business beneath it will
              become a child of the destination you select.
            </p>

            <div class="mf-field">
              <label for="destination-search">Search authorized destinations</label>
              <input
                id="destination-search"
                class="mf-input"
                type="search"
                [(ngModel)]="destinationSearch"
                placeholder="Company, master, or hierarchy path"
                data-testid="destination-search"
              />
            </div>

            <div class="destination-list" role="radiogroup" aria-label="Destination company">
              @for (destination of filteredDestinations(); track destination.id) {
                <button
                  type="button"
                  class="destination-option"
                  [class.is-selected]="selectedDestinationId() === destination.id"
                  [attr.aria-checked]="selectedDestinationId() === destination.id"
                  role="radio"
                  data-testid="destination-option"
                  (click)="selectedDestinationId.set(destination.id)"
                >
                  <span class="destination-name">{{ destination.name }}</span>
                  <span class="destination-root">
                    Master: {{ destination.tenant_root_name }}
                    @if (destination.is_tenant_root) {
                      · master root
                    }
                  </span>
                  <span class="destination-path">{{ destination.hierarchy_path }}</span>
                </button>
              } @empty {
                <p class="mf-text-muted empty-search">
                  No authorized active destinations match that search.
                </p>
              }
            </div>

            <div class="merge-actions">
              <button
                class="mf-btn mf-btn-primary"
                type="button"
                [disabled]="!selectedDestinationId() || submitting()"
                data-testid="review-merge"
                (click)="beginReview()"
              >
                {{ submitting() ? 'Running preflight…' : 'Review impact' }}
              </button>
            </div>
          </div>
        </section>
      }

      @if (!loading() && operation(); as merge) {
        @if (merge.status === 'running' || cutoverPending()) {
          <section class="merge-state running-state" data-testid="merge-running" aria-live="polite">
            <mf-status-pill tone="warn" label="running" />
            <div>
              <h2>Secure cutover is in progress</h2>
              <p>
                Writes and background workers are fenced for both tenants. You may close this page;
                reopening this URL resumes durable status monitoring.
              </p>
              <p class="mf-text-muted">
                If this status does not advance, operator intervention is required. Do not start
                another merge.
              </p>
              <code>{{ merge.correlation_id }}</code>
            </div>
          </section>
        } @else if (merge.status === 'succeeded') {
          <section class="merge-state success-state" data-testid="merge-success">
            <mf-status-pill tone="success" label="succeeded" />
            <div>
              <h2>Master moved successfully</h2>
              <p>
                The hierarchy has been reloaded below. The former master is now a child inside the
                destination tenant boundary.
              </p>
              <code>{{ merge.correlation_id }}</code>
            </div>
          </section>

          @if (resultRows().length > 0) {
            <section class="result-tree" data-testid="result-hierarchy">
              <h3>Current destination hierarchy</h3>
              @for (row of resultRows(); track row.business.id) {
                <div
                  class="result-node"
                  [style.paddingLeft.px]="row.depth * 22"
                  [attr.data-business-id]="row.business.id"
                >
                  <span>{{ row.business.name }}</span>
                  @if (row.business.is_tenant_root) {
                    <mf-status-pill tone="accent" label="master" />
                  }
                </div>
              }
            </section>
          }
          <a class="mf-btn mf-btn-primary" routerLink="/dashboard">View businesses</a>
        } @else if (merge.status === 'failed') {
          <section class="merge-state failed-state" data-testid="merge-failed">
            <mf-status-pill tone="danger" label="failed" />
            <div>
              <h2>The cutover failed and rolled back safely</h2>
              <p>
                No partial tenant move was committed. Do not repeat the confirmation from this page.
                Contact an operator to review the failure before any new attempt.
              </p>
              <p>
                Stage: <strong>{{ merge.failure?.stage || 'unknown' }}</strong
                ><br />
                Operator correlation ID:
                <code>{{ merge.failure?.operator_correlation_id || merge.correlation_id }}</code>
              </p>
            </div>
          </section>
          <a class="mf-btn mf-btn-ghost" routerLink="/dashboard">Return to businesses</a>
        } @else {
          <section class="merge-review" data-testid="merge-review">
            <div class="review-heading">
              <div>
                <p class="eyebrow">Destination</p>
                <h2>{{ sourceName() || merge.source_root_id }} → {{ destinationPath() }}</h2>
                <p class="mf-text-muted">
                  Result: {{ destinationPath() }} / {{ sourceName() || 'source master' }}
                </p>
              </div>
              <mf-status-pill
                [tone]="merge.status === 'ready' ? 'success' : 'warn'"
                [label]="merge.status === 'ready' ? 'preflight current' : 'review required'"
              />
            </div>

            <div class="impact-grid">
              <div>
                <span>Affected rows</span><strong>{{ formatNumber(merge.affected_rows) }}</strong>
              </div>
              <div>
                <span>Estimated data</span><strong>{{ formatBytes(merge.estimated_bytes) }}</strong>
              </div>
              <div>
                <span>Businesses moved</span><strong>{{ merge.source_businesses ?? '—' }}</strong>
              </div>
              <div>
                <span>Resulting depth</span><strong>{{ merge.resulting_depth ?? '—' }}</strong>
              </div>
            </div>

            <section class="tree-preview" data-testid="resulting-tree-preview">
              <h3>Resulting parent and tree</h3>
              <div class="preview-parent">{{ destinationPath() }}</div>
              @for (row of reviewRows(); track row.business.id) {
                <div
                  class="preview-node"
                  [style.paddingLeft.px]="(row.depth + 1) * 22"
                  [attr.data-business-id]="row.business.id"
                >
                  <span>{{ row.business.name }}</span>
                  @if (row.business.id === merge.source_root_id) {
                    <mf-status-pill tone="warn" label="former master" />
                  }
                </div>
              } @empty {
                <p class="mf-text-muted">The source subtree contains the master shown above.</p>
              }
            </section>

            <section>
              <h3>Per-module impact</h3>
              <div class="mf-table impact-table" role="table" aria-label="Per-module impact">
                @for (module of moduleRows(); track module.name) {
                  <div class="mf-tr" role="row">
                    <span role="cell">{{ module.name }}</span>
                    <span role="cell">{{ formatNumber(module.rows) }} rows</span>
                    <span role="cell">{{ formatBytes(module.bytes) }}</span>
                  </div>
                } @empty {
                  <div class="mf-tr"><span>No module-owned rows found.</span></div>
                }
              </div>
            </section>

            <section class="consequences">
              <h3>Access and maintenance consequences</h3>
              <ul>
                <li>
                  All source users and data enter the destination tenant boundary, so destination
                  Owners gain access to the moved data.
                </li>
                <li>
                  Existing source grants remain subtree-scoped; source Owners do not gain access to
                  destination ancestors or siblings.
                </li>
                <li>
                  Both tenants enter maintenance during cutover. Writes, ingest, connectors, agents,
                  support mail, telemetry, and workers are paused or safely retried.
                </li>
                <li>
                  The database move is all-or-nothing. Attachments are staged before the fenced
                  transaction and cleaned up after success.
                </li>
              </ul>
            </section>

            @if (merge.warnings.length > 0) {
              <section class="finding-list warning-list" data-testid="merge-warnings">
                <h3>Warnings</h3>
                @for (finding of merge.warnings; track finding.code) {
                  <div class="finding">
                    <strong>{{ findingTitle(finding) }}</strong>
                    <span>{{ findingDetail(finding) }}</span>
                  </div>
                }
              </section>
            }

            @if (merge.conflicts.length > 0) {
              <section class="finding-list blocker-list" data-testid="merge-blockers">
                <h3>Blocking conflicts</h3>
                <p>
                  Resolve every item in its source or destination module, then run preflight again.
                  Nothing can start while a blocker remains.
                </p>
                @for (finding of merge.conflicts; track finding.code) {
                  <div class="finding">
                    <strong>{{ findingTitle(finding) }}</strong>
                    <span>{{ findingDetail(finding) }}</span>
                  </div>
                }
              </section>
            }

            @if (merge.status === 'preflight_required' || merge.conflicts.length > 0) {
              <div class="merge-actions">
                <button
                  class="mf-btn mf-btn-primary"
                  type="button"
                  [disabled]="submitting()"
                  data-testid="rerun-preflight"
                  (click)="rerunPreflight()"
                >
                  {{ submitting() ? 'Checking…' : 'Run preflight again' }}
                </button>
              </div>
            }

            <section class="confirmation-card">
              <h3>Fresh authentication and exact confirmation</h3>
              <p>
                Enter your current password and type both names exactly. The backend rechecks
                ownership, authorization, and the complete preflight generation immediately before
                cutover.
              </p>
              <div class="confirmation-grid">
                <div class="mf-field">
                  <label for="confirm-source">Type “{{ sourceName() }}”</label>
                  <input
                    id="confirm-source"
                    class="mf-input"
                    autocomplete="off"
                    [(ngModel)]="typedSourceName"
                    data-testid="confirm-source"
                  />
                </div>
                <div class="mf-field">
                  <label for="confirm-destination">Type “{{ destinationName() }}”</label>
                  <input
                    id="confirm-destination"
                    class="mf-input"
                    autocomplete="off"
                    [(ngModel)]="typedDestinationName"
                    data-testid="confirm-destination"
                  />
                </div>
                <div class="mf-field">
                  <label for="confirm-password">Current password</label>
                  <input
                    id="confirm-password"
                    class="mf-input"
                    type="password"
                    autocomplete="current-password"
                    [(ngModel)]="password"
                    data-testid="confirm-password"
                  />
                </div>
              </div>
              @if (confirmationError()) {
                <p class="mf-err" data-testid="confirmation-error">{{ confirmationError() }}</p>
              }
              <button
                class="mf-btn mf-btn-danger"
                type="button"
                [disabled]="!canConfirm()"
                data-testid="start-merge"
                (click)="confirmAndStart()"
              >
                {{ submitting() ? 'Authenticating and starting…' : 'Authenticate and start merge' }}
              </button>
            </section>

            <p class="stable-url mf-text-muted">
              Durable operation URL: <code>/tenant-merges/{{ merge.id }}</code>
            </p>
          </section>
        }
      }
    </div>
  `,
  styles: `
    .merge-shell {
      max-width: 980px;
      margin: 0 auto;
    }
    .merge-danger {
      display: grid;
      gap: var(--mf-space-2);
      padding: var(--mf-space-4);
      margin-bottom: var(--mf-space-5);
      border: 1px solid color-mix(in srgb, var(--mf-danger) 45%, var(--mf-border));
      border-left: 4px solid var(--mf-danger);
      border-radius: var(--mf-radius);
      background: color-mix(in srgb, var(--mf-danger) 8%, var(--mf-surface));
    }
    .merge-step {
      display: grid;
      grid-template-columns: 44px 1fr;
      gap: var(--mf-space-4);
    }
    .step-number {
      display: grid;
      place-items: center;
      width: 36px;
      height: 36px;
      border-radius: 50%;
      background: var(--mf-accent);
      color: var(--mf-text-on-accent);
      font-weight: 700;
    }
    h2,
    h3 {
      margin-top: 0;
    }
    .destination-list {
      display: grid;
      gap: var(--mf-space-2);
      margin-top: var(--mf-space-3);
    }
    .destination-option {
      display: grid;
      gap: 3px;
      width: 100%;
      padding: var(--mf-space-3);
      text-align: left;
      color: var(--mf-text);
      background: var(--mf-surface);
      border: 1px solid var(--mf-border);
      border-radius: var(--mf-radius);
      cursor: pointer;
    }
    .destination-option:hover,
    .destination-option.is-selected {
      border-color: var(--mf-accent);
      box-shadow: 0 0 0 2px color-mix(in srgb, var(--mf-accent) 18%, transparent);
    }
    .destination-name {
      font-weight: 700;
    }
    .destination-root,
    .destination-path {
      color: var(--mf-text-muted);
      font-size: 0.875rem;
    }
    .empty-search {
      padding: var(--mf-space-4);
      border: 1px dashed var(--mf-border);
    }
    .merge-actions {
      display: flex;
      gap: var(--mf-space-2);
      margin-top: var(--mf-space-4);
    }
    .merge-state {
      display: flex;
      align-items: flex-start;
      gap: var(--mf-space-4);
      padding: var(--mf-space-5);
      margin-bottom: var(--mf-space-5);
      border: 1px solid var(--mf-border);
      border-radius: var(--mf-radius);
    }
    .success-state {
      border-color: color-mix(in srgb, var(--mf-success) 50%, var(--mf-border));
    }
    .failed-state {
      border-color: color-mix(in srgb, var(--mf-danger) 50%, var(--mf-border));
    }
    .running-state {
      border-color: color-mix(in srgb, var(--mf-warn) 50%, var(--mf-border));
    }
    .review-heading {
      display: flex;
      justify-content: space-between;
      gap: var(--mf-space-4);
      align-items: flex-start;
    }
    .eyebrow {
      margin: 0 0 var(--mf-space-1);
      color: var(--mf-text-muted);
      font-size: 0.75rem;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }
    .impact-grid {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: var(--mf-space-3);
      margin: var(--mf-space-5) 0;
    }
    .impact-grid > div {
      display: grid;
      gap: var(--mf-space-1);
      padding: var(--mf-space-3);
      border: 1px solid var(--mf-border);
      border-radius: var(--mf-radius);
    }
    .impact-grid span {
      color: var(--mf-text-muted);
      font-size: 0.8rem;
    }
    .impact-grid strong {
      font-size: 1.25rem;
    }
    .impact-table .mf-tr {
      display: grid;
      grid-template-columns: 1fr auto auto;
      gap: var(--mf-space-4);
    }
    .consequences,
    .finding-list,
    .confirmation-card,
    .tree-preview,
    .result-tree {
      margin-top: var(--mf-space-5);
      padding: var(--mf-space-4);
      border: 1px solid var(--mf-border);
      border-radius: var(--mf-radius);
    }
    .consequences li + li {
      margin-top: var(--mf-space-2);
    }
    .warning-list {
      border-left: 4px solid var(--mf-warn);
    }
    .blocker-list {
      border-left: 4px solid var(--mf-danger);
    }
    .finding {
      display: grid;
      gap: 2px;
      padding: var(--mf-space-2) 0;
    }
    .finding + .finding {
      border-top: 1px solid var(--mf-border);
    }
    .finding span {
      color: var(--mf-text-muted);
      font-size: 0.875rem;
    }
    .confirmation-card {
      background: color-mix(in srgb, var(--mf-danger) 4%, var(--mf-surface));
    }
    .confirmation-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: var(--mf-space-3);
    }
    .confirmation-grid .mf-field:last-child {
      grid-column: 1 / -1;
    }
    .stable-url {
      margin-top: var(--mf-space-4);
    }
    .result-node {
      display: flex;
      align-items: center;
      gap: var(--mf-space-2);
      min-height: 34px;
    }
    .preview-parent {
      min-height: 34px;
      font-weight: 700;
    }
    .preview-node {
      display: flex;
      align-items: center;
      gap: var(--mf-space-2);
      min-height: 32px;
      border-left: 2px solid var(--mf-accent-soft);
    }
    code {
      overflow-wrap: anywhere;
    }
    @media (max-width: 720px) {
      .merge-step {
        grid-template-columns: 1fr;
      }
      .step-number {
        display: none;
      }
      .impact-grid,
      .confirmation-grid {
        grid-template-columns: 1fr;
      }
      .confirmation-grid .mf-field:last-child {
        grid-column: auto;
      }
      .review-heading {
        display: grid;
      }
    }
  `,
})
export class TenantMergeComponent implements OnInit, OnDestroy {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private api = inject(TenantMergeService);
  private businesses = inject(BusinessService);
  private polling?: Subscription;

  readonly sourceRootId = this.route.snapshot.paramMap.get('sourceRootId');
  readonly operationId = this.route.snapshot.paramMap.get('operationId');

  loading = signal(true);
  error = signal('');
  submitting = signal(false);
  cutoverPending = signal(false);
  confirmationError = signal('');
  sourceOptions = signal<TenantMergeSourceOptions | null>(null);
  selectedDestinationId = signal('');
  operation = signal<TenantMergeOperation | null>(null);
  sourceName = signal('');
  destinationName = signal('');
  destinationPath = signal('');
  reviewRows = signal<Row[]>([]);
  resultRows = signal<Row[]>([]);

  destinationSearch = '';
  typedSourceName = '';
  typedDestinationName = '';
  password = '';

  filteredDestinations(): TenantMergeDestination[] {
    const query = this.destinationSearch.trim().toLocaleLowerCase();
    const destinations = this.sourceOptions()?.destinations ?? [];
    if (!query) return destinations;
    return destinations.filter((destination) =>
      [destination.name, destination.tenant_root_name, destination.hierarchy_path].some((value) =>
        value.toLocaleLowerCase().includes(query),
      ),
    );
  }

  moduleRows = computed(() =>
    Object.entries(this.operation()?.module_counts ?? {})
      .map(([name, count]) => ({ name, rows: count.rows, bytes: count.bytes }))
      .sort((left, right) => left.name.localeCompare(right.name)),
  );

  canConfirm(): boolean {
    const operation = this.operation();
    return (
      operation?.status === 'ready' &&
      !!operation.preflight_generation &&
      operation.conflicts.length === 0 &&
      this.sourceName().length > 0 &&
      this.destinationName().length > 0 &&
      this.typedSourceName === this.sourceName() &&
      this.typedDestinationName === this.destinationName() &&
      this.password.length > 0 &&
      !this.submitting()
    );
  }

  ngOnInit(): void {
    if (this.operationId) {
      this.loadOperation();
      return;
    }
    this.loadOptions();
  }

  ngOnDestroy(): void {
    this.stopPolling();
  }

  private loadOptions(): void {
    if (!this.sourceRootId) {
      this.loading.set(false);
      this.error.set('This merge source is invalid.');
      return;
    }
    this.api.options().subscribe({
      next: (response) => {
        const source = (response.sources ?? []).find(
          (item) => item.source_root_id === this.sourceRootId,
        );
        this.loading.set(false);
        if (!source || source.destinations.length === 0) {
          this.error.set(
            'This master is not eligible to move, or no authorized active destination is available.',
          );
          return;
        }
        this.sourceOptions.set(source);
        this.sourceName.set(source.source_root_name);
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load authorized merge destinations.');
      },
    });
  }

  beginReview(): void {
    const source = this.sourceOptions();
    const destination = source?.destinations.find(
      (item) => item.id === this.selectedDestinationId(),
    );
    if (!source || !destination) return;

    this.submitting.set(true);
    this.error.set('');
    this.api
      .create(
        source.source_root_id,
        destination.id,
        this.idempotencyKey(source.source_root_id, destination.id),
      )
      .subscribe({
        next: (operation) => {
          this.submitting.set(false);
          void this.router.navigateByUrl(`/tenant-merges/${operation.id}`);
        },
        error: (error: HttpErrorResponse) => {
          this.submitting.set(false);
          this.error.set(this.describeError(error, 'preflight'));
        },
      });
  }

  loadOperation(): void {
    if (!this.operationId) return;
    this.stopPolling();
    this.loading.set(true);
    this.error.set('');
    this.api.get(this.operationId).subscribe({
      next: (operation) => {
        this.loading.set(false);
        this.acceptOperation(operation);
      },
      error: (error: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(this.describeError(error, 'status'));
      },
    });
  }

  rerunPreflight(): void {
    const operation = this.operation();
    if (!operation) return;
    this.submitting.set(true);
    this.error.set('');
    this.confirmationError.set('');
    this.api.preflight(operation.id).subscribe({
      next: (current) => {
        this.submitting.set(false);
        this.acceptOperation(current);
      },
      error: (error: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describeError(error, 'preflight'));
      },
    });
  }

  confirmAndStart(): void {
    const operation = this.operation();
    if (!operation || !this.canConfirm()) return;
    this.submitting.set(true);
    this.cutoverPending.set(true);
    this.confirmationError.set('');
    this.error.set('');
    this.startPolling(operation.id, 750);
    this.api
      .confirm(operation.id, this.typedSourceName, this.typedDestinationName, this.password)
      .subscribe({
        next: (current) => {
          this.password = '';
          this.submitting.set(false);
          this.cutoverPending.set(false);
          this.acceptOperation(current);
        },
        error: (error: HttpErrorResponse) => {
          this.password = '';
          this.submitting.set(false);
          this.cutoverPending.set(false);
          this.stopPolling();
          this.confirmationError.set(this.describeError(error, 'confirmation'));
          if (error.status === 412) this.loadOperation();
        },
      });
  }

  private acceptOperation(operation: TenantMergeOperation): void {
    operation.conflicts ??= [];
    operation.warnings ??= [];
    operation.module_counts ??= {};
    this.operation.set(operation);
    if (!this.sourceName() || !this.destinationName()) {
      this.loadBusinessLabels(operation);
    }
    if (operation.status === 'running') {
      this.startPolling(operation.id);
    } else {
      this.stopPolling();
    }
    if (operation.status === 'succeeded') {
      this.loadResultHierarchy(operation.destination_root_id);
    } else if (
      (operation.status === 'ready' || operation.status === 'preflight_required') &&
      this.reviewRows().length === 0
    ) {
      this.loadReviewHierarchy(operation.source_root_id);
    }
  }

  private loadBusinessLabels(operation: TenantMergeOperation): void {
    this.businesses.get(operation.source_root_id).subscribe({
      next: (source) => this.sourceName.set(source.name),
      error: () => {},
    });
    this.businesses.get(operation.destination_parent_id).subscribe({
      next: (destination) => {
        this.destinationName.set(destination.name);
        this.destinationPath.set(destination.name);
      },
      error: () => {},
    });
    this.api.options().subscribe({
      next: (response) => {
        for (const source of response.sources ?? []) {
          const destination = source.destinations.find(
            (item) => item.id === operation.destination_parent_id,
          );
          if (destination) {
            this.destinationName.set(destination.name);
            this.destinationPath.set(destination.hierarchy_path);
            return;
          }
        }
      },
      error: () => {},
    });
  }

  private loadResultHierarchy(destinationRootId: string): void {
    this.businesses.list().subscribe({
      next: (response) => {
        const destination = (response.items ?? []).filter(
          (business) => business.tenant_root_id === destinationRootId,
        );
        this.resultRows.set(flatten(buildTree(destination), new Set()));
      },
      error: () => this.resultRows.set([]),
    });
  }

  private loadReviewHierarchy(sourceRootId: string): void {
    this.businesses.list().subscribe({
      next: (response) => {
        const source = (response.items ?? []).filter(
          (business) => business.tenant_root_id === sourceRootId,
        );
        this.reviewRows.set(flatten(buildTree(source), new Set()));
      },
      error: () => this.reviewRows.set([]),
    });
  }

  private startPolling(operationId: string, initialDelay = 1500): void {
    if (this.polling) return;
    this.polling = timer(initialDelay, 1500)
      .pipe(switchMap(() => this.api.get(operationId)))
      .subscribe({
        next: (operation) => this.acceptOperation(operation),
        // A transient poll failure must not erase the durable operation view.
        error: () => {
          this.stopPolling();
          this.error.set('Status monitoring paused. Reload this URL to resume.');
        },
      });
  }

  private stopPolling(): void {
    this.polling?.unsubscribe();
    this.polling = undefined;
  }

  private idempotencyKey(sourceRootId: string, destinationParentId: string): string {
    const storageKey = `mf-tenant-merge:${sourceRootId}:${destinationParentId}`;
    const existing = sessionStorage.getItem(storageKey);
    if (existing) return existing;
    const random =
      globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`;
    const value = `dashboard-${random}`;
    sessionStorage.setItem(storageKey, value);
    return value;
  }

  findingTitle(finding: TenantMergeFinding): string {
    return finding.code
      .split('_')
      .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
      .join(' ');
  }

  findingDetail(finding: TenantMergeFinding): string {
    const parts = [
      `${finding.module} · ${finding.object}`,
      `${this.formatNumber(finding.count)} affected`,
    ];
    if (finding.limit != null) {
      parts.push(`limit ${this.formatNumber(finding.limit)}`);
    }
    if (finding.bytes != null) parts.push(this.formatBytes(finding.bytes));
    return parts.join(' · ');
  }

  formatNumber(value: number): string {
    return new Intl.NumberFormat().format(value ?? 0);
  }

  formatBytes(value: number): string {
    if (!value) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
    const amount = value / 1024 ** index;
    return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
  }

  private describeError(error: HttpErrorResponse, action: string): string {
    if (error.status === 401 && action === 'confirmation') {
      return 'Fresh authentication failed. Check your current password and try again.';
    }
    if (error.status === 412) {
      return 'The preflight became stale. Review a fresh preflight before confirming.';
    }
    if (error.status === 404) {
      return 'This merge is unavailable or you no longer have the required direct ownership.';
    }
    if (error.status === 409) {
      return 'The merge conflicts with newer tenant state. Reload and review before continuing.';
    }
    if (error.status === 429) {
      return 'Too many merge requests. Wait before trying again.';
    }
    if (error.status === 503) {
      return 'A tenant is already in maintenance. Reload the durable status before taking action.';
    }
    return `Could not ${action} this tenant merge.`;
  }
}
