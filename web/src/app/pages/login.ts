import { Component, DestroyRef, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { Observable, tap } from 'rxjs';
import { AuthService, TokenPair } from '../core/auth.service';
import { PasskeyService, supportsPasskeys } from '../core/passkey.service';

@Component({
  selector: 'app-login',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="mf-card" style="max-width:410px;margin:8vh auto 0">
      <div class="mf-eyebrow">Welcome back</div>
      <h1>Sign in to your workspace</h1>
      <p class="mf-pageheader-sub">The forge is lit. Pick up where you left off.</p>
      <form class="auth-form" (ngSubmit)="submit()">
        <div class="mf-field">
          <label for="email">Email</label>
          <input class="mf-input" id="email" type="email" name="email" [(ngModel)]="email" autocomplete="email" required data-testid="login-email" />
        </div>
        <div class="mf-field">
          <label for="password">Password</label>
          <input class="mf-input" id="password" type="password" name="password" [(ngModel)]="password" autocomplete="current-password" required data-testid="login-password" />
        </div>
        <button class="mf-btn mf-btn-primary" type="submit" [disabled]="loading() !== null" data-testid="login-submit">{{ loading() === 'password' ? 'Signing in…' : 'Sign in' }}</button>
        @if (passkeysAvailable()) {
          <button class="mf-btn mf-btn-ghost" type="button" [disabled]="loading() !== null"
            (click)="signInWithPasskey()" data-testid="login-passkey">
            {{ loading() === 'passkey' ? 'Waiting for passkey…' : 'Sign in with a passkey' }}
          </button>
        }
      </form>
      @if (error()) { <p class="mf-err" role="alert" data-testid="login-error">{{ error() }}</p> }
      <p class="auth-footer">New here? <a class="mf-btn-link" routerLink="/signup">Create an account</a></p>
    </div>
  `,
  styles: [`
    h1 { font: var(--mf-text-h1); letter-spacing: -.02em; margin: 0; }
    .mf-pageheader-sub { margin-bottom: 18px; }
    .auth-form { display: grid; gap: 14px; }
    .auth-form .mf-btn { justify-content: center; }
    .auth-footer { font-size: 13px; color: var(--mf-text-muted); margin: 18px 0 0; }
  `],
})
export class LoginComponent {
  private auth = inject(AuthService);
  private router = inject(Router);
  private route = inject(ActivatedRoute);
  private passkeys = inject(PasskeyService);
  private destroyRef = inject(DestroyRef);
  private passkeyPrompt = new AbortController();

  email = '';
  password = '';
  loading = signal<'password' | 'passkey' | null>(null);
  error = signal('');
  readonly passkeysAvailable = signal(false);

  constructor() {
    this.destroyRef.onDestroy(() => this.passkeyPrompt.abort());
    if (supportsPasskeys()) {
      this.passkeys.available().pipe(takeUntilDestroyed()).subscribe({
        next: (enabled) => this.passkeysAvailable.set(enabled),
        error: () => {},
      });
    }
  }

  submit(): void {
    if (this.loading()) return;
    this.signIn('password', this.auth.login(this.email, this.password));
  }

  signInWithPasskey(): void {
    if (this.loading()) return;
    this.signIn('passkey', this.passkeys.login(this.passkeyPrompt.signal).pipe(
      tap((pair) => this.auth.startSession(pair)),
    ));
  }

  private signIn(method: 'password' | 'passkey', request: Observable<TokenPair>): void {
    this.loading.set(method);
    this.error.set('');
    request.pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: () => {
        this.loading.set(null);
        const destination = this.route.snapshot.queryParamMap.get('returnTo') === 'security'
          ? '/account/security' : '/dashboard';
        void this.router.navigateByUrl(destination);
      },
      error: (err: unknown) => {
        this.loading.set(null);
        if (method === 'password') {
          this.error.set('Could not sign in with email and password. If you added a passkey, use it to sign in.');
        } else if (err instanceof DOMException && err.name === 'NotAllowedError') {
          this.error.set('No passkey was selected. Try again.');
        } else {
          this.error.set('Could not sign in with that passkey. Try another passkey for your account.');
        }
      },
    });
  }
}
