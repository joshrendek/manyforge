import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnInit, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { McpService, MCPServer } from '../../core/mcp.service';

@Component({
  selector: 'app-mcp-server-form',
  imports: [FormsModule],
  template: `
    <form class="mf-add-form" data-testid="mcp-server-form" (ngSubmit)="submit()">
      <div class="mf-field" style="flex:1 1 180px">
        <label for="mcp-name">Name</label>
        <input id="mcp-name" type="text" class="mf-input" data-testid="mcp-name"
               [(ngModel)]="name" name="name" autocomplete="off" [disabled]="submitting()" />
        <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">No colons (used in tool ids).</span>
      </div>
      <div class="mf-field" style="flex:1 1 260px">
        <label for="mcp-url">URL</label>
        <input id="mcp-url" type="url" class="mf-input" data-testid="mcp-url"
               placeholder="https://mcp.example.com" [(ngModel)]="url" name="url" autocomplete="off" [disabled]="submitting()" />
      </div>
      <div class="mf-field" style="flex:1 1 200px">
        <label for="mcp-token">Auth token{{ mode === 'edit' ? ' (leave blank to keep)' : '' }}</label>
        <input id="mcp-token" type="password" class="mf-input" data-testid="mcp-token"
               placeholder="••••••••" [(ngModel)]="authToken" name="auth_token" autocomplete="off" [disabled]="submitting()" />
        <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">Never shown again.</span>
      </div>
      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="mcp-form-submit"
                [disabled]="submitting() || !valid()">{{ submitting() ? 'Saving…' : (mode === 'create' ? 'Add server' : 'Save') }}</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-form-cancel"
                (click)="cancelled.emit()" [disabled]="submitting()">Cancel</button>
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-form-error" style="flex:1 1 100%">{{ error() }}</p> }
    </form>
  `,
})
export class McpServerFormComponent implements OnInit {
  @Input() businessId = '';
  @Input() mode: 'create' | 'edit' = 'create';
  @Input() server: MCPServer | null = null;
  @Output() saved = new EventEmitter<MCPServer>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(McpService);
  name = '';
  url = '';
  authToken = '';
  submitting = signal(false);
  error = signal('');

  ngOnInit(): void {
    if (this.mode === 'edit' && this.server) {
      this.name = this.server.name;
      this.url = this.server.url;
      // authToken intentionally NOT prefilled (write-only).
    }
  }

  valid(): boolean {
    return !!this.name.trim() && !!this.url.trim() && !this.name.includes(':');
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    const obs =
      this.mode === 'create'
        ? this.api.create(this.businessId, {
            name: this.name.trim(),
            url: this.url.trim(),
            auth_token: this.authToken || undefined,
          })
        : this.api.update(this.businessId, this.server!.id, {
            name: this.name.trim(),
            url: this.url.trim(),
            auth_token: this.authToken || undefined, // omitted when blank → keep current
          });
    obs.subscribe({
      next: (s) => {
        this.submitting.set(false);
        this.authToken = '';
        this.saved.emit(s);
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values.';
    }
    if (e.status === 409) return 'A server with that name already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
