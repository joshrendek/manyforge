import { HttpErrorResponse } from '@angular/common/http';
import { Component, DestroyRef, EventEmitter, Input, OnInit, Output, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import {
  AICredentialsService,
  CodexConnectBody,
  CodexConnectStatus,
  CodexPKCEStart,
} from '../../../core/ai-credentials.service';
import { AgentsService, ModelDescriptor } from '../../../core/agents.service';

// CodexConnectComponent drives the "Sign in with ChatGPT" flow for an openai_codex credential.
// OpenAI's ChatGPT auth has NO device-authorization grant (its OIDC discovery advertises only
// authorization_code + refresh_token), so the only web-viable flow is the codex CLI's
// authorization_code + PKCE flow reproduced via paste-the-redirect-URL: the user signs in in a new
// tab and is redirected to the CLI's fixed http://localhost:1455 loopback (which can't load in a
// hosted app), then pastes that URL back so the backend can exchange the code. The backend upserts
// on (business_id, provider), so re-running this for an already-connected account replaces it in
// place — this component is reused verbatim for "Reconnect".
@Component({
  selector: 'app-codex-connect',
  imports: [FormsModule],
  template: `
    <div class="mf-card" data-testid="codex-connect" style="padding:12px;margin-top:8px">
      @if (phase() === 'configure') {
        <div class="mf-field">
          <label for="codex-model">Model</label>
          <select id="codex-model" class="mf-select" data-testid="codex-model"
                  [ngModel]="model()" (ngModelChange)="model.set($event)" name="codex-model">
            <option value="" disabled>Choose a model…</option>
            @for (m of codexModels(); track m.model_id) {
              <option [value]="m.model_id">{{ m.model_id }}</option>
            }
          </select>
        </div>
        <div class="mf-field">
          <label for="codex-lanes">Max concurrent lanes</label>
          <input id="codex-lanes" class="mf-input" type="number" min="1" max="16"
                 name="codex-lanes" [(ngModel)]="maxLanes" />
        </div>
        @if (error()) { <p class="mf-err" data-testid="codex-error" role="alert">{{ error() }}</p> }
        <div style="display:flex;gap:8px">
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-signin"
                  [disabled]="submitting() || !model()" (click)="startSignin()">
            {{ submitting() ? 'Starting…' : 'Sign in with ChatGPT' }}
          </button>
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        </div>
      }

      @if (phase() === 'signin') {
        <div data-testid="codex-signin-steps">
          <ol style="margin:0 0 8px 1.1rem;padding:0;display:flex;flex-direction:column;gap:6px">
            <li>
              Open the ChatGPT sign-in page and approve access:
              <a class="mf-btn mf-btn-primary mf-btn-sm" style="margin-left:6px"
                 [href]="pkce()?.authorize_url" target="_blank" rel="noopener" data-testid="codex-open"
                 aria-label="Open ChatGPT to sign in — opens in a new tab">Open ChatGPT sign-in</a>
            </li>
            <li>
              After you approve, your browser lands on a <code>localhost:1455</code> page that
              <strong>won't load</strong> — that is expected, nothing is running there.
            </li>
            <li>Copy the full URL from that tab's address bar and paste it below.</li>
          </ol>
          <div class="mf-field">
            <label for="codex-paste">Redirect URL</label>
            <input id="codex-paste" class="mf-input" type="text" name="codex-paste" [(ngModel)]="pasteUrl"
                   placeholder="http://localhost:1455/auth/callback?code=…&state=…" data-testid="codex-paste-url" />
          </div>
          @if (error()) { <p class="mf-err" data-testid="codex-error" role="alert">{{ error() }}</p> }
          <div style="display:flex;gap:8px">
            <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-paste-submit"
                    [disabled]="submitting() || !pasteUrl.trim()" (click)="submitPaste()">
              {{ submitting() ? 'Finishing…' : 'Finish sign-in' }}
            </button>
            <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="codex-startover"
                    (click)="reset()">Start over</button>
          </div>
        </div>
      }
    </div>
  `,
})
export class CodexConnectComponent implements OnInit {
  @Input() businessId = '';
  // connected emits the newly-created/refreshed credential_id once the flow resolves.
  @Output() connected = new EventEmitter<string>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AICredentialsService);
  private agents = inject(AgentsService);
  private destroyRef = inject(DestroyRef);

  phase = signal<'configure' | 'signin'>('configure');
  codexModels = signal<ModelDescriptor[]>([]);
  model = signal<string>('');
  maxLanes = 4;
  pkce = signal<CodexPKCEStart | null>(null);
  pasteUrl = '';
  submitting = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.agents
      .models(this.businessId)
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (r) => {
          const codex = (r.items ?? []).filter((m) => m.provider === 'openai_codex');
          this.codexModels.set(codex);
          // Pre-select the first catalog model so the primary button isn't disabled by default.
          if (!this.model() && codex.length) this.model.set(codex[0].model_id);
        },
        error: () => this.codexModels.set([]),
      });
  }

  private body(): CodexConnectBody {
    return {
      default_model: this.model(),
      max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.maxLanes) || 4))),
    };
  }

  // startSignin begins the PKCE flow: it stores a pending row and returns the ChatGPT authorize URL
  // to open, then advances to the paste step.
  startSignin(): void {
    if (this.submitting() || !this.model()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api
      .codexPKCEStart(this.businessId, this.body())
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (p) => {
          this.pkce.set(p);
          this.phase.set('signin');
          this.submitting.set(false);
        },
        error: (e: HttpErrorResponse) => {
          this.submitting.set(false);
          this.error.set(this.describe(e));
        },
      });
  }

  submitPaste(): void {
    const p = this.pkce();
    if (!p || !this.pasteUrl.trim() || this.submitting()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api
      .codexPKCEExchange(this.businessId, p.pending_id, this.pasteUrl.trim())
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (s) => {
          this.submitting.set(false);
          this.applyStatus(s);
        },
        error: (e: HttpErrorResponse) => {
          this.submitting.set(false);
          this.error.set(this.describe(e));
        },
      });
  }

  reset(): void {
    this.pkce.set(null);
    this.pasteUrl = '';
    this.error.set('');
    this.submitting.set(false);
    this.phase.set('configure');
  }

  private applyStatus(s: CodexConnectStatus): void {
    if (s.status === 'approved' && s.credential_id) {
      this.connected.emit(s.credential_id);
      return;
    }
    if (s.status === 'denied') {
      this.error.set('Sign-in was denied. Click "Start over" to try again.');
      return;
    }
    if (s.status === 'expired') {
      this.error.set('This sign-in request expired. Click "Start over" to try again.');
      return;
    }
    // 'pending' should not occur for a one-shot exchange; treat as a retryable paste problem.
    this.error.set('Sign-in is not complete yet — double-check the pasted URL and try again.');
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Could not complete sign-in. Check the pasted URL and try again.';
    }
    // 404 on the exchange means the single-use pending row expired or was already consumed.
    if (e.status === 403 || e.status === 404) return 'This sign-in request expired or is invalid — click "Start over".';
    if (e.status === 409) return 'A ChatGPT connection already exists for this business.';
    return 'Could not connect. Please try again.';
  }
}
