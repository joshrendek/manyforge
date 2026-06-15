import { Component, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import {
  NavigationEnd,
  Router,
  RouterLink,
  RouterLinkActive,
  RouterOutlet,
} from '@angular/router';
import { filter } from 'rxjs';
import { ApprovalsService } from './core/approvals.service';
import { AuthService, Profile } from './core/auth.service';
import { ConnectorsService } from './core/connectors.service';
import { CurrentBusinessService } from './core/current-business.service';
import { NAV_ITEMS } from './ui/nav';
import { ThemeToggle } from './ui/theme-toggle/theme-toggle';
import { ToastHost } from './ui/toast/toast';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, ThemeToggle, ToastHost],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App implements OnInit, OnDestroy {
  private auth = inject(AuthService);
  private router = inject(Router);
  private approvals = inject(ApprovalsService);
  private connectors = inject(ConnectorsService);
  private currentBusiness = inject(CurrentBusinessService);

  private badgeTimer?: ReturnType<typeof setInterval>;

  readonly profile = signal<Profile | null>(null);

  // Copy NAV_ITEMS (object spread — never mutate the shared array) and stamp the live
  // pending-approvals count and degraded-connector count onto their nav items for the
  // current business.
  readonly navItemsWithBadge = computed(() => {
    const approvals = this.approvals.pendingCount();
    const degraded = this.connectors.degradedCount();
    const hasBiz = !!this.currentBusiness.businessId();
    return NAV_ITEMS.map((item) => {
      if (item.route === '/approvals' && hasBiz && approvals > 0) return { ...item, badge: approvals };
      if (item.route === '/credentials/connector' && hasBiz && degraded > 0) return { ...item, badge: degraded };
      return item;
    });
  });

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

    if (this.auth.isAuthenticated()) {
      const id = this.currentBusiness.businessId();
      if (id) {
        this.approvals.refreshCount(id);
        this.connectors.refreshCount(id);
      }
      // Poll for freshness; read businessId live each tick so a business chosen later (on the
      // approvals page, which persists it) is picked up without an app reload.
      this.badgeTimer = setInterval(() => {
        const b = this.currentBusiness.businessId();
        if (b) {
          this.approvals.refreshCount(b);
          this.connectors.refreshCount(b);
        }
      }, 20000);
    }
  }

  ngOnDestroy(): void {
    if (this.badgeTimer) clearInterval(this.badgeTimer);
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
