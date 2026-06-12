import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { BusinessService } from '../../core/business.service';
import { Connector, ConnectorsService } from '../../core/connectors.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { Tone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { ConnectorFormComponent } from './connector-form';

// Connectors management page: list external connectors for the selected business with their
// sync health, connect new ones, test/enable-disable/rotate/delete. Business selection mirrors
// the approvals queue (same /api/v1/businesses list, persisted via CurrentBusinessService).
// Mutations update the row in place; delete uses an inline type-free confirm panel that names
// the connector and shows how many tickets will be detached to native.
@Component({
  selector: 'app-connectors-list',
  imports: [FormsModule, DatePipe, PageHeader, StatusPill, EmptyState, Spinner, ConnectorFormComponent],
  template: `
    <div class="mf-card" data-testid="connectors-page">
      <mf-page-header title="Connectors" [subtitle]="items().length + ' connected'">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="connectors-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select id="biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Connect a system' }}
          </button>
        </div>
      </div>

      @if (showAdd() && businessId()) {
        <app-connector-form mode="create" [businessId]="businessId()"
                            (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }

      <div class="mf-table" data-testid="connectors-list">
        <div class="mf-tr mf-th">
          <span style="width:80px">Type</span>
          <span style="flex:1">Name</span>
          <span style="width:110px">Health</span>
          <span style="width:90px">Tickets</span>
          <span style="width:110px">Reconciled</span>
          <span style="width:280px"></span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="connector-row" [attr.data-connector-id]="c.id">
            <span style="width:80px;text-transform:capitalize">{{ c.type }}</span>
            <span style="flex:1" data-testid="connector-name">
              {{ c.display_name }}
              <span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ c.base_url }}</span>
            </span>
            <span style="width:110px" data-testid="connector-health">
              <mf-status-pill [tone]="healthTone(c.health.state)" [label]="healthLabel(c.health.state)" />
            </span>
            <span style="width:90px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.health.linked_ticket_count }}</span>
            <span style="width:110px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.last_reconciled_at ? (c.last_reconciled_at | date: 'short') : '—' }}</span>
            <span style="width:280px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (confirmDeleteId() === c.id) {
                <span class="mf-err" data-testid="connector-delete-confirm" style="font-size:var(--mf-fs-xs);align-self:center">
                  Delete {{ c.display_name }}? Detaches {{ c.health.linked_ticket_count }} ticket(s).
                </span>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-delete-no" (click)="confirmDeleteId.set('')">Cancel</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete-yes" (click)="remove(c)">Delete</button>
              } @else {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-test" (click)="test(c)">Test</button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-toggle" (click)="toggle(c)">
                  {{ c.status === 'enabled' ? 'Disable' : 'Enable' }}
                </button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-rotate" (click)="rotateId.set(rotateId() === c.id ? '' : c.id)">Rotate</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="connector-delete" (click)="confirmDeleteId.set(c.id)">Delete</button>
              }
            </span>
            @if (rotateId() === c.id) {
              <div style="flex:1 1 100%">
                <app-connector-form mode="rotate" [businessId]="businessId()" [connectorId]="c.id"
                                    (saved)="onRotated()" (cancelled)="rotateId.set('')" />
              </div>
            }
          </div>
        }
        @if (!items().length) {
          <mf-empty-state title="No connectors yet" data-testid="connectors-empty">
            Connect Jira or Zendesk to sync tickets with an external system.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="connectors-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class ConnectorsListComponent implements OnInit, OnDestroy {
  private bizApi = inject(BusinessService);
  private api = inject(ConnectorsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Connector[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  rotateId = signal<string>('');
  confirmDeleteId = signal<string>('');

  private timer: ReturnType<typeof setInterval> | undefined;

  healthTone(state: string): Tone {
    return state === 'healthy' ? 'success' : state === 'degraded' ? 'warn' : 'neutral';
  }
  healthLabel(state: string): string {
    return state.charAt(0).toUpperCase() + state.slice(1);
  }

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
    this.timer = setInterval(() => this.reload(), 20000);
  }

  ngOnDestroy(): void {
    clearInterval(this.timer);
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
    this.rotateId.set('');
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load connectors');
        this.loading.set(false);
      },
    });
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Connector added');
    this.reload();
  }

  onRotated(): void {
    this.rotateId.set('');
    this.toast.success('Credential rotated');
    this.reload();
  }

  test(c: Connector): void {
    this.api.test(this.businessId(), c.id).subscribe({
      next: (res) => (res.ok ? this.toast.success('Connection OK') : this.toast.error('Test failed: ' + res.detail)),
      error: () => this.toast.error('Test failed'),
    });
  }

  toggle(c: Connector): void {
    const status = c.status === 'enabled' ? 'disabled' : 'enabled';
    this.api.update(this.businessId(), c.id, { status }).subscribe({
      next: (updated) => {
        this.items.update((xs) => xs.map((x) => (x.id === updated.id ? updated : x)));
        this.api.degradedCount.set(this.items().filter((x) => x.health.state !== 'healthy').length);
        this.toast.success(status === 'enabled' ? 'Enabled' : 'Disabled');
      },
      error: (e: HttpErrorResponse) => this.toast.error(e.status === 404 ? 'Not found' : 'Update failed'),
    });
  }

  remove(c: Connector): void {
    this.api.remove(this.businessId(), c.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== c.id));
        this.api.degradedCount.set(this.items().filter((x) => x.health.state !== 'healthy').length);
        this.confirmDeleteId.set('');
        this.toast.success('Connector deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
