import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService, AIProvider } from '../../../core/ai-credentials.service';

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
                [ngModel]="provider()" (ngModelChange)="provider.set($event)" name="provider" [disabled]="submitting()">
          <option value="anthropic">Anthropic</option>
          <option value="openai">OpenAI</option>
          <option value="ollama">Ollama (self-host)</option>
          <option value="vllm">vLLM (self-host)</option>
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
        <label for="cred-base-url">Base URL <span class="mf-hint">(optional)</span></label>
        <input id="cred-base-url" class="mf-input" type="text" data-testid="cred-base-url"
               name="base_url" [(ngModel)]="baseUrl" placeholder="https://… (openai-compatible / self-host only)" [disabled]="submitting()" />
        <small class="mf-hint">Only needed for OpenAI-compatible or self-hosted (Ollama/vLLM) endpoints. Leave blank for the provider default.</small>
      </div>

      <label class="mf-field" data-testid="cred-allow-private-wrap"
             style="display:flex;align-items:center;gap:8px;cursor:pointer">
        <input type="checkbox" data-testid="cred-allow-private" name="allow_private_base_url"
               [(ngModel)]="allowPrivateBaseUrl" [disabled]="submitting()" />
        Allow a private / loopback base URL (self-host only)
      </label>

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

  submitting = signal(false);
  error = signal('');

  valid(): boolean {
    return this.apiKey.trim().length > 0 && this.defaultModel.trim().length > 0;
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
