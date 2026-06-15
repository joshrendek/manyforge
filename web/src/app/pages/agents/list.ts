import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Agent, AgentsService } from '../../core/agents.service';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';
import { AgentFormComponent } from './agent-form';

const MODE_LABELS: Record<number, string> = { 1: 'Assist', 2: 'Queue writes', 3: 'Autonomous' };

// Agents management page: list per-business agents, add new ones, edit inline, delete with an
// inline confirm. Business selection mirrors the credentials page (same /api/v1/businesses list,
// persisted via CurrentBusinessService). No poll — an agent's stored config has no changing state.
@Component({
  selector: 'app-agents-list',
  imports: [FormsModule, PageHeader, EmptyState, Spinner, AgentFormComponent],
  template: `
    <div class="mf-card" data-testid="agents-page">
      <mf-page-header title="Agents" subtitle="Automated agents that act on your tickets">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="agents-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="ag-biz-select">Business</label>
          <select id="ag-biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="agent-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Add agent' }}
          </button>
        </div>
      </div>

      @if (showAdd() && businessId()) {
        <app-agent-form mode="create" [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }

      <div class="mf-table" data-testid="agents-list">
        <div class="mf-tr mf-th">
          <span style="flex:1">Name</span>
          <span style="flex:1">Model</span>
          <span style="width:130px">Autonomy</span>
          <span style="width:80px">Enabled</span>
          <span style="width:90px">Budget</span>
          <span style="width:220px"></span>
        </div>
        @for (a of items(); track a.id) {
          <div class="mf-tr" data-testid="agent-row" [attr.data-agent-id]="a.id">
            <span style="flex:1" data-testid="agent-name-cell">{{ a.name }}</span>
            <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ a.provider }} / {{ a.model }}</span>
            <span style="width:130px">{{ modeLabel(a.autonomy_mode) }}</span>
            <span style="width:80px">{{ a.enabled ? 'yes' : 'no' }}</span>
            <span style="width:90px">\${{ (a.monthly_budget_cents / 100).toFixed(0) }}</span>
            <span style="width:220px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (confirmDeleteId() === a.id) {
                <span class="mf-err" data-testid="agent-delete-confirm" style="font-size:var(--mf-fs-xs);align-self:center">
                  Delete {{ a.name }}?
                </span>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="agent-delete-no" (click)="confirmDeleteId.set('')">Cancel</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="agent-delete-yes" (click)="remove(a)">Delete</button>
              } @else {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="agent-edit" (click)="startEdit(a)">Edit</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="agent-delete" (click)="confirmDeleteId.set(a.id)">Delete</button>
              }
            </span>
          </div>
          @if (editId() === a.id) {
            <div class="mf-tr" data-testid="agent-edit-row">
              <app-agent-form mode="edit" [businessId]="businessId()" [agent]="a"
                              (saved)="onEdited()" (cancelled)="editId.set('')" />
            </div>
          }
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No agents yet" data-testid="agents-empty">
            Add one (you'll need an AI credential first).
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="agents-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class AgentsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AgentsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Agent[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  editId = signal<string>('');
  confirmDeleteId = signal<string>('');

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

  modeLabel(m: number): string {
    return MODE_LABELS[m] ?? String(m);
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
    this.editId.set('');
    this.showAdd.set(false);
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
        this.error.set('Could not load agents');
        this.loading.set(false);
      },
    });
  }

  startEdit(a: Agent): void {
    this.editId.set(a.id);
    this.showAdd.set(false);
    this.confirmDeleteId.set('');
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Agent created');
    this.reload();
  }

  onEdited(): void {
    this.editId.set('');
    this.toast.success('Agent updated');
    this.reload();
  }

  remove(a: Agent): void {
    this.api.remove(this.businessId(), a.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== a.id));
        this.confirmDeleteId.set('');
        this.toast.success('Agent deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
