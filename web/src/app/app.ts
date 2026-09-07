import { Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink, RouterOutlet } from '@angular/router';
import { LucideAngularModule } from 'lucide-angular';
import { EMPTY, Subscription, catchError, filter, switchMap, timer } from 'rxjs';
import { ApprovalsService } from './core/approvals.service';
import { AuthService, Profile } from './core/auth.service';
import { BusinessService } from './core/business.service';
import { ConnectorsService } from './core/connectors.service';
import { CurrentBusinessService } from './core/current-business.service';
import { NAV_GROUPS, NAV_ITEMS } from './ui/nav';
import { ThemeToggle } from './ui/theme-toggle/theme-toggle';
import { ToastHost } from './ui/toast/toast';
import { ToastService } from './ui/toast/toast.service';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, LucideAngularModule, ThemeToggle, ToastHost],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App {
  private auth = inject(AuthService);
  private router = inject(Router);
  private approvals = inject(ApprovalsService);
  private connectors = inject(ConnectorsService);
  private businessApi = inject(BusinessService);
  private toasts = inject(ToastService);
  readonly currentBusiness = inject(CurrentBusinessService);
  readonly profile = signal<Profile | null>(null);
  readonly businessesLoading = signal(false);
  private businessRequest?: Subscription;
  readonly businessesError = signal('');
  readonly switchingBusiness = signal(false);
  readonly navOpen = signal(false);
  private currentUrl = signal(this.router.url);

  readonly navGroupsWithBadge = computed(() => {
    const approvals = this.approvals.pendingCount();
    const degraded = this.connectors.degradedCount();
    const hasBusiness = !!this.currentBusiness.businessId();
    return NAV_GROUPS.map((group) => ({ ...group, items: group.items.map((item) => {
      const badge = item.route === '/approvals' ? approvals
        : item.route === '/credentials/connector' ? degraded : 0;
      return hasBusiness && badge > 0 ? { ...item, badge } : item;
    }) }));
  });

  readonly portalRoute = computed(() => /^\/(p|m)\//.test(this.currentUrl()));
  readonly showShell = computed(() => this.auth.isAuthenticated()
    && !/^\/(login|signup)(\/|\?|$)/.test(this.currentUrl()) && !this.portalRoute());
  readonly activeRoute = computed(() => {
    const url = this.currentUrl().split(/[?#]/)[0];
    const destination = this.scopedListRoute() ?? url;
    if (/^\/mailing\/(templates|sending|suppression)(\/|$)/.test(destination)) return '/mailing/lists';
    let active: string | undefined;
    for (const item of NAV_ITEMS) {
      if ((destination === item.route || destination.startsWith(item.route + '/'))
        && item.route.length > (active?.length ?? 0)) active = item.route;
    }
    return active;
  });

  constructor() {
    this.router.events.pipe(
      filter((e): e is NavigationEnd => e instanceof NavigationEnd),
      takeUntilDestroyed(),
    ).subscribe((e) => {
      this.currentUrl.set(e.urlAfterRedirects);
      this.navOpen.set(false);
      const id = this.routeBusinessId();
      if (id && this.auth.isAuthenticated()) this.currentBusiness.set(id);
    });

    // Authentication is reactive: signing in must populate the shell without a reload.
    effect((onCleanup) => {
      const authenticated = this.auth.isAuthenticated();
      untracked(() => {
        if (!authenticated) {
          this.profile.set(null);
          this.currentBusiness.clear();
          this.businessesError.set('');
          this.businessesLoading.set(false);
          return;
        }
        const requests = new Subscription();
        requests.add(this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => {} }));
        requests.add(this.loadBusinesses());
        onCleanup(() => {
          requests.unsubscribe();
          this.businessRequest?.unsubscribe();
        });
      });
    });

    effect((onCleanup) => {
      const authenticated = this.auth.isAuthenticated();
      const id = this.currentBusiness.businessId();
      untracked(() => {
        this.approvals.pendingCount.set(0);
        this.connectors.degradedCount.set(0);
      });
      if (!authenticated || !id) return;
      const requests = new Subscription();
      requests.add(timer(0, 20000).pipe(switchMap(() => this.approvals.listPending(id).pipe(
        catchError(() => { this.approvals.pendingCount.set(0); return EMPTY; }),
      ))).subscribe());
      requests.add(timer(0, 20000).pipe(switchMap(() => this.connectors.list(id).pipe(
        catchError(() => { this.connectors.degradedCount.set(0); return EMPTY; }),
      ))).subscribe());
      onCleanup(() => requests.unsubscribe());
    });
  }

  loadBusinesses(): Subscription {
    this.businessesLoading.set(true);
    this.businessRequest?.unsubscribe();
    this.businessesError.set('');
    this.businessRequest = this.businessApi.list().subscribe({
      next: ({ items }) => {
        const businesses = items ?? [];
        this.currentBusiness.replaceBusinesses(businesses);
        const routeId = this.routeBusinessId();
        if (routeId && businesses.some((b) => b.id === routeId)) this.currentBusiness.set(routeId);
        this.businessesLoading.set(false);
      },
      error: (error) => {
        this.businessesLoading.set(false);
        this.businessesError.set(error.status === 403 || error.status === 404
          ? "You don't have access to do that." : "We couldn't load your businesses.");
      },
    });
    return this.businessRequest;
  }

  async switchBusiness(event: Event): Promise<void> {
    const select = event.target as HTMLSelectElement;
    const id = select.value;
    // Keep the select on the committed context while a canDeactivate guard is pending.
    select.value = this.currentBusiness.businessId() ?? '';
    if (!id || id === this.currentBusiness.businessId() || this.switchingBusiness()) return;
    this.switchingBusiness.set(true);
    try {
      const list = this.scopedListRoute();
      if (list && !(await this.router.navigateByUrl(list))) return;
      if (this.auth.isAuthenticated()) this.currentBusiness.set(id);
    } catch {
      this.toasts.error("We couldn't switch businesses. Try again.");
    } finally {
      this.switchingBusiness.set(false);
      select.value = this.currentBusiness.businessId() ?? '';
    }
  }

  logout(): void {
    this.currentBusiness.clear();
    this.auth.logout().subscribe({
      next: () => { void this.router.navigateByUrl('/login'); },
      error: () => { void this.router.navigateByUrl('/login'); },
    });
  }

  private routeBusinessId(): string | null {
    let route = this.router.routerState.snapshot.root;
    while (route.firstChild) route = route.firstChild;
    return route.paramMap.get('businessId');
  }

  private scopedListRoute(): string | null {
    let route = this.router.routerState.snapshot.root;
    while (route.firstChild) route = route.firstChild;
    if (!route.paramMap.has('businessId')) return null;
    const path = route.routeConfig?.path ?? '';
    if (path.startsWith('crm/')) return '/crm/contacts';
    if (path.startsWith('mailing/')) return '/mailing/' + path.split('/')[2];
    return '/' + path.split('/')[0];
  }
}
