import { Component, OnInit, computed, inject, signal } from '@angular/core';
import {
  NavigationEnd,
  Router,
  RouterLink,
  RouterLinkActive,
  RouterOutlet,
} from '@angular/router';
import { filter } from 'rxjs';
import { AuthService, Profile } from './core/auth.service';
import { NAV_ITEMS } from './ui/nav';
import { ThemeToggle } from './ui/theme-toggle/theme-toggle';
import { ToastHost } from './ui/toast/toast';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, ThemeToggle, ToastHost],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App implements OnInit {
  private auth = inject(AuthService);
  private router = inject(Router);

  readonly navItems = NAV_ITEMS;
  readonly profile = signal<Profile | null>(null);

  // The current URL, tracked so the shell can hide itself on the auth screens.
  private currentUrl = signal(this.router.url);

  // Show the persistent sidebar only when authenticated AND not on an auth screen.
  // A logged-in user who navigates to /login or /signup (no guard stops them) must
  // see the bare auth page, not the app shell wrapped around the login form.
  readonly showShell = computed(
    () => this.auth.isAuthenticated() && !this.isAuthRoute(this.currentUrl()),
  );

  constructor() {
    this.router.events
      .pipe(filter((e): e is NavigationEnd => e instanceof NavigationEnd))
      .subscribe((e) => this.currentUrl.set(e.urlAfterRedirects));
  }

  ngOnInit(): void {
    // Best-effort identity for the sidebar footer. A failure here is non-fatal:
    // the interceptor already handles refresh/redirect, so we just leave it blank.
    if (this.auth.isAuthenticated()) {
      this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => {} });
    }
  }

  logout(): void {
    this.auth.logout().subscribe({
      next: () => this.toLogin(),
      error: () => this.toLogin(),
    });
  }

  private isAuthRoute(url: string): boolean {
    return url.startsWith('/login') || url.startsWith('/signup');
  }

  private toLogin(): void {
    this.profile.set(null);
    void this.router.navigateByUrl('/login');
  }
}
