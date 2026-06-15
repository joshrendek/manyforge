import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnInit, Output, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  Agent, AgentsService, CreateAgentBody, MCPServer, ModelDescriptor, ToolDescriptor, UpdateAgentBody,
} from '../../core/agents.service';
import { AIProvider } from '../../core/ai-credentials.service';

const SELF_HOST: AIProvider[] = ['ollama', 'vllm'];

// Agent create/edit form. Standalone, template-driven, mirrors connector-form/credential-form.
// On init it loads tools()/models()/mcpServers() to populate the pickers. Provider is immutable
// on edit (disabled select, omitted from the PATCH). Model is a provider-filtered dropdown for
// catalog providers (anthropic/openai) and a free-text input for self-host (ollama/vllm). Budget
// is shown in dollars and sent as cents.
@Component({
  selector: 'app-agent-form',
  imports: [FormsModule],
  template: `
    <form class="mf-card" style="display:flex;flex-direction:column;gap:14px" (ngSubmit)="submit()" data-testid="agent-form">
      <div class="mf-field">
        <label for="ag-name">Name</label>
        <input id="ag-name" class="mf-input" type="text" data-testid="agent-name" name="name" [(ngModel)]="name" />
      </div>

      <div class="mf-field">
        <label for="ag-provider">Provider</label>
        <select id="ag-provider" class="mf-select" data-testid="agent-provider" name="provider"
                [ngModel]="provider()" (ngModelChange)="onProviderChange($event)" [disabled]="mode === 'edit'">
          <option value="anthropic">Anthropic</option>
          <option value="openai">OpenAI</option>
          <option value="ollama">Ollama (self-host)</option>
          <option value="vllm">vLLM (self-host)</option>
        </select>
        @if (mode === 'edit') { <small class="mf-hint">Provider can't change after creation.</small> }
      </div>

      <div class="mf-field">
        <label for="ag-model">Model</label>
        @if (isSelfHost()) {
          <input id="ag-model" class="mf-input" type="text" data-testid="agent-model-text" name="model"
                 [(ngModel)]="model" placeholder="e.g. llama3.1:70b" />
        } @else {
          <select id="ag-model" class="mf-select" data-testid="agent-model-select" name="model" [(ngModel)]="model">
            <option value="" disabled>Choose a model…</option>
            @for (m of modelsForProvider(); track m.model_id) {
              <option [value]="m.model_id">{{ m.model_id }}</option>
            }
          </select>
        }
      </div>

      <div class="mf-field">
        <label for="ag-prompt">System prompt</label>
        <textarea id="ag-prompt" class="mf-input" rows="4" data-testid="agent-system-prompt" name="system_prompt"
                  [(ngModel)]="systemPrompt"></textarea>
      </div>

      <div class="mf-field">
        <span>Allowed tools</span>
        <div data-testid="agent-tools">
          @for (t of tools(); track t.name) {
            <label style="display:flex;align-items:center;gap:8px;cursor:pointer">
              <input type="checkbox" [attr.data-testid]="'agent-tool-' + t.name"
                     [checked]="selectedTools().has(t.name)" (change)="toggleTool(t.name)" />
              {{ t.name }} <span class="mf-hint">({{ t.effect }}{{ t.required_perm ? ', needs ' + t.required_perm : '' }})</span>
            </label>
          }
        </div>
      </div>

      <div class="mf-field">
        <label for="ag-autonomy">Autonomy mode</label>
        <select id="ag-autonomy" class="mf-select" data-testid="agent-autonomy" name="autonomy_mode"
                [ngModel]="autonomyMode()" (ngModelChange)="autonomyMode.set(+$event)">
          <option [value]="1">1 — Assist (auto safe writes, queue risky)</option>
          <option [value]="2">2 — Queue all writes</option>
          <option [value]="3">3 — Autonomous</option>
        </select>
      </div>

      <div class="mf-field">
        <label for="ag-budget">Monthly budget (USD)</label>
        <input id="ag-budget" class="mf-input" type="number" min="0" step="1" data-testid="agent-budget"
               name="budget" [(ngModel)]="budgetDollars" />
      </div>

      @if (mcpServers().length > 0) {
        <div class="mf-field">
          <span>MCP servers</span>
          <div data-testid="agent-mcp">
            @for (s of mcpServers(); track s.id) {
              <label style="display:flex;align-items:center;gap:8px;cursor:pointer">
                <input type="checkbox" [attr.data-testid]="'agent-mcp-' + s.id"
                       [checked]="selectedMcp().has(s.id)" (change)="toggleMcp(s.id)" />
                {{ s.name }}
              </label>
            }
          </div>
        </div>
      }

      <label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" data-testid="agent-enabled" name="enabled" [(ngModel)]="enabled" /> Enabled</label>
      <label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" data-testid="agent-retriage" name="retriage" [(ngModel)]="retriageOnReply" /> Re-triage when the user replies</label>

      @if (error()) { <p class="mf-err" data-testid="agent-form-error">{{ error() }}</p> }

      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="agent-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : (mode === 'create' ? 'Create agent' : 'Save') }}
        </button>
      </div>
    </form>
  `,
})
export class AgentFormComponent implements OnInit {
  @Input() businessId = '';
  @Input() mode: 'create' | 'edit' = 'create';
  @Input() agent: Agent | null = null;
  @Output() saved = new EventEmitter<Agent>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AgentsService);

  name = '';
  provider = signal<AIProvider>('anthropic');
  model = '';
  systemPrompt = '';
  autonomyMode = signal<number>(1);
  enabled = true;
  retriageOnReply = false;
  budgetDollars = 0;

  tools = signal<ToolDescriptor[]>([]);
  allModels = signal<ModelDescriptor[]>([]);
  mcpServers = signal<MCPServer[]>([]);
  selectedTools = signal<Set<string>>(new Set());
  selectedMcp = signal<Set<string>>(new Set());

  submitting = signal(false);
  error = signal('');

  modelsForProvider = computed(() => this.allModels().filter((m) => m.provider === this.provider()));
  isSelfHost = computed(() => SELF_HOST.includes(this.provider()));

  ngOnInit(): void {
    this.api.tools(this.businessId).subscribe({ next: (r) => this.tools.set(r.items ?? []), error: () => {} });
    this.api.models(this.businessId).subscribe({ next: (r) => this.allModels.set(r.items ?? []), error: () => {} });
    this.api.mcpServers(this.businessId).subscribe({ next: (r) => this.mcpServers.set(r.items ?? []), error: () => {} });

    if (this.mode === 'edit' && this.agent) {
      const a = this.agent;
      this.name = a.name;
      this.provider.set(a.provider);
      this.model = a.model;
      this.systemPrompt = a.system_prompt;
      this.autonomyMode.set(a.autonomy_mode);
      this.enabled = a.enabled;
      this.retriageOnReply = a.retriage_on_reply;
      this.budgetDollars = Math.round(a.monthly_budget_cents / 100);
      this.selectedTools.set(new Set(a.allowed_tools ?? []));
      this.selectedMcp.set(new Set(a.allowed_mcp_servers ?? []));
    }
  }

  onProviderChange(p: AIProvider): void {
    this.provider.set(p);
    this.model = ''; // reset model when provider changes (catalog list differs)
  }

  toggleTool(name: string): void {
    this.selectedTools.update((s) => {
      const next = new Set(s);
      next.has(name) ? next.delete(name) : next.add(name);
      return next;
    });
  }

  toggleMcp(id: string): void {
    this.selectedMcp.update((s) => {
      const next = new Set(s);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  }

  valid(): boolean {
    return this.name.trim().length > 0 && this.model.trim().length > 0;
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    const cents = Math.round((this.budgetDollars || 0) * 100);
    const obs =
      this.mode === 'edit' && this.agent
        ? this.api.update(this.businessId, this.agent.id, this.buildUpdate(cents))
        : this.api.create(this.businessId, this.buildCreate(cents));
    obs.subscribe({
      next: (a) => {
        this.submitting.set(false);
        this.saved.emit(a);
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  private buildCreate(cents: number): CreateAgentBody {
    return {
      name: this.name.trim(),
      provider: this.provider(),
      model: this.model.trim(),
      system_prompt: this.systemPrompt,
      allowed_tools: [...this.selectedTools()],
      autonomy_mode: this.autonomyMode(),
      enabled: this.enabled,
      monthly_budget_cents: cents,
      allowed_mcp_servers: [...this.selectedMcp()],
      retriage_on_reply: this.retriageOnReply,
    };
  }

  // Edit sends the full editable set (provider omitted — it's immutable).
  private buildUpdate(cents: number): UpdateAgentBody {
    return {
      name: this.name.trim(),
      model: this.model.trim(),
      system_prompt: this.systemPrompt,
      allowed_tools: [...this.selectedTools()],
      autonomy_mode: this.autonomyMode(),
      enabled: this.enabled,
      monthly_budget_cents: cents,
      allowed_mcp_servers: [...this.selectedMcp()],
      retriage_on_reply: this.retriageOnReply,
    };
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'An agent with that name already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
