import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth.service';

@Component({
  selector: 'app-login',
  imports: [FormsModule, RouterLink],
  template: `
    <section class="card auth">
      <h1>Welcome back</h1>
      <p class="sub">Sign in to your ManyForge workspace.</p>
      <form (ngSubmit)="submit()">
        <label for="email">Email</label>
        <input id="email" type="email" name="email" [(ngModel)]="email" autocomplete="email" required />
        <label for="password">Password</label>
        <input id="password" type="password" name="password" [(ngModel)]="password" autocomplete="current-password" required />
        <button type="submit" [disabled]="loading()">{{ loading() ? 'Signing in…' : 'Sign in' }}</button>
      </form>
      @if (error()) { <p class="msg error">{{ error() }}</p> }
      <p class="switch">New here? <a routerLink="/signup">Create an account</a></p>
    </section>
  `,
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
