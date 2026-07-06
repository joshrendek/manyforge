import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { HttpErrorResponse } from '@angular/common/http';
import { Agent, AgentsService, ModelDescriptor } from '../../core/agents.service';
import { BusinessService } from '../../core/business.service';
import { Business } from '../../core/tree';
import {
  CodeReviewService,
  FindingSeverity,
  ReviewConfig,
  ReviewDimension,
  ReviewDimensionInput,
} from '../../core/code-review.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';

// Providers whose model is free text (self-host / aggregator) rather than a catalog select —
// mirrors agent-form.ts. OpenRouter additionally gets a live typeahead <datalist>.
const FREE_TEXT_MODEL_PROVIDERS = ['ollama', 'vllm', 'openrouter'];

// DIMENSION_CATALOG mirrors internal/agents/coding/dimensions.go dimensionCatalog() — the
// built-in specialist reviewer lanes. Kept in sync by hand (Slice 2 presets seed EDITABLE
// rows from it; the review always runs whatever the user saved). If you change a prompt or
// scope here, change it in dimensions.go too. Prompts are the value each lane adds.
interface CatalogDim {
  key: string;
  label: string;
  min_severity: FindingSeverity;
  scope_globs: string[];
  prompt: string;
}

const DIMENSION_CATALOG: CatalogDim[] = [
  {
    key: 'security', label: 'Security', min_severity: 'warning', scope_globs: [],
    prompt:
      'You are a senior application-security engineer reviewing a pull request. Report ONLY security concerns: injection (SQL/command/template), authentication or authorization gaps, secret/credential exposure, unsafe or unbounded input handling, SSRF, path traversal, insecure deserialization, and missing validation on trust boundaries. Severity: error = an exploitable vulnerability; warning = a risky pattern or missing control; info = a hardening suggestion. Do not report style, performance, or non-security issues, and do not fabricate issues with no basis in the code.',
  },
  {
    key: 'correctness', label: 'Correctness', min_severity: 'info', scope_globs: [],
    prompt:
      'You are a senior software engineer reviewing a pull request for CORRECTNESS. Report bugs and logic errors: crashes, nil/undefined access, off-by-one, incorrect conditions, race conditions and concurrency hazards, resource leaks, unhandled errors, and wrong results. Severity: error = a real bug causing a crash, data loss, or incorrect behavior; warning = a likely bug or risky pattern; info = a plausible concern worth surfacing. Skip pure style and performance; do not fabricate issues.',
  },
  {
    key: 'performance', label: 'Performance', min_severity: 'warning',
    scope_globs: ['**/*.go', '**/*.ts', '**/*.tsx', '**/*.js', '**/*.py', '**/*.rs', '**/*.java', '**/*.sql'],
    prompt:
      'You are a performance engineer reviewing a pull request. Report efficiency concerns: N+1 queries, unbounded loops or allocations, blocking I/O on hot paths, missing indexes or pagination, redundant work, and quadratic behavior. Severity: error = a change that will clearly degrade production performance; warning = a likely inefficiency; info = an optimization worth considering. Skip correctness and style; do not fabricate issues.',
  },
  {
    key: 'ui', label: 'UI / A11y', min_severity: 'info',
    scope_globs: ['frontend/**', 'web/**', '**/*.tsx', '**/*.jsx', '**/*.vue', '**/*.svelte', '**/*.css', '**/*.scss', '**/*.html'],
    prompt:
      'You are a frontend engineer reviewing a pull request for UI quality and ACCESSIBILITY. Report: missing ARIA/roles/labels, keyboard-navigation and focus-management gaps, insufficient color contrast, non-semantic markup, layout/responsive issues, and inconsistent component usage. Severity: error = a broken or inaccessible experience; warning = a likely UX/a11y problem; info = a polish suggestion. Skip backend logic; do not fabricate issues.',
  },
  {
    key: 'tests', label: 'Tests', min_severity: 'warning', scope_globs: [],
    prompt:
      "You are a senior engineer reviewing a pull request's TEST coverage and quality. Report: new or changed logic that lacks tests, missing edge/error-case coverage, weak assertions, flaky patterns (time/order/network dependence), and tests that don't actually exercise the change. Severity: error = untested critical/security logic; warning = a meaningful coverage or quality gap; info = a testing suggestion. Do not fabricate issues.",
  },
  {
    key: 'docs', label: 'Docs & Comments', min_severity: 'info', scope_globs: [],
    prompt:
      'You are reviewing a pull request for DOCUMENTATION and COMMENT quality. Report: comments that no longer match the code (comment rot), missing docs on exported/public API, misleading names, and stale references. Severity: warning = a misleading or wrong comment/doc; info = a documentation suggestion. Skip code logic; do not fabricate issues.',
  },
];

// Presets are curated subsets of the catalog (spec 008 Slice 2). Fast = the two highest-value
// lanes; Balanced adds performance + tests; Thorough enables the whole catalog. Each is a
// starting point — a review multiplies cost/latency per enabled lane, so smaller is cheaper.
const PRESETS: Record<string, string[]> = {
  fast: ['security', 'correctness'],
  balanced: ['security', 'correctness', 'performance', 'tests'],
  thorough: ['security', 'correctness', 'performance', 'ui', 'tests', 'docs'],
};

const PROVIDERS: { value: string; label: string }[] = [
  { value: '', label: 'Default (review credential)' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'openai', label: 'OpenAI' },
  { value: 'ollama', label: 'Ollama (self-host)' },
  { value: 'vllm', label: 'vLLM (self-host)' },
  { value: 'openrouter', label: 'OpenRouter' },
];

const SEVERITIES: FindingSeverity[] = ['info', 'warning', 'error'];

// DraftRow is the editable in-memory shape of one dimension row. id is null for a preset-seeded
// row not yet persisted; scope is the comma-separated glob string shown in the input.
interface DraftRow {
  id: string | null;
  dimension: string;
  label: string;
  enabled: boolean;
  provider: string;
  model: string;
  min_severity: FindingSeverity;
  scope: string;
  prompt: string;
  saving: boolean;
}

function catalogLabel(key: string): string {
  return DIMENSION_CATALOG.find((c) => c.key === key)?.label ?? key;
}

// Review Setup page (/code-review/setup). Configures a business's reviewer panel (spec 008
// Slice 2): pick a business, seed dimensions from a Fast/Balanced/Thorough preset or the
// configured rows, tweak provider/model/scope/severity per lane, and set panel-level
// aggregation config. Deviation from the plan sketch: paramless route + on-page business
// selector (mirrors the code-review list page) so it works as a static nav target.
@Component({
  selector: 'app-code-review-setup',
  standalone: true,
  imports: [FormsModule, PageHeader, Spinner, EmptyState],
  template: `
    <div class="mf-card" data-testid="code-review-setup">
      <mf-page-header title="Review Setup" subtitle="Configure the multi-dimension reviewer panel for a business.">
        @if (loading()) {
          <span class="mf-loading-row" actions><mf-spinner /></span>
        }
      </mf-page-header>

      @if (error()) {
        <p class="mf-err" data-testid="setup-error">{{ error() }}</p>
      }
      @if (saved()) {
        <p style="color:var(--mf-success,#2e7d32);font-size:var(--mf-fs-sm)" data-testid="setup-saved">{{ saved() }}</p>
      }

      <!-- Business selector -->
      <div class="mf-field" style="max-width:360px;margin-bottom:16px">
        <label for="setup-business">Business</label>
        <select id="setup-business" class="mf-select" data-testid="setup-business"
                [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
          @for (b of businesses(); track b.id) {
            <option [value]="b.id">{{ b.name }}</option>
          }
        </select>
      </div>

      <!-- Presets -->
      <div style="display:flex;gap:8px;align-items:center;margin-bottom:12px;flex-wrap:wrap">
        <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Start from a preset:</span>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="preset-fast" (click)="applyPreset('fast')">Fast</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="preset-balanced" (click)="applyPreset('balanced')">Balanced</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="preset-thorough" (click)="applyPreset('thorough')">Thorough</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="add-dimension" (click)="addRow()">+ Dimension</button>
      </div>

      <!-- Dimensions table -->
      <div class="mf-table" data-testid="dimensions-table" role="table" aria-label="Review dimensions">
        <div class="mf-tr mf-th" role="row">
          <span style="width:56px" role="columnheader">On</span>
          <span style="flex:1" role="columnheader">Dimension</span>
          <span style="flex:1" role="columnheader">Provider</span>
          <span style="flex:1" role="columnheader">Model</span>
          <span style="width:110px" role="columnheader">Min severity</span>
          <span style="flex:1" role="columnheader">Scope globs</span>
          <span style="width:150px" role="columnheader" aria-label="Actions"></span>
        </div>
        @for (row of rows(); track $index) {
          <div class="mf-tr" data-testid="dimension-row" role="row">
            <span style="width:56px" role="cell">
              <input type="checkbox" data-testid="row-enabled" [(ngModel)]="row.enabled"
                     [attr.aria-label]="'Enable ' + row.label + ' dimension'" />
            </span>
            <span style="flex:1;font-weight:500" role="cell">{{ row.label }}</span>
            <span style="flex:1" role="cell">
              <select class="mf-select" data-testid="row-provider" [ngModel]="row.provider" (ngModelChange)="onProviderChange(row, $event)"
                      [attr.aria-label]="'Provider for ' + row.label">
                @for (p of providers; track p.value) {
                  <option [value]="p.value">{{ p.label }}</option>
                }
              </select>
            </span>
            <span style="flex:1" role="cell">
              @if (isFreeText(row.provider)) {
                <input class="mf-input" type="text" data-testid="row-model-text" [(ngModel)]="row.model"
                       [attr.list]="isOpenRouter(row.provider) ? 'setup-openrouter-models' : null"
                       [attr.aria-label]="'Model for ' + row.label" placeholder="model id" />
              } @else if (row.provider === '') {
                <input class="mf-input" type="text" data-testid="row-model-text" [(ngModel)]="row.model"
                       [attr.aria-label]="'Model for ' + row.label" placeholder="(default)" />
              } @else {
                <select class="mf-select" data-testid="row-model-select" [(ngModel)]="row.model"
                        [attr.aria-label]="'Model for ' + row.label">
                  <option value="">Choose a model…</option>
                  @for (m of modelsForProvider(row.provider); track m.model_id) {
                    <option [value]="m.model_id">{{ m.model_id }}</option>
                  }
                </select>
              }
            </span>
            <span style="width:110px" role="cell">
              <select class="mf-select" data-testid="row-severity" [(ngModel)]="row.min_severity"
                      [attr.aria-label]="'Minimum severity for ' + row.label">
                @for (s of severities; track s) {
                  <option [value]="s">{{ s }}</option>
                }
              </select>
            </span>
            <span style="flex:1" role="cell">
              <input class="mf-input" type="text" data-testid="row-scope" [(ngModel)]="row.scope"
                     [attr.aria-label]="'Scope globs for ' + row.label" placeholder="all files" />
            </span>
            <span style="width:150px;display:flex;gap:6px;justify-content:flex-end" role="cell">
              <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="row-save"
                      [disabled]="row.saving" (click)="saveRow(row)" [attr.aria-label]="'Save ' + row.label">Save</button>
              <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="row-remove"
                      (click)="removeRow(row)" [attr.aria-label]="'Remove ' + row.label">Remove</button>
            </span>
          </div>
        }
        @if (!rows().length && !loading()) {
          <mf-empty-state title="No dimensions configured" data-testid="dimensions-empty">
            Pick a preset above, or add a dimension. With none configured, reviews run the default single general lane.
          </mf-empty-state>
        }
      </div>

      <datalist id="setup-openrouter-models">
        @for (m of openrouterModels(); track m.model_id) {
          <option [value]="m.model_id"></option>
        }
      </datalist>

      <!-- Panel-level aggregation config -->
      <h3 style="margin:24px 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
        Aggregation
      </h3>
      <div class="mf-card" data-testid="config-form" style="display:flex;flex-direction:column;gap:10px">
        <label style="display:flex;gap:8px;align-items:center">
          <input type="checkbox" data-testid="config-dedupe" [ngModel]="config().dedupe" (ngModelChange)="patchConfig({ dedupe: $event })" />
          Deduplicate findings flagged by multiple lanes
        </label>
        <label style="display:flex;gap:8px;align-items:center">
          <input type="checkbox" data-testid="config-cite-rules" [ngModel]="config().cite_rules" (ngModelChange)="patchConfig({ cite_rules: $event })" />
          Cite rules in findings
        </label>
        <div class="mf-field" style="max-width:260px">
          <label for="config-post-mode">Post mode</label>
          <select id="config-post-mode" class="mf-select" data-testid="config-post-mode"
                  [ngModel]="config().post_mode" (ngModelChange)="patchConfig({ post_mode: $event })">
            <option value="single">Single combined review</option>
            <option value="per_dimension">One review per dimension</option>
          </select>
        </div>
        <fieldset class="mf-field" style="border:none;padding:0;margin:0;min-inline-size:auto">
          <legend style="font-weight:500;margin-bottom:2px;padding:0">Reviewbot fallback chain</legend>
          <small class="mf-hint" style="display:block">Ordered — the first reachable bot reviews; if it's down, the next one does.
            Empty means no fallback (the triggering agent reviews).</small>
          <div data-testid="chain-list" style="display:flex;flex-direction:column;gap:6px;margin-top:6px">
            @for (id of config().review_agent_chain; track id; let i = $index) {
              <div style="display:flex;gap:8px;align-items:center" [attr.data-testid]="'chain-row-' + i">
                <span style="min-width:20px;color:var(--mf-text-muted)">{{ i + 1 }}.</span>
                <span style="flex:1" [attr.data-testid]="'chain-name-' + i">{{ agentName(id) }}</span>
                <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" [attr.data-testid]="'chain-up-' + i"
                        [disabled]="i === 0" (click)="moveChain(i, -1)" [attr.aria-label]="'Move ' + agentName(id) + ' up'">↑</button>
                <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" [attr.data-testid]="'chain-down-' + i"
                        [disabled]="i === config().review_agent_chain.length - 1" (click)="moveChain(i, 1)" [attr.aria-label]="'Move ' + agentName(id) + ' down'">↓</button>
                <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" [attr.data-testid]="'chain-remove-' + i"
                        (click)="removeFromChain(id)" [attr.aria-label]="'Remove ' + agentName(id) + ' from the fallback chain'">Remove</button>
              </div>
            }
            @if (config().review_agent_chain.length === 0) {
              <small class="mf-hint" data-testid="chain-empty">No fallback configured.</small>
            }
          </div>
          @if (availableAgents().length > 0) {
            <select class="mf-select" data-testid="chain-add" style="margin-top:6px;max-width:260px"
                    aria-label="Add a reviewbot to the fallback chain"
                    [ngModel]="''" (ngModelChange)="addToChain($event)">
              <option value="" disabled>Add a reviewbot…</option>
              @for (a of availableAgents(); track a.id) {
                <option [value]="a.id">{{ a.name }}</option>
              }
            </select>
          }
        </fieldset>
        <div>
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="config-save"
                  [disabled]="savingConfig()" (click)="saveConfig()">Save aggregation</button>
        </div>
      </div>
    </div>
  `,
})
export class CodeReviewSetupComponent implements OnInit {
  private api = inject(CodeReviewService);
  private agentsApi = inject(AgentsService);
  private bizApi = inject(BusinessService);
  private current = inject(CurrentBusinessService);

  readonly providers = PROVIDERS;
  readonly severities = SEVERITIES;

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  rows = signal<DraftRow[]>([]);
  config = signal<ReviewConfig>({
    dedupe: true, verify_enabled: false, verify_provider: '', verify_model: '', cite_rules: false, post_mode: 'single',
    review_agent_chain: [],
  });
  agents = signal<Agent[]>([]);
  allModels = signal<ModelDescriptor[]>([]);
  openrouterModels = signal<ModelDescriptor[]>([]);
  private openrouterLoaded = false;

  loading = signal(true);
  savingConfig = signal(false);
  error = signal('');
  saved = signal('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id ?? '';
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.loadPanel();
        } else {
          this.loading.set(false);
        }
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load businesses.');
      },
    });
  }

  selectBusiness(id: string): void {
    if (!id || id === this.businessId()) return;
    this.businessId.set(id);
    this.current.set(id);
    this.loadPanel();
  }

  private loadPanel(): void {
    const bid = this.businessId();
    if (!bid) return;
    this.loading.set(true);
    this.error.set('');
    this.saved.set('');
    this.openrouterLoaded = false;
    this.openrouterModels.set([]);

    this.api.listDimensions(bid).subscribe({
      next: (r) => {
        const rows = (r.items ?? []).map((d) => this.rowFromServer(d));
        this.rows.set(rows);
        this.loading.set(false);
        if (rows.some((row) => row.provider === 'openrouter')) this.ensureOpenrouterModels();
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(e.status === 403 || e.status === 404 ? "You don't have access to this business." : 'Could not load the review panel.');
      },
    });
    this.api.getConfig(bid).subscribe({ next: (c) => this.config.set(this.withChain(c)), error: () => {} });
    this.agentsApi.models(bid).subscribe({ next: (r) => this.allModels.set(r.items ?? []), error: () => {} });
    this.agentsApi.list(bid).subscribe({ next: (r) => this.agents.set(r.items ?? []), error: () => {} });
  }

  applyPreset(name: string): void {
    const keys = PRESETS[name] ?? [];
    const rows = keys
      .map((k) => DIMENSION_CATALOG.find((c) => c.key === k))
      .filter((c): c is CatalogDim => !!c)
      .map((c) => this.rowFromCatalog(c));
    this.rows.set(rows);
  }

  addRow(): void {
    // Add the first catalog dimension not already present (falls back to "general").
    const present = new Set(this.rows().map((r) => r.dimension));
    const next = DIMENSION_CATALOG.find((c) => !present.has(c.key));
    const row = next
      ? this.rowFromCatalog(next)
      : { id: null, dimension: 'general', label: 'General', enabled: true, provider: '', model: '', min_severity: 'info' as FindingSeverity, scope: '', prompt: '', saving: false };
    this.rows.set([...this.rows(), row]);
  }

  saveRow(row: DraftRow): void {
    const bid = this.businessId();
    if (!bid) return;
    row.saving = true;
    this.bumpRows();
    this.api.upsertDimension(bid, this.toInput(row)).subscribe({
      next: (saved) => {
        row.id = saved.id;
        row.saving = false;
        this.saved.set(`Saved ${row.label}.`);
        this.bumpRows();
      },
      error: (e: HttpErrorResponse) => {
        row.saving = false;
        this.bumpRows();
        this.error.set(e.status === 400 ? 'Invalid dimension config.' : 'Could not save the dimension.');
      },
    });
  }

  removeRow(row: DraftRow): void {
    const drop = () => this.rows.set(this.rows().filter((r) => r !== row));
    if (!row.id) {
      drop();
      return;
    }
    const bid = this.businessId();
    if (!bid) return;
    this.api.deleteDimension(bid, row.id).subscribe({
      next: () => {
        drop();
        this.saved.set(`Removed ${row.label}.`);
      },
      error: () => this.error.set('Could not remove the dimension.'),
    });
  }

  saveConfig(): void {
    const bid = this.businessId();
    if (!bid) return;
    this.savingConfig.set(true);
    this.api.putConfig(bid, this.config()).subscribe({
      next: (c) => {
        this.config.set(c);
        this.savingConfig.set(false);
        this.saved.set('Saved aggregation config.');
      },
      error: () => {
        this.savingConfig.set(false);
        this.error.set('Could not save the aggregation config.');
      },
    });
  }

  onProviderChange(row: DraftRow, provider: string): void {
    row.provider = provider;
    row.model = '';
    if (provider === 'openrouter') this.ensureOpenrouterModels();
    this.bumpRows();
  }

  modelsForProvider(provider: string): ModelDescriptor[] {
    return this.allModels().filter((m) => m.provider === provider);
  }

  isFreeText(provider: string): boolean {
    return FREE_TEXT_MODEL_PROVIDERS.includes(provider);
  }

  isOpenRouter(provider: string): boolean {
    return provider === 'openrouter';
  }

  patchConfig(patch: Partial<ReviewConfig>): void {
    this.config.set({ ...this.config(), ...patch });
  }

  // withChain guards a server config that predates / omits the chain field, so the
  // template's @for and length checks never hit undefined.
  private withChain(c: ReviewConfig): ReviewConfig {
    return { ...c, review_agent_chain: c.review_agent_chain ?? [] };
  }

  // agentName renders a chain entry by its agent name (falls back to the raw id when the
  // agent list hasn't loaded or the agent was deleted).
  agentName(id: string): string {
    return this.agents().find((a) => a.id === id)?.name ?? id;
  }

  // availableAgents are the business's agents not already in the chain (the add dropdown).
  availableAgents = computed(() => {
    const inChain = new Set(this.config().review_agent_chain);
    return this.agents().filter((a) => !inChain.has(a.id));
  });

  addToChain(id: string): void {
    if (!id) return;
    const chain = this.config().review_agent_chain;
    if (chain.includes(id)) return;
    this.patchConfig({ review_agent_chain: [...chain, id] });
  }

  removeFromChain(id: string): void {
    this.patchConfig({ review_agent_chain: this.config().review_agent_chain.filter((x) => x !== id) });
  }

  // moveChain swaps entry i with its neighbor (delta -1 up / +1 down) to reorder priority.
  moveChain(i: number, delta: number): void {
    const chain = [...this.config().review_agent_chain];
    const j = i + delta;
    if (j < 0 || j >= chain.length) return;
    [chain[i], chain[j]] = [chain[j], chain[i]];
    this.patchConfig({ review_agent_chain: chain });
  }

  private ensureOpenrouterModels(): void {
    if (this.openrouterLoaded) return;
    this.openrouterLoaded = true;
    const bid = this.businessId();
    if (!bid) return;
    this.agentsApi.providerModels(bid, 'openrouter').subscribe({
      next: (r) => this.openrouterModels.set(r.items ?? []),
      error: () => this.openrouterModels.set([]),
    });
  }

  private bumpRows(): void {
    this.rows.set([...this.rows()]);
  }

  private rowFromServer(d: ReviewDimension): DraftRow {
    return {
      id: d.id,
      dimension: d.dimension,
      label: catalogLabel(d.dimension),
      enabled: d.enabled,
      provider: d.provider ?? '',
      model: d.model,
      min_severity: d.min_severity,
      scope: (d.scope_globs ?? []).join(', '),
      prompt: d.prompt,
      saving: false,
    };
  }

  private rowFromCatalog(c: CatalogDim): DraftRow {
    return {
      id: null,
      dimension: c.key,
      label: c.label,
      enabled: true,
      provider: '',
      model: '',
      min_severity: c.min_severity,
      scope: c.scope_globs.join(', '),
      prompt: c.prompt,
      saving: false,
    };
  }

  private toInput(row: DraftRow): ReviewDimensionInput {
    const idx = this.rows().indexOf(row);
    return {
      dimension: row.dimension,
      provider: row.provider,
      model: row.model,
      prompt: row.prompt,
      scope_globs: row.scope.split(',').map((s) => s.trim()).filter(Boolean),
      min_severity: row.min_severity,
      enabled: row.enabled,
      sort_order: (idx >= 0 ? idx : this.rows().length) + 1,
    };
  }
}
