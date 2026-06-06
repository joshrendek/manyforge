import { Routes } from '@angular/router';
import { authGuard } from './core/auth.guard';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'dashboard' },
  { path: 'login', loadComponent: () => import('./pages/login').then((m) => m.LoginComponent) },
  { path: 'signup', loadComponent: () => import('./pages/signup').then((m) => m.SignupComponent) },
  {
    path: 'dashboard',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/dashboard').then((m) => m.DashboardComponent),
  },
  {
    path: 'support',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/support/ticket-list').then((m) => m.TicketListComponent),
  },
  {
    path: 'support/:businessId/settings/inbox',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/support/inbox-settings').then((m) => m.InboxSettingsComponent),
  },
  {
    path: 'support/:businessId/:tid',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/support/thread-view').then((m) => m.ThreadViewComponent),
  },
  {
    path: 'accounting',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/summary').then((m) => m.AccountingSummaryComponent),
  },
  { path: '**', redirectTo: 'dashboard' },
];
