import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService, AIProvider, UpdateAICredentialBody } from '../../../core/ai-credentials.service';
import { AgentsService, ModelDescriptor } from '../../../core/agents.service';
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
                  (click)="toggleAdd()" [disabled]="!businessId()">
            {{ showAdd() ? 'Close' : 'Add credential' }}
          </button>
        </div>
      </div>

      @if (showAdd() && businessId()) {
        <app-credential-form [businessId]="businessId()" [initialProvider]="reconnectProvider()"
                             (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }

      <div class="mf-table" data-testid="credentials-list">
        <div class="mf-tr mf-th">
          <span style="width:120px">Provider</span>
          <span style="flex:1">Default model</span>
          <span style="flex:1">Base URL</span>
          <span style="width:110px">Private URL</span>
          <span style="width:90px">Lanes</span>
          <span style="width:300px"></span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="credential-row" [attr.data-credential-id]="c.id">
            <span style="width:120px;text-transform:capitalize" data-testid="credential-provider">{{ c.provider }}
              @if (c.provider === 'openai_codex') {
                <span data-testid="codex-health"
                      [style.color]="c.connection_status === 'connected' ? 'var(--mf-ok, green)' : 'var(--mf-danger, crimson)'"
                      style="font-size:var(--mf-fs-xs);display:block">
                  {{ c.connection_status || 'unknown' }}@if (c.chatgpt_plan) { · {{ c.chatgpt_plan }} }
                </span>
              }
            </span>
            <span style="flex:1">{{ c.default_model }}</span>
            <span style="flex:1;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">
              @if (c.provider === 'openai_codex') { {{ c.oauth_access_expiry ? ('expires ' + c.oauth_access_expiry) : '—' }} }
              @else { {{ c.base_url || '—' }} }
            </span>
            <span style="width:110px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ c.allow_private_base_url ? 'allowed' : '—' }}</span>
            <span style="width:90px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="credential-lanes">{{ c.max_concurrent_lanes }}</span>
            <span style="width:300px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              @if (c.provider === 'openai_codex' && c.connection_status !== 'connected') {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="codex-reconnect"
                        [attr.aria-label]="'Reconnect ' + c.provider" (click)="reconnect()">Reconnect</button>
              }
              @if (editId() === c.id) {
                @if (c.provider === 'openai_codex') {
                  <select class="mf-select mf-input-sm" data-testid="credential-edit-model"
                          [(ngModel)]="editModel" name="editModel" style="width:150px" aria-label="Default model">
                    @for (m of codexModels(); track m.model_id) {
                      <option [value]="m.model_id">{{ m.model_id }}</option>
                    }
                  </select>
                } @else {
                  <input type="text" class="mf-input mf-input-sm" data-testid="credential-edit-model"
                         [(ngModel)]="editModel" name="editModel" style="width:130px" aria-label="Default model" />
                }
                <input type="number" min="1" max="16" class="mf-input mf-input-sm" data-testid="credential-edit-lanes"
                       [(ngModel)]="editLanes" name="editLanes" style="width:64px" aria-label="Max concurrent lanes" />
                <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-edit-save" (click)="saveEdit(c)">Save</button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-edit-cancel" (click)="editId.set('')">Cancel</button>
              } @else {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-edit"
                        [attr.aria-label]="'Edit ' + c.provider" (click)="startEdit(c)">Edit</button>
              }
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
  private agents = inject(AgentsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<AICredential[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  confirmDeleteId = signal<string>('');
  reconnectProvider = signal<AIProvider | null>(null);
  editId = signal<string>('');
  editModel = '';
  editLanes = 4;
  // Codex model catalog for the inline editor's <select> (openai_codex only); fetched lazily on Edit.
  codexModels = signal<ModelDescriptor[]>([]);

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

  toggleAdd(): void {
    if (this.showAdd()) {
      this.showAdd.set(false);
      return;
    }
    this.reconnectProvider.set(null);
    this.showAdd.set(true);
  }

  reconnect(): void {
    this.reconnectProvider.set('openai_codex');
    this.showAdd.set(true);
  }

  startEdit(c: AICredential): void {
    this.confirmDeleteId.set('');
    this.editModel = c.default_model;
    this.editLanes = c.max_concurrent_lanes ?? 4;
    this.editId.set(c.id);
    // openai_codex has a known catalog → offer a real picker instead of free text. Prefer the LIVE
    // per-account list (exact plan + client_version set); fall back to the static catalog when the
    // live fetch is empty or fails. Other providers keep the free-text input for now.
    if (c.provider === 'openai_codex') {
      this.codexModels.set([]);
      this.api.liveCodexModels(this.businessId()).subscribe({
        next: (r) => {
          const live = (r.items ?? []).filter((m) => m.provider === 'openai_codex');
          if (live.length) this.codexModels.set(live);
          else this.loadStaticCodexModels();
        },
        error: () => this.loadStaticCodexModels(),
      });
    }
  }

  // loadStaticCodexModels populates the editor picker from the static model_pricing catalog — the
  // fallback when the live per-account list is unavailable.
  private loadStaticCodexModels(): void {
    this.agents.models(this.businessId()).subscribe({
      next: (r) => this.codexModels.set((r.items ?? []).filter((m) => m.provider === 'openai_codex')),
      error: () => this.codexModels.set([]),
    });
  }

  saveEdit(c: AICredential): void {
    const body: UpdateAICredentialBody = {
      default_model: this.editModel.trim(),
      max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.editLanes) || 4))),
    };
    this.api.update(this.businessId(), c.id, body).subscribe({
      next: (updated) => {
        this.items.update((xs) => xs.map((x) => (x.id === c.id ? updated : x)));
        this.editId.set('');
        this.toast.success('Credential updated');
      },
      error: (e: HttpErrorResponse) => {
        this.toast.error(e.status === 404 ? 'Not found' : e.status === 400 ? 'Invalid values' : 'Update failed');
      },
    });
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
