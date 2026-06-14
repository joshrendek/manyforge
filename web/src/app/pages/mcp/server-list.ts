import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { McpService, MCPServer } from '../../core/mcp.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { ToastService } from '../../ui/toast/toast.service';
import { McpServerFormComponent } from './server-form';

@Component({
  selector: 'app-mcp-server-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, McpServerFormComponent],
  template: `
    <div class="mf-card" data-testid="mcp-page">
      <mf-page-header title="MCP servers" [subtitle]="items().length + ' configured'"></mf-page-header>
      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="mcp-biz">Business</label>
          <select id="mcp-biz" class="mf-select" data-testid="mcp-business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="mcp-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">{{ showAdd() ? 'Close' : 'Add server' }}</button>
        </div>
      </div>
      @if (showAdd() && businessId()) {
        <app-mcp-server-form mode="create" [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }
      <div class="mf-table" data-testid="mcp-list">
        <div class="mf-tr mf-th"><span style="flex:1">Name</span><span style="width:90px">Enabled</span><span style="width:260px"></span></div>
        @for (s of items(); track s.id) {
          <div class="mf-tr" data-testid="mcp-row" [attr.data-server-id]="s.id">
            <span style="flex:1" data-testid="mcp-name-cell">{{ s.name }}<span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ s.url }}</span></span>
            <span style="width:90px">{{ s.enabled ? 'Yes' : 'No' }}</span>
            <span style="width:260px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              <a class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-tools"
                 [routerLink]="['/mcp', businessId(), s.id]">Tools</a>
              <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-edit" (click)="editId.set(editId() === s.id ? '' : s.id)">Edit</button>
              <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="mcp-delete" (click)="remove(s)">Delete</button>
            </span>
            @if (editId() === s.id) {
              <div style="flex:1 1 100%"><app-mcp-server-form mode="edit" [businessId]="businessId()" [server]="s" (saved)="onEdited()" (cancelled)="editId.set('')" /></div>
            }
          </div>
        }
        @if (!items().length) { <mf-empty-state title="No MCP servers" data-testid="mcp-empty">Add an MCP server to expose its tools to agents.</mf-empty-state> }
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-error">{{ error() }}</p> }
    </div>
  `,
})
export class McpServerListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(McpService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<MCPServer[]>([]);
  error = signal('');
  showAdd = signal(false);
  editId = signal<string>('');

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
    this.editId.set('');
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.api.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() === biz) {
          this.items.set(r.items ?? []);
          this.error.set('');
        }
      },
      error: () => {
        if (this.businessId() === biz) {
          this.items.set([]);
          this.error.set('Could not load servers');
        }
      },
    });
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Server added');
    this.reload();
  }
  onEdited(): void {
    this.editId.set('');
    this.toast.success('Server updated');
    this.reload();
  }

  remove(s: MCPServer): void {
    this.api.remove(this.businessId(), s.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== s.id));
        this.toast.success('Server deleted');
      },
      error: (e: HttpErrorResponse) => this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed'),
    });
  }
}
