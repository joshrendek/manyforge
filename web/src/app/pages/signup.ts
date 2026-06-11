import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth.service';

@Component({
  selector: 'app-signup',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="mf-card" style="max-width:410px;margin:8vh auto 0">
      @if (step() === 'form') {
        <h1>Create your account</h1>
        <p class="mf-pageheader-sub">Start your ManyForge workspace.</p>
        <form (ngSubmit)="signup()">
          <div class="mf-field">
            <label for="email">Email</label>
            <input class="mf-input" id="email" type="email" name="email" [(ngModel)]="email" autocomplete="email" required data-testid="signup-email" />
          </div>
          <div class="mf-field">
            <label for="displayName">Display name</label>
            <input class="mf-input" id="displayName" type="text" name="displayName" [(ngModel)]="displayName" required data-testid="signup-display-name" />
          </div>
          <div class="mf-field">
            <label for="password">Password</label>
            <input class="mf-input" id="password" type="password" name="password" [(ngModel)]="password" autocomplete="new-password" required data-testid="signup-password" />
          </div>
          <button class="mf-btn mf-btn-primary" type="submit" [disabled]="loading()" data-testid="signup-submit">{{ loading() ? 'Creating…' : 'Create account' }}</button>
        </form>
        @if (error()) { <p class="mf-err" data-testid="signup-error">{{ error() }}</p> }
        <p>Already have an account? <a class="mf-btn-link" routerLink="/login">Sign in</a></p>
      } @else {
        <h1>Verify your email</h1>
        <p class="mf-pageheader-sub">We sent a verification link to <b>{{ email }}</b>.</p>
        <form (ngSubmit)="verify()">
          <div class="mf-field">
            <label for="token">Verification token</label>
            <input class="mf-input" id="token" type="text" name="token" [(ngModel)]="token" required data-testid="signup-token" />
          </div>
          <button class="mf-btn mf-btn-primary" type="submit" [disabled]="loading()" data-testid="signup-verify-submit">{{ loading() ? 'Verifying…' : 'Verify email' }}</button>
        </form>
        <p class="mf-hint">Local dev: grab the token from the API server log line <code>dev mailer: would send … body=token: …</code></p>
        @if (error()) { <p class="mf-err" data-testid="signup-error">{{ error() }}</p> }
      }
    </div>
  `,
})
export class SignupComponent {
  private auth = inject(AuthService);
  private router = inject(Router);

  email = '';
  displayName = '';
  password = '';
  token = '';
  step = signal<'form' | 'verify'>('form');
  loading = signal(false);
  error = signal('');

  signup(): void {
    this.loading.set(true);
    this.error.set('');
    this.auth.signup(this.email, this.displayName, this.password).subscribe({
      next: () => {
        this.loading.set(false);
        this.step.set('verify');
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not create the account. Check your details and try again.');
      },
    });
  }

  verify(): void {
    this.loading.set(true);
    this.error.set('');
    this.auth.verify(this.token).subscribe({
      next: () => {
        this.loading.set(false);
        void this.router.navigateByUrl('/login');
      },
      error: () => {
        this.loading.set(false);
        this.error.set('That token is invalid or expired.');
      },
    });
  }
}
