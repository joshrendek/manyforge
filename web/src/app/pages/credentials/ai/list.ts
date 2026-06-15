import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService } from '../../../core/ai-credentials.service';
import { BusinessService } from '../../../core/business.service';
import { CurrentBusinessService } from '../../../core/current-business.service';
import { Business } from '../../../core/tree';
import { EmptyState } from '../../../ui/empty-state/empty-state';
import { PageHeader } from '../../../ui/page-header/page-header';
import { Spinner } from '../../../ui/spinner/spinner';
import { ToastService } from '../../../ui/toast/toast.service';
import { CredentialFormComponent } from './credential-form';

// AI credentials management page: list per-business provider keys for the agents that
// power this business, add new ones, delete with an inline confirm. Business selection
// mirrors the connectors page (same /api/v1/businesses list, persisted via
// CurrentBusinessService). No health poll — a stored credential has no changing state.
@Component({
  selector: 'app-ai-credentials-list',
  imports: [FormsModule, PageHeader, EmptyState, Spinner, CredentialFormComponent],
  template: `
    <div class="mf-card" data-testid="ai-credentials-page">
      <mf-page-header title="AI Credentials" subtitle="Per-business provider keys for your agents">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="credentials-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="cred-biz-select">Business</label>
          <select id="cred-biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Add credential' }}
          </button>
        </div>
      </div>

      @if (showAdd() && businessId()) {
        <app-credential-form [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }

      <div class="mf-table" data-testid="credentials-list">
        <div class="mf-tr mf-th">
          <span style="width:120px">Provider</span>
          <span style="flex:1">Default model</span>
          <span style="flex:1">Base URL</span>
          <span style="width:110px">Private URL</span>
          <span style="width:220px"></span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="credential-row" [attr.data-credential-id]="c.id">
            <span style="width:120px;text-transform:capitalize" data-testid="credential-provider">{{ c.provider }}</span>
            <span style="flex:1">{{ c.default_model }}</span>
            <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.base_url || '—' }}</span>
            <span style="width:110px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.allow_private_base_url ? 'allowed' : '—' }}</span>
            <span style="width:220px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (confirmDeleteId() === c.id) {
                <span class="mf-err" data-testid="credential-delete-confirm" style="font-size:var(--mf-fs-xs);align-self:center">
                  Delete {{ c.provider }} credential?
                </span>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-delete-no" (click)="confirmDeleteId.set('')">Cancel</button>
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="credential-delete-yes" (click)="remove(c)">Delete</button>
              } @else {
                <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="credential-delete" (click)="confirmDeleteId.set(c.id)">Delete</button>
              }
            </span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No credentials yet" data-testid="credentials-empty">
            Add one to let an agent call a provider.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="credentials-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class AICredentialsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AICredentialsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<AICredential[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
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

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
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
        this.error.set('Could not load credentials');
        this.loading.set(false);
      },
    });
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Credential added');
    this.reload();
  }

  remove(c: AICredential): void {
    this.api.remove(this.businessId(), c.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== c.id));
        this.confirmDeleteId.set('');
        this.toast.success('Credential deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
