import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { GithubAppService } from '../../core/github-app.service';

// Landing page for GitHub's post-installation browser redirect
// (.../installations/new/... -> our registered redirect URI). GitHub appends
// ?code&installation_id&state (or, when the install needs org-admin approval,
// ?setup_action=request&state with no installation_id/code yet). This page
// reads those params and POSTs to the authenticated backend endpoint that
// verifies the install and links it to the business named in `state` (Task 6);
// business/agent come from the signed state, so the SPA sends no path params.
@Component({
  selector: 'app-github-installed',
  imports: [RouterLink],
  template: `
    <div class="mf-card" data-testid="github-installed">
      @if (state() === 'working') {
        <p>Linking your installation…</p>
      }
      @if (state() === 'pending') {
        <p data-testid="gh-pending">
          Installation is awaiting org-admin approval. Re-run this once approved.
        </p>
      }
      @if (state() === 'done') {
        <p data-testid="gh-linked">
          Installation linked. manyforge will review pull requests for this organization.
        </p>
      }
      @if (state() === 'error') {
        <p class="mf-err" data-testid="gh-error" role="alert">{{ message() }}</p>
      }
      <div>
        <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/settings/github" data-testid="back-to-github-settings">Back to GitHub settings</a>
      </div>
    </div>
  `,
})
export class GithubInstalledComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(GithubAppService);
  state = signal<'working' | 'pending' | 'done' | 'error'>('working');
  message = signal('');

  ngOnInit(): void {
    const q = this.route.snapshot.queryParamMap;
    const code = q.get('code') ?? '';
    const instId = q.get('installation_id') ?? '';
    const st = q.get('state') ?? '';
    if (q.get('setup_action') === 'request' || !instId) {
      this.state.set('pending');
      return;
    }
    if (!code || !st) {
      this.state.set('error');
      this.message.set('Missing installation parameters.');
      return;
    }
    this.api.linkInstallation({ code, installation_id: instId, state: st }).subscribe({
      next: () => this.state.set('done'),
      error: (e: HttpErrorResponse) => {
        this.state.set('error');
        this.message.set(this.describe(e));
      },
    });
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 404)
      return "You don't have permission to link to this business, or the installation couldn't be verified.";
    if (e.status === 409) return 'This link request was already used. Start again from settings.';
    return 'Could not link the installation. Please try again.';
  }
}
