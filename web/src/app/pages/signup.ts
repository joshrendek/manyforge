import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth.service';

@Component({
  selector: 'app-signup',
  imports: [FormsModule, RouterLink],
  template: `
    <section class="card auth">
      @if (step() === 'form') {
        <h1>Create your account</h1>
        <p class="sub">Start your ManyForge workspace.</p>
        <form (ngSubmit)="signup()">
          <label for="email">Email</label>
          <input id="email" type="email" name="email" [(ngModel)]="email" autocomplete="email" required />
          <label for="displayName">Display name</label>
          <input id="displayName" type="text" name="displayName" [(ngModel)]="displayName" required />
          <label for="password">Password</label>
          <input id="password" type="password" name="password" [(ngModel)]="password" autocomplete="new-password" required />
          <button type="submit" [disabled]="loading()">{{ loading() ? 'Creating…' : 'Create account' }}</button>
        </form>
        @if (error()) { <p class="msg error">{{ error() }}</p> }
        <p class="switch">Already have an account? <a routerLink="/login">Sign in</a></p>
      } @else {
        <h1>Verify your email</h1>
        <p class="sub">We sent a verification link to <b>{{ email }}</b>.</p>
        <form (ngSubmit)="verify()">
          <label for="token">Verification token</label>
          <input id="token" type="text" name="token" [(ngModel)]="token" required />
          <button type="submit" [disabled]="loading()">{{ loading() ? 'Verifying…' : 'Verify email' }}</button>
        </form>
        <p class="hint">Local dev: grab the token from the API server log line <code>dev mailer: would send … body=token: …</code></p>
        @if (error()) { <p class="msg error">{{ error() }}</p> }
      }
    </section>
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
