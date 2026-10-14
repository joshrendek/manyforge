import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import {
  Component,
  ElementRef,
  Input,
  OnInit,
  computed,
  inject,
  signal,
  viewChild,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { WorkProject, WorkService, WorkToken } from '../../core/work.service';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { describeWorkMutation } from './work-labels';

// Connect tab: how to wire a repo to this project with the `mf` CLI, plus work-token
// management. A token's plaintext comes back exactly once (create response) and lives only in
// `created` until dismissed — the list endpoint returns the prefix, never the secret.
@Component({
  selector: 'app-work-connect-panel',
  imports: [FormsModule, DatePipe, StatusPill],
  template: `
    <div class="connect" data-testid="work-connect">
      <section class="steps">
        <div class="mf-section">Connect a repo</div>
        <ol>
          <li>
            <div class="step-title">Install the <code>mf</code> CLI</div>
            <div class="cmd">
              <code data-testid="work-cmd-install">{{ installCmd() }}</code>
              <button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                (click)="copy(installCmd(), 'Command')"
              >
                Copy
              </button>
            </div>
            <p class="mf-hint">Or, from a ManyForge checkout: <code>make install-mf</code></p>
          </li>
          <li>
            <div class="step-title">Point the repo at this project</div>
            <div class="cmd">
              <code data-testid="work-cmd-init">{{ initCmd() }}</code>
              <button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                (click)="copy(initCmd(), 'Command')"
              >
                Copy
              </button>
            </div>
            <p class="mf-hint">
              Writes <code>.mf/config.json</code> (commit it) and adds the workflow block to
              <code>AGENTS.md</code> / <code>CLAUDE.md</code>.
            </p>
          </li>
          <li>
            <div class="step-title">Log in with a work token</div>
            <div class="cmd">
              <code data-testid="work-cmd-login">{{ loginCmd() }}</code>
              <button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                (click)="copy(loginCmd(), 'Command')"
              >
                Copy
              </button>
            </div>
            <p class="mf-hint">
              @if (canManage()) {
                Create a token below.
              } @else {
                Ask a project admin for a token.
              }
              It is stored in <code>~/.config/mf</code>, never in the repo.
            </p>
          </li>
          <li>
            <div class="step-title">Hook it into Claude Code</div>
            <div class="cmd">
              <code data-testid="work-cmd-setup">mf setup claude</code>
              <button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                (click)="copy('mf setup claude', 'Command')"
              >
                Copy
              </button>
            </div>
            <p class="mf-hint">
              Runs <code>mf prime</code> at session start, so agents see the ready list and your
              operator inbox.
            </p>
          </li>
        </ol>
      </section>

      @if (canManage()) {
        <section class="tokens" data-testid="work-tokens">
          <div class="mf-section">Work tokens</div>
          <p class="mf-hint">
            A token can read and write this project's tasks only. Create one per machine or agent so
            you can revoke them independently.
          </p>
          <form class="mf-filters" (ngSubmit)="create()">
            <div class="mf-field" style="flex:1 1 220px">
              <label for="wk-token-name">Token name</label>
              <input
                id="wk-token-name"
                class="mf-input"
                name="tokenName"
                placeholder="e.g. work-laptop"
                data-testid="work-token-name"
                [(ngModel)]="newName"
              />
            </div>
            <div style="display:flex;align-items:flex-end">
              <button
                #createBtn
                type="submit"
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="work-token-create"
                [disabled]="!newName.trim() || creating()"
              >
                {{ creating() ? 'Creating…' : 'Create token' }}
              </button>
            </div>
          </form>

          @if (created(); as c) {
            <div
              class="secret-once"
              data-testid="work-token-once"
              role="status"
              aria-live="polite"
              aria-atomic="true"
            >
              <div class="secret-once-head">
                <mf-status-pill tone="warn" label="Shown once" />
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="work-token-dismiss"
                  (click)="dismiss()"
                >
                  Dismiss
                </button>
              </div>
              <p class="mf-hint">
                Copy this token now — you won't be able to see it again. Run this in the repo:
              </p>
              <div class="secret-once-value">
                <code data-testid="work-token-login-cmd">{{ loginFor(c.token) }}</code>
                <button
                  type="button"
                  class="mf-btn mf-btn-primary mf-btn-sm"
                  data-testid="work-token-copy"
                  (click)="copy(loginFor(c.token), 'Login command')"
                >
                  Copy
                </button>
              </div>
              <div class="secret-once-value">
                <code data-testid="work-token-value">{{ c.token }}</code>
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="work-token-copy-raw"
                  (click)="copy(c.token, 'Token')"
                >
                  Copy token
                </button>
              </div>
            </div>
          }

          <div class="mf-table" data-testid="work-tokens-list">
            <div class="mf-tr mf-th">
              <span style="flex:2">Name</span>
              <span style="flex:1">Prefix</span>
              <span style="flex:1">Created</span>
              <span style="flex:1">Last used</span>
              <span style="flex:0 0 150px"></span>
            </div>
            @for (t of tokens(); track t.id) {
              <div class="mf-tr" data-testid="work-token-row" [attr.data-token-id]="t.id">
                <span style="flex:2">{{ t.name }}</span>
                <code style="flex:1" class="mf-mono prefix">{{ t.prefix }}…</code>
                <span style="flex:1" class="when">{{ t.created_at | date: 'MMM d, y' }}</span>
                <span style="flex:1" class="when">{{
                  t.last_used_at ? (t.last_used_at | date: 'MMM d, HH:mm') : 'Never'
                }}</span>
                <span style="flex:0 0 150px;display:flex;gap:6px;justify-content:flex-end">
                  @if (t.revoked_at) {
                    <mf-status-pill tone="neutral" label="Revoked" />
                  } @else if (confirming() === t.id) {
                    <button
                      type="button"
                      class="mf-btn mf-btn-danger mf-btn-sm"
                      data-testid="work-token-revoke-confirm"
                      [disabled]="busy() === t.id"
                      (click)="revoke(t)"
                    >
                      Revoke
                    </button>
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      (click)="confirming.set('')"
                    >
                      Keep
                    </button>
                  } @else {
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="work-token-revoke"
                      (click)="confirming.set(t.id)"
                    >
                      Revoke…
                    </button>
                  }
                </span>
              </div>
            }
            @if (!tokens().length) {
              <span class="mf-empty-inline" data-testid="work-tokens-empty"
                >No tokens yet. Create one to connect your first repo.</span
              >
            }
          </div>
        </section>
      }
    </div>
  `,
  styles: [
    `
      .connect {
        display: grid;
        grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
        gap: var(--mf-space-6);
        align-items: start;
      }
      @media (max-width: 1100px) {
        .connect {
          grid-template-columns: minmax(0, 1fr);
        }
      }
      .mf-section {
        margin-top: 0;
      }
      ol {
        margin: 0;
        padding: 0;
        list-style: none;
        counter-reset: step;
        display: flex;
        flex-direction: column;
        gap: 16px;
      }
      li {
        counter-increment: step;
        position: relative;
        padding-left: 30px;
      }
      li::before {
        content: counter(step);
        position: absolute;
        left: 0;
        top: 0;
        width: 20px;
        height: 20px;
        display: flex;
        align-items: center;
        justify-content: center;
        border-radius: var(--mf-radius-xs);
        background: var(--mf-accent-soft);
        color: var(--mf-accent-text);
        font: var(--mf-text-eyebrow);
      }
      .step-title {
        font-weight: var(--mf-fw-semibold);
        margin-bottom: 6px;
      }
      .cmd,
      .secret-once-value {
        display: flex;
        align-items: center;
        gap: 8px;
      }
      .cmd code,
      .secret-once-value code {
        flex: 1;
        min-width: 0;
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-sm);
        background: var(--mf-code-bg);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 7px 10px;
        overflow-wrap: anywhere;
      }
      .mf-hint {
        margin: 6px 0 0;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
      }
      .mf-hint code {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
      }
      .tokens .mf-hint {
        margin-bottom: 12px;
      }
      .secret-once {
        border: 1px solid var(--mf-warn);
        background: var(--mf-warn-soft);
        border-radius: var(--mf-radius-sm);
        padding: 14px;
        margin-bottom: 14px;
        display: flex;
        flex-direction: column;
        gap: 8px;
      }
      .secret-once .mf-hint {
        margin: 0;
        color: var(--mf-warn-text);
      }
      .secret-once-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
      }
      .prefix,
      .when {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
      }
    `,
  ],
})
export class WorkConnectPanelComponent implements OnInit {
  private work = inject(WorkService);
  private toast = inject(ToastService);

  @Input({ required: true }) businessId = '';
  @Input({ required: true }) project!: WorkProject;

  private createBtn = viewChild<ElementRef<HTMLButtonElement>>('createBtn');

  readonly origin = signal(typeof window !== 'undefined' ? window.location.origin : '');
  readonly slug = signal('');
  readonly installCmd = computed(() => `curl -fsSL ${this.origin()}/cli/install.sh | sh`);
  readonly initCmd = computed(() => `mf init --server ${this.origin()} --project ${this.slug()}`);
  // The server is pinned explicitly: the CLI refuses a login that doesn't name one.
  readonly loginCmd = computed(() => this.loginFor('mfw_…'));

  tokens = signal<WorkToken[]>([]);
  // Token management needs work.write; a read-only member gets 404 from the list and sees the
  // setup steps without the token panel.
  canManage = signal(true);
  created = signal<{ id: string; token: string } | null>(null);
  creating = signal(false);
  confirming = signal('');
  busy = signal('');
  newName = '';

  ngOnInit(): void {
    this.slug.set(this.project.slug);
    this.reload();
  }

  reload(): void {
    this.work.listTokens(this.businessId, this.project.id).subscribe({
      next: (r) => this.tokens.set(r.items ?? []),
      error: (e: HttpErrorResponse) => {
        if (e.status === 403 || e.status === 404) this.canManage.set(false);
        else this.toast.error('Could not load work tokens');
      },
    });
  }

  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    this.creating.set(true);
    this.work.createToken(this.businessId, this.project.id, name).subscribe({
      next: (t) => {
        this.creating.set(false);
        this.newName = '';
        this.created.set({ id: t.id, token: t.token });
        const { token: _secret, ...row } = t;
        this.tokens.set([{ ...row, last_used_at: null, revoked_at: null }, ...this.tokens()]);
        this.toast.success('Token created');
      },
      error: (e: HttpErrorResponse) => {
        this.creating.set(false);
        this.toast.error(describeWorkMutation(e, 'Could not create the token'));
      },
    });
  }

  // The plaintext is not kept anywhere else; once dismissed only a new token can show one.
  dismiss(): void {
    this.created.set(null);
    this.createBtn()?.nativeElement.focus();
  }

  revoke(t: WorkToken): void {
    if (this.busy()) return;
    this.busy.set(t.id);
    this.work.revokeToken(this.businessId, this.project.id, t.id).subscribe({
      next: () => {
        this.busy.set('');
        this.confirming.set('');
        if (this.created()?.id === t.id) this.created.set(null);
        this.tokens.set(
          this.tokens().map((x) =>
            x.id === t.id ? { ...x, revoked_at: new Date().toISOString() } : x,
          ),
        );
        this.toast.success('Token revoked');
      },
      error: (e: HttpErrorResponse) => {
        this.busy.set('');
        this.toast.error(describeWorkMutation(e, 'Could not revoke the token'));
      },
    });
  }

  loginFor(token: string): string {
    return `mf login --server ${this.origin()} --token ${token}`;
  }

  copy(text: string, what: string): void {
    void navigator.clipboard?.writeText(text).then(
      () => this.toast.success(`${what} copied`),
      () => this.toast.error('Could not copy'),
    );
  }
}
