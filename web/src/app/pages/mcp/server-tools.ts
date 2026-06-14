import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import { DiscoveredTool, McpService, ToolEffect } from '../../core/mcp.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mcp-server-tools',
  imports: [FormsModule, PageHeader],
  template: `
    <div class="mf-card" data-testid="mcp-tools-page">
      <mf-page-header title="Tool policies" [subtitle]="serverId()"></mf-page-header>
      @if (!reachable()) {
        <p class="mf-err" data-testid="mcp-unreachable">Server unreachable — showing saved policies only. New tools can't be discovered until it's reachable.</p>
      }
      <div class="mf-table" data-testid="mcp-tools-list">
        <div class="mf-tr mf-th"><span style="flex:1">Tool</span><span style="width:200px">Effect</span></div>
        @for (t of tools(); track t.name) {
          <div class="mf-tr" data-testid="mcp-tool-row" [attr.data-tool-name]="t.name">
            <span style="flex:1">{{ t.name }}<span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ t.description }}</span></span>
            <span style="width:200px">
              <select class="mf-select" data-testid="mcp-tool-effect" [ngModel]="t.effect" (ngModelChange)="setEffect(t, $event)">
                <option value="external">External (default — queues)</option>
                <option value="reversible">Reversible (auto in Assist)</option>
                <option value="read">Safe / read (always auto)</option>
              </select>
            </span>
          </div>
        }
        @if (!tools().length) { <p data-testid="mcp-tools-empty" style="color:var(--mf-text-muted)">No tools.</p> }
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-tools-error">{{ error() }}</p> }
    </div>
  `,
})
export class McpServerToolsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(McpService);
  private toast = inject(ToastService);

  businessId = signal('');
  serverId = signal('');
  tools = signal<DiscoveredTool[]>([]);
  reachable = signal(true);
  error = signal('');

  ngOnInit(): void {
    this.businessId.set(this.route.snapshot.paramMap.get('businessId') ?? '');
    this.serverId.set(this.route.snapshot.paramMap.get('serverId') ?? '');
    this.load();
  }

  load(): void {
    this.api.discoverTools(this.businessId(), this.serverId()).subscribe({
      next: (r) => {
        this.reachable.set(r.reachable);
        this.tools.set(r.tools ?? []);
        this.error.set('');
      },
      error: (e: HttpErrorResponse) => this.error.set(e.status === 404 ? 'Server not found' : 'Could not load tools'),
    });
  }

  setEffect(t: DiscoveredTool, effect: ToolEffect): void {
    const prev = t.effect;
    if (effect === 'external') {
      this.api.clearPolicy(this.businessId(), this.serverId(), t.name).subscribe({
        next: () => {
          this.apply(t, 'external');
          this.toast.success('Reverted to default');
        },
        error: (e: HttpErrorResponse) => this.fail(t, prev, e),
      });
    } else {
      this.api.setPolicy(this.businessId(), this.serverId(), t.name, effect).subscribe({
        next: () => {
          this.apply(t, effect);
          this.toast.success('Policy saved');
        },
        error: (e: HttpErrorResponse) => this.fail(t, prev, e),
      });
    }
  }

  private apply(t: DiscoveredTool, effect: ToolEffect): void {
    this.tools.update((xs) => xs.map((x) => (x.name === t.name ? { ...x, effect } : x)));
  }
  private fail(t: DiscoveredTool, prev: ToolEffect, e: HttpErrorResponse): void {
    this.apply(t, prev); // revert the optimistic select
    this.toast.error(e.status === 403 || e.status === 404 ? "You don't have access" : 'Could not save policy');
  }
}
