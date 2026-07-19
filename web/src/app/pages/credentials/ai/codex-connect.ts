import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnDestroy, OnInit, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  AICredentialsService,
  CodexConnectBody,
  CodexConnectStatus,
  CodexDeviceStart,
  CodexPKCEStart,
} from '../../../core/ai-credentials.service';
import { AgentsService, ModelDescriptor } from '../../../core/agents.service';

// CodexConnectComponent drives the "Sign in with ChatGPT" flow for an openai_codex credential.
// Device-code is primary (no URL copy-paste); PKCE-paste is a fallback for accounts where
// device-code login is disabled. The backend upserts on (business_id, provider), so re-running
// this flow for an already-connected account replaces it in place — this component is reused
// verbatim for "Reconnect".
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
        @if (error()) { <p class="mf-err" data-testid="codex-error">{{ error() }}</p> }
        <div style="display:flex;gap:8px">
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-signin"
                  [disabled]="submitting() || !model()" (click)="startDevice()">
            {{ submitting() ? 'Starting…' : 'Sign in with ChatGPT' }}
          </button>
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        </div>
      }

      @if (phase() === 'authorizing') {
        <div data-testid="codex-authorizing">
          <p>Enter this code at ChatGPT to authorize this connection:</p>
          <p class="mf-code" data-testid="codex-user-code" style="font-size:var(--mf-fs-lg);letter-spacing:2px">{{ device()?.user_code }}</p>
          <a class="mf-btn mf-btn-primary mf-btn-sm" [href]="device()?.verification_uri_complete"
             target="_blank" rel="noopener" data-testid="codex-open">Open ChatGPT</a>
          <p class="mf-hint" data-testid="codex-waiting">Waiting for approval…</p>
          @if (error()) { <p class="mf-err" data-testid="codex-error">{{ error() }}</p> }
          <details style="margin-top:8px">
            <summary data-testid="codex-paste-toggle">Trouble signing in? Paste a link instead</summary>
            <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="codex-paste-start"
                    style="margin:6px 0" (click)="startPaste()">Open sign-in page</button>
            @if (showPaste()) {
              <p><a class="mf-btn mf-btn-primary mf-btn-sm" [href]="pkce()?.authorize_url" target="_blank" rel="noopener" data-testid="codex-paste-open">Open ChatGPT sign-in</a></p>
              <div class="mf-field">
                <label for="codex-paste">Paste the redirect URL from the address bar</label>
                <input id="codex-paste" class="mf-input" type="text" name="codex-paste" [(ngModel)]="pasteUrl"
                       placeholder="http://localhost:1455/auth/callback?code=…&state=…" data-testid="codex-paste-url" />
              </div>
              <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-paste-submit"
                      [disabled]="submitting() || !pasteUrl.trim()" (click)="submitPaste()">Finish sign-in</button>
            }
          </details>
        </div>
      }

      @if (phase() === 'expired') {
        <div data-testid="codex-expired">
          <p class="mf-err">{{ error() || 'The code expired before it was approved.' }}</p>
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="codex-retry" (click)="reset()">Try again</button>
        </div>
      }
    </div>
  `,
})
export class CodexConnectComponent implements OnInit, OnDestroy {
  @Input() businessId = '';
  @Output() connected = new EventEmitter<string>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AICredentialsService);
  private agents = inject(AgentsService);

  phase = signal<'configure' | 'authorizing' | 'expired'>('configure');
  codexModels = signal<ModelDescriptor[]>([]);
  model = signal<string>('');
  maxLanes = 4;
  device = signal<CodexDeviceStart | null>(null);
  pkce = signal<CodexPKCEStart | null>(null);
  showPaste = signal(false);
  pasteUrl = '';
  submitting = signal(false);
  error = signal('');

  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private resolved = false;
  private deviceAbandoned = false;

  ngOnInit(): void {
    this.agents.models(this.businessId).subscribe({
      next: (r) => this.codexModels.set((r.items ?? []).filter((m) => m.provider === 'openai_codex')),
      error: () => this.codexModels.set([]),
    });
  }

  ngOnDestroy(): void {
    this.stopPolling();
  }

  private body(): CodexConnectBody {
    return {
      default_model: this.model(),
      max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.maxLanes) || 4))),
    };
  }

  startDevice(): void {
    if (this.submitting() || !this.model()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api.codexDeviceStart(this.businessId, this.body()).subscribe({
      next: (d) => {
        this.device.set(d);
        this.phase.set('authorizing');
        this.submitting.set(false);
        this.scheduleNextPoll();
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  // pollOnce fetches the pending status once and applies it. Production schedules repeated calls
  // via scheduleNextPoll; tests call it directly and flush the HTTP response.
  pollOnce(): void {
    const d = this.device();
    if (!d) return;
    this.api.codexDeviceStatus(this.businessId, d.pending_id).subscribe({
      next: (s) => this.applyStatus(s),
      error: (e: HttpErrorResponse) => {
        // A 4xx means the pending flow is gone/denied server-side — terminal, surface it.
        // Network blips / 5xx are transient: keep polling.
        if (e.status >= 400 && e.status < 500) {
          this.resolved = true;
          this.stopPolling();
          this.error.set('This sign-in request expired or was revoked — please try again.');
          this.phase.set('expired');
          return;
        }
        this.scheduleNextPoll();
      },
    });
  }

  startPaste(): void {
    if (!this.model()) return;
    this.stopPolling(); // don't let the device-code poll keep racing once we switch to paste
    this.deviceAbandoned = true; // the paste flow supersedes the device poll
    this.error.set('');
    this.api.codexPKCEStart(this.businessId, this.body()).subscribe({
      next: (p) => {
        this.pkce.set(p);
        this.showPaste.set(true);
      },
      error: (e: HttpErrorResponse) => this.error.set(this.describe(e)),
    });
  }

  submitPaste(): void {
    const p = this.pkce();
    if (!p || !this.pasteUrl.trim()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api.codexPKCEExchange(this.businessId, p.pending_id, this.pasteUrl.trim()).subscribe({
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
    this.stopPolling();
    this.resolved = false;
    this.deviceAbandoned = false;
    this.device.set(null);
    this.pkce.set(null);
    this.showPaste.set(false);
    this.pasteUrl = '';
    this.error.set('');
    this.phase.set('configure');
  }

  private applyStatus(s: CodexConnectStatus): void {
    if (this.resolved) return; // a terminal status already fired — ignore any in-flight stale poll
    if (s.status === 'approved' && s.credential_id) {
      this.resolved = true;
      this.stopPolling();
      this.connected.emit(s.credential_id);
      return;
    }
    if (s.status === 'expired') {
      this.resolved = true;
      this.stopPolling();
      this.phase.set('expired');
      return;
    }
    if (s.status === 'denied') {
      this.resolved = true;
      this.stopPolling();
      this.error.set('Sign-in was denied. Try again.');
      this.phase.set('expired');
      return;
    }
    this.scheduleNextPoll(); // pending
  }

  private scheduleNextPoll(): void {
    if (this.deviceAbandoned) return; // user switched to the paste fallback
    const d = this.device();
    if (!d) return;
    this.stopPolling();
    this.pollTimer = setTimeout(() => this.pollOnce(), Math.max(1, d.interval) * 1000);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Could not start sign-in. Check the model and try again.';
    }
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    if (e.status === 409) return 'A ChatGPT connection already exists for this business.';
    return 'Could not connect. Please try again.';
  }
}
