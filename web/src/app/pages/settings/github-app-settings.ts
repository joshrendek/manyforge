import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { Agent, AgentsService } from '../../core/agents.service';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { GithubAppService } from '../../core/github-app.service';
import { Business } from '../../core/tree';
import { PageHeader } from '../../ui/page-header/page-header';

// GitHub App setup page (spec 010): the two START triggers for the flow whose
// landings shipped in spec 009 (github-app-created.ts / github-installed.ts).
//
// Section 1, "Create GitHub App", is an operator-only one-time action: fetch
// the manifest + signed state, then POST them straight to GitHub's App-creation
// form via a form built with document.createElement (never an Angular template
// binding — the manifest is a JSON string and must reach GitHub byte-for-byte,
// not HTML-escaped/mangled). GitHub redirects back to /settings/github/app-created.
//
// Section 2, "Connect an organization", lets a business admin pick one of the
// business's agents (any agent — the codebase has no "coding agent" subtype;
// code-review/list.ts's own agent picker draws from the same unfiltered
// AgentsService.list()) and mints an install URL to hand off to GitHub. GitHub
// redirects back to /settings/github/installed.
@Component({
  selector: 'app-github-app-settings',
  imports: [FormsModule, RouterLink, PageHeader],
  template: `
    <div class="mf-card" data-testid="github-app-settings-page">
      <mf-page-header title="GitHub App" subtitle="Create the GitHub App and connect organizations for PR review." />

      <h3 style="margin:0 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
        Create GitHub App
      </h3>
      <p style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm);margin:0 0 12px">
        Creates the GitHub App on GitHub from a manifest and redirects you back here when it's
        done. Operator-only, and only needs to be done once per instance.
      </p>
      <div style="display:flex;flex-direction:column;gap:8px;align-items:flex-start;margin-bottom:24px">
        <button type="button" class="mf-btn mf-btn-primary" data-testid="create-app-button"
                [disabled]="creatingApp()" (click)="createApp()">
          {{ creatingApp() ? 'Preparing…' : 'Create GitHub App' }}
        </button>
        @if (createAppError()) {
          <p class="mf-err" data-testid="create-app-error">{{ createAppError() }}</p>
        }
      </div>

      <h3 style="margin:0 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
        Connect an organization
      </h3>
      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="gh-biz-select">Business</label>
          <select id="gh-biz-select" class="mf-select" data-testid="gh-business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        @if (agents().length) {
          <div class="mf-field" style="flex:1 1 220px">
            <label for="gh-agent-select">Review agent</label>
            <select id="gh-agent-select" class="mf-select" data-testid="gh-agent-select"
                    [ngModel]="selectedAgentId()" (ngModelChange)="selectedAgentId.set($event)">
              <option value="" disabled>Choose an agent…</option>
              @for (a of agents(); track a.id) {
                <option [value]="a.id">{{ a.name }}</option>
              }
            </select>
          </div>
        } @else if (businessId()) {
          <p style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="gh-no-agents-hint">
            This business has no agents yet.
            <a routerLink="/agents" data-testid="gh-agents-link">Create one</a>
            to connect a GitHub organization.
          </p>
        }
        <div style="display:flex;align-items:flex-end">
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="connect-github-button"
                  [disabled]="!selectedAgentId() || connecting()" (click)="connect()">
            {{ connecting() ? 'Connecting…' : 'Connect on GitHub' }}
          </button>
        </div>
      </div>
      @if (connectError()) {
        <p class="mf-err" data-testid="connect-error">{{ connectError() }}</p>
      }
    </div>
  `,
})
export class GithubAppSettingsComponent implements OnInit {
  private api = inject(GithubAppService);
  private bizApi = inject(BusinessService);
  private agentsApi = inject(AgentsService);
  private current = inject(CurrentBusinessService);

  creatingApp = signal(false);
  createAppError = signal('');

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  agents = signal<Agent[]>([]);
  selectedAgentId = signal<string>('');
  connecting = signal(false);
  connectError = signal('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.loadAgents(id);
        }
      },
      error: () => {
        // Business list is best-effort context for the connect section; the
        // create-App section above works regardless.
      },
    });
  }

  createApp(): void {
    this.creatingApp.set(true);
    this.createAppError.set('');
    this.api.getManifest().subscribe({
      next: (res) => this.submitManifestForm(res.action_url, res.manifest, res.state),
      error: (e: HttpErrorResponse) => {
        this.creatingApp.set(false);
        this.createAppError.set(this.describeManifestError(e));
      },
    });
  }

  // submitManifestForm builds and submits the App-creation form entirely
  // through the DOM (never an Angular template binding) so the manifest JSON
  // string reaches GitHub exactly as returned, with no HTML-escaping/mangling.
  private submitManifestForm(actionUrl: string, manifest: string, state: string): void {
    const form = document.createElement('form');
    form.method = 'post';
    form.action = `${actionUrl}?state=${encodeURIComponent(state)}`;
    const input = document.createElement('input');
    input.type = 'hidden';
    input.name = 'manifest';
    input.value = manifest;
    form.appendChild(input);
    document.body.appendChild(form);
    form.submit();
  }

  private describeManifestError(e: HttpErrorResponse): string {
    if (e.status === 401) return 'Please sign in to continue.';
    if (e.status === 404) return 'Only the instance operator can create the GitHub App.';
    return 'Could not start GitHub App setup. Please try again.';
  }

  selectBusiness(id: string): void {
    if (!id || id === this.businessId()) return;
    this.businessId.set(id);
    this.current.set(id);
    this.selectedAgentId.set('');
    this.connectError.set('');
    this.loadAgents(id);
  }

  private loadAgents(businessId: string): void {
    this.agentsApi.list(businessId).subscribe({
      next: (r) => {
        if (this.businessId() === businessId) this.agents.set(r.items ?? []);
      },
      error: () => {
        if (this.businessId() === businessId) this.agents.set([]);
      },
    });
  }

  connect(): void {
    const businessId = this.businessId();
    const agentId = this.selectedAgentId();
    if (!businessId || !agentId) return;
    this.connecting.set(true);
    this.connectError.set('');
    this.api.getInstallUrl(businessId, agentId).subscribe({
      next: (res) => {
        window.location.href = res.install_url;
      },
      error: (e: HttpErrorResponse) => {
        this.connecting.set(false);
        this.connectError.set(
          e.status === 404 ? 'Create the GitHub App first, then try again.' : 'Could not start the GitHub connection. Please try again.',
        );
      },
    });
  }
}
