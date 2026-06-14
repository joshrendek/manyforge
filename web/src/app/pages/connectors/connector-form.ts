import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnInit, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Connector, ConnectorsService } from '../../core/connectors.service';

// Reusable connector form. mode='create' collects the full connector + credential bundle;
// mode='rotate' collects ONLY the credential bundle (for an existing connector). Credential
// fields are type=password and are write-only — they are sent to the API but the API never
// returns them, so the form starts blank every time (no prefill on rotate).
// mode='edit' collects display_name + config (project_key, issue_type) for an existing connector;
// prefills from the connector input and PATCHes on submit.
@Component({
  selector: 'app-connector-form',
  imports: [FormsModule],
  template: `
    <form class="mf-add-form" data-testid="connector-form" (ngSubmit)="submit()">
      @if (mode === 'create') {
        <div class="mf-field" style="flex:0 1 160px">
          <label for="conn-type">Type</label>
          <select id="conn-type" class="mf-select" data-testid="conn-type"
                  [ngModel]="type()" (ngModelChange)="type.set($event)" name="type" [disabled]="submitting()">
            <option value="jira">Jira</option>
            <option value="zendesk">Zendesk</option>
          </select>
        </div>
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-display-name">Display name</label>
          <input id="conn-display-name" type="text" class="mf-input" data-testid="conn-display-name"
                 [(ngModel)]="displayName" name="display_name" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:1 1 240px">
          <label for="conn-base-url">Base URL</label>
          <input id="conn-base-url" type="url" class="mf-input" data-testid="conn-base-url"
                 placeholder="https://acme.atlassian.net" [(ngModel)]="baseUrl" name="base_url"
                 autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-project-key">Project key</label>
          <input id="conn-project-key" type="text" class="mf-input" data-testid="conn-project-key"
                 placeholder="PROJ" [(ngModel)]="projectKey" name="project_key" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-issue-type">Issue type</label>
          <input id="conn-issue-type" type="text" class="mf-input" data-testid="conn-issue-type"
                 placeholder="Task" [(ngModel)]="issueType" name="issue_type" autocomplete="off" [disabled]="submitting()" />
        </div>
      }

      @if (mode === 'edit') {
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-display-name">Display name</label>
          <input id="conn-display-name" type="text" class="mf-input" data-testid="conn-display-name"
                 [(ngModel)]="displayName" name="display_name" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-project-key">Project key</label>
          <input id="conn-project-key" type="text" class="mf-input" data-testid="conn-project-key"
                 placeholder="PROJ" [(ngModel)]="projectKey" name="project_key" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:0 1 140px">
          <label for="conn-issue-type">Issue type</label>
          <input id="conn-issue-type" type="text" class="mf-input" data-testid="conn-issue-type"
                 placeholder="Task" [(ngModel)]="issueType" name="issue_type" autocomplete="off" [disabled]="submitting()" />
        </div>
      }

      @if (mode !== 'rotate') {
        <div class="mf-field" style="flex:1 1 100%">
          <label for="conn-suppress-native" style="display:flex;align-items:center;gap:8px;cursor:pointer">
            <input id="conn-suppress-native" type="checkbox" data-testid="conn-suppress-native"
                   [(ngModel)]="suppressNativeNotifications" name="suppress_native_notifications" [disabled]="submitting()" />
            Suppress native email notifications
          </label>
          <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">When enabled, replies on this connector's tickets are mirrored to the external system only — no native email is sent.</span>
        </div>
      }

      @if (mode !== 'edit') {
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-email">Email</label>
          <input id="conn-email" type="email" class="mf-input" data-testid="conn-email"
                 [(ngModel)]="email" name="email" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-api-token">API token</label>
          <input id="conn-api-token" type="password" class="mf-input" data-testid="conn-api-token"
                 placeholder="••••••••" [(ngModel)]="apiToken" name="api_token" autocomplete="off" [disabled]="submitting()" />
        </div>
        <div class="mf-field" style="flex:1 1 200px">
          <label for="conn-webhook-secret">Webhook secret</label>
          <input id="conn-webhook-secret" type="password" class="mf-input" data-testid="conn-webhook-secret"
                 placeholder="••••••••" [(ngModel)]="webhookSecret" name="webhook_secret" autocomplete="off" [disabled]="submitting()" />
          <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">Never shown again — save it somewhere safe.</span>
        </div>
      }

      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connector-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : (mode === 'create' ? 'Connect' : mode === 'edit' ? 'Save' : 'Rotate credential') }}
        </button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="connector-form-cancel"
                (click)="cancelled.emit()" [disabled]="submitting()">Cancel</button>
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="connector-form-error" style="flex:1 1 100%">{{ error() }}</p>
      }
    </form>
  `,
})
export class ConnectorFormComponent implements OnInit {
  @Input() businessId = '';
  @Input() mode: 'create' | 'rotate' | 'edit' = 'create';
  @Input() connectorId = '';
  @Input() connector: Connector | null = null;
  @Output() saved = new EventEmitter<Connector>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(ConnectorsService);

  type = signal<'jira' | 'zendesk'>('jira');
  displayName = '';
  baseUrl = '';
  projectKey = '';
  issueType = '';
  email = '';
  apiToken = '';
  webhookSecret = '';
  suppressNativeNotifications = false;

  submitting = signal(false);
  error = signal('');

  ngOnInit(): void {
    if (this.mode === 'edit' && this.connector) {
      this.displayName = this.connector.display_name;
      this.projectKey = (this.connector.config['project_key'] as string) ?? '';
      this.issueType = (this.connector.config['issue_type'] as string) ?? '';
      this.suppressNativeNotifications = this.connector.suppress_native_notifications;
    }
  }

  valid(): boolean {
    if (this.mode === 'edit') return !!this.displayName.trim();
    if (!this.email.trim() || !this.apiToken.trim()) return false;
    if (this.mode === 'create') return !!this.displayName.trim() && !!this.baseUrl.trim();
    return true;
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    let obs;
    if (this.mode === 'edit') {
      obs = this.api.update(this.businessId, this.connectorId, {
        display_name: this.displayName.trim(),
        config: this.buildConfig(),
        suppress_native_notifications: this.suppressNativeNotifications,
      });
    } else if (this.mode === 'create') {
      obs = this.api.create(this.businessId, {
        type: this.type(),
        display_name: this.displayName.trim(),
        base_url: this.baseUrl.trim(),
        suppress_native_notifications: this.suppressNativeNotifications,
        email: this.email.trim(),
        api_token: this.apiToken,
        webhook_secret: this.webhookSecret || undefined,
        config: this.buildConfig(),
      });
    } else {
      obs = this.api.rotate(this.businessId, this.connectorId, {
        email: this.email.trim(),
        api_token: this.apiToken,
        webhook_secret: this.webhookSecret || undefined,
      });
    }
    obs.subscribe({
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

  private buildConfig(): Record<string, unknown> {
    const cfg: Record<string, unknown> = {};
    if (this.projectKey.trim()) cfg['project_key'] = this.projectKey.trim();
    if (this.issueType.trim()) cfg['issue_type'] = this.issueType.trim();
    return cfg;
  }

  private reset(): void {
    this.displayName = '';
    this.baseUrl = '';
    this.projectKey = '';
    this.issueType = '';
    this.email = '';
    this.apiToken = '';
    this.webhookSecret = '';
    this.suppressNativeNotifications = false;
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'A connector for that system already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
