import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth.service';

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
        <button class="mf-btn mf-btn-primary" type="submit" [disabled]="loading()" data-testid="login-submit">{{ loading() ? 'Signing in…' : 'Sign in' }}</button>
      </form>
      @if (error()) { <p class="mf-err" data-testid="login-error">{{ error() }}</p> }
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

  email = '';
  password = '';
  loading = signal(false);
  error = signal('');

  submit(): void {
    this.loading.set(true);
    this.error.set('');
    this.auth.login(this.email, this.password).subscribe({
      next: () => {
        this.loading.set(false);
        void this.router.navigateByUrl('/dashboard');
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Invalid email or password.');
      },
    });
  }
}
