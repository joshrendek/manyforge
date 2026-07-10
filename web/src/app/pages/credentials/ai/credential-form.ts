import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, Output, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService, AIProvider } from '../../../core/ai-credentials.service';

// Providers with one canonical OpenAI-compatible endpoint. The form prefills it so the user
// just pastes a key, and the server would default it anyway. Mirrors ai.DefaultBaseURL —
// anthropic is defaulted server-side but has nothing to prefill (its transport applies it).
const PREFILLED_BASE_URLS: Partial<Record<AIProvider, string>> = {
  openrouter: 'https://openrouter.ai/api/v1',
  huggingface: 'https://router.huggingface.co/v1',
};

// Providers the server defaults a base_url for; every other provider must supply one.
// Mirrors validate() in internal/agents/credential.go — keep them in sync.
const BASE_URL_DEFAULTED: readonly AIProvider[] = ['anthropic', 'openrouter', 'huggingface'];

// AI credential create form. Standalone, template-driven. The API key is write-only
// (type=password, never prefilled). base_url + allow_private_base_url are ALWAYS visible
// with helper text (design decision: not conditional on provider) so an operator can point
// any provider at an OpenAI-compatible / self-hosted endpoint.
@Component({
  selector: 'app-credential-form',
  imports: [FormsModule],
  template: `
    <form class="mf-card" style="display:flex;flex-direction:column;gap:14px" (ngSubmit)="submit()" data-testid="credential-form">
      <div class="mf-field">
        <label for="cred-provider">Provider</label>
        <select id="cred-provider" class="mf-select" data-testid="cred-provider"
                [ngModel]="provider()" (ngModelChange)="onProviderChange($event)" name="provider" [disabled]="submitting()">
          <option value="anthropic">Anthropic</option>
          <option value="openai">OpenAI</option>
          <option value="ollama">Ollama (self-host)</option>
          <option value="vllm">vLLM (self-host)</option>
          <option value="openrouter">OpenRouter</option>
          <option value="huggingface">HuggingFace (Inference Providers)</option>
        </select>
      </div>

      <div class="mf-field">
        <label for="cred-key">API key</label>
        <input id="cred-key" class="mf-input" type="password" autocomplete="off"
               data-testid="cred-api-key" name="api_key"
               [(ngModel)]="apiKey" placeholder="••••••••" [disabled]="submitting()" />
      </div>

      <div class="mf-field">
        <label for="cred-model">Default model</label>
        <input id="cred-model" class="mf-input" type="text" data-testid="cred-default-model"
               name="default_model" [(ngModel)]="defaultModel" placeholder="e.g. claude-opus-4-8" [disabled]="submitting()" />
      </div>

      <div class="mf-field">
        <label for="cred-base-url">Base URL
          <span class="mf-hint">({{ baseUrlRequired() ? 'required' : 'optional' }})</span>
        </label>
        <input id="cred-base-url" class="mf-input" type="text" data-testid="cred-base-url"
               name="base_url" [(ngModel)]="baseUrl" placeholder="https://… (openai-compatible / self-host only)"
               [disabled]="submitting()"
               [required]="baseUrlRequired()"
               [attr.aria-required]="baseUrlRequired() ? 'true' : null"
               aria-describedby="cred-base-url-hint" />
        <small id="cred-base-url-hint" class="mf-hint">Prefilled for OpenRouter and HuggingFace; defaulted server-side for Anthropic. Required for OpenAI-compatible or self-hosted (OpenAI/Ollama/vLLM) endpoints.</small>
      </div>

      <label class="mf-field" data-testid="cred-allow-private-wrap"
             style="display:flex;align-items:center;gap:8px;cursor:pointer">
        <input type="checkbox" data-testid="cred-allow-private" name="allow_private_base_url"
               [(ngModel)]="allowPrivateBaseUrl" [disabled]="submitting()" />
        Allow a private / loopback base URL (self-host only)
      </label>

      <div class="mf-field">
        <label for="cred-lanes">Max concurrent review lanes</label>
        <input id="cred-lanes" class="mf-input" type="number" min="1" max="16" step="1" data-testid="cred-lanes"
               name="max_concurrent_lanes" aria-describedby="cred-lanes-hint" [(ngModel)]="maxConcurrentLanes" [disabled]="submitting()" />
        <small id="cred-lanes-hint" class="mf-hint">How many code-review lanes may hit this endpoint at once. A single-GPU self-host: 1; cloud: 4.</small>
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="credential-form-error">{{ error() }}</p>
      }

      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : 'Add credential' }}
        </button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()" [disabled]="submitting()">Cancel</button>
      </div>
    </form>
  `,
})
export class CredentialFormComponent {
  @Input() businessId = '';
  @Output() saved = new EventEmitter<AICredential>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AICredentialsService);

  provider = signal<AIProvider>('anthropic');
  apiKey = '';
  defaultModel = '';
  baseUrl = '';
  allowPrivateBaseUrl = false;
  maxConcurrentLanes = 4;

  submitting = signal(false);
  error = signal('');

  baseUrlRequired = computed(() => !BASE_URL_DEFAULTED.includes(this.provider()));

  valid(): boolean {
    if (this.apiKey.trim().length === 0 || this.defaultModel.trim().length === 0) {
      return false;
    }
    // The server rejects a missing base_url for every provider it has no default for
    // (credential.go validate()); block it here rather than round-trip a 400.
    return !this.baseUrlRequired() || this.baseUrl.trim().length > 0;
  }

  onProviderChange(p: AIProvider): void {
    // Providers with one canonical endpoint get it prefilled so the user just pastes a key.
    // Only ever auto-manage base_url between blank and the outgoing provider's own default, so
    // a custom value the user typed is never clobbered — and so switching openrouter →
    // huggingface swaps the default rather than leaving the wrong one behind.
    const leaving = PREFILLED_BASE_URLS[this.provider()];
    if (leaving && this.baseUrl === leaving) {
      this.baseUrl = '';
    }
    const arriving = PREFILLED_BASE_URLS[p];
    if (arriving && this.baseUrl === '') {
      this.baseUrl = arriving;
    }
    this.provider.set(p);
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api
      .create(this.businessId, {
        provider: this.provider(),
        api_key: this.apiKey,
        default_model: this.defaultModel.trim(),
        base_url: this.baseUrl.trim() || undefined,
        allow_private_base_url: this.allowPrivateBaseUrl,
        max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.maxConcurrentLanes) || 4))),
      })
      .subscribe({
        next: (c) => {
          this.reset();
          this.submitting.set(false);
          this.saved.emit(c);
        },
        error: (e: HttpErrorResponse) => {
          this.submitting.set(false);
          this.error.set(this.describe(e));
        },
      });
  }

  private reset(): void {
    this.apiKey = '';
    this.defaultModel = '';
    this.baseUrl = '';
    this.allowPrivateBaseUrl = false;
    this.provider.set('anthropic');
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'A credential for that provider already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
