import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { GithubAppService } from '../../core/github-app.service';

// Landing page for GitHub's post-manifest-flow browser redirect
// (https://github.com/settings/apps/.../installations?state=... after the
// manifest conversion). GitHub appends ?code&state to the redirect_url we
// registered; this page reads them and POSTs to the authenticated backend
// endpoint that exchanges the code for the App's credentials (Task 6).
@Component({
  selector: 'app-github-app-created',
  imports: [RouterLink],
  template: `
    <div class="mf-card" data-testid="github-app-created">
      @if (state() === 'working') {
        <p>Finishing GitHub App setup…</p>
      }
      @if (state() === 'done') {
        <p data-testid="gh-success" role="status">GitHub App created. You can now connect your organizations.</p>
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
export class GithubAppCreatedComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(GithubAppService);
  state = signal<'working' | 'done' | 'error'>('working');
  message = signal('');

  ngOnInit(): void {
    const code = this.route.snapshot.queryParamMap.get('code') ?? '';
    const st = this.route.snapshot.queryParamMap.get('state') ?? '';
    if (!code || !st) {
      this.state.set('error');
      this.message.set('Missing setup parameters.');
      return;
    }
    this.api.convertManifest({ code, state: st }).subscribe({
      next: () => this.state.set('done'),
      error: (e: HttpErrorResponse) => {
        this.state.set('error');
        this.message.set(this.describe(e));
      },
    });
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 409) return 'A GitHub App is already configured for this instance.';
    if (e.status === 404) return 'Not authorized to complete this setup.';
    return 'Could not complete GitHub App setup. Please try again.';
  }
}
