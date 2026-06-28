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
    path: 'approvals',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/approvals/queue').then((m) => m.ApprovalsQueueComponent),
  },
  {
    path: 'credentials',
    pathMatch: 'full',
    redirectTo: 'credentials/ai',
  },
  {
    path: 'credentials/ai',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/credentials/ai/list').then((m) => m.AICredentialsListComponent),
  },
  {
    path: 'agents',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/agents/list').then((m) => m.AgentsListComponent),
  },
  {
    path: 'code-review',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/code-review/list').then((m) => m.CodeReviewListComponent),
  },
  {
    path: 'code-review/:businessId/:id',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/code-review/detail').then((m) => m.CodeReviewDetailComponent),
  },
  {
    path: 'crm/contacts',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/crm/contacts-list').then((m) => m.ContactsListComponent),
  },
  {
    path: 'crm/companies',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/crm/companies-list').then((m) => m.CompaniesListComponent),
  },
  {
    path: 'crm/:businessId/contacts/:id',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/crm/contact-detail').then((m) => m.ContactDetailComponent),
  },
  {
    path: 'credentials/connector',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/connectors/list').then((m) => m.ConnectorsListComponent),
  },
  {
    path: 'mcp',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/mcp/server-list').then((m) => m.McpServerListComponent),
  },
  {
    path: 'mcp/:businessId/:serverId',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/mcp/server-tools').then((m) => m.McpServerToolsComponent),
  },
  {
    path: 'accounting',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/summary').then((m) => m.AccountingSummaryComponent),
  },
  {
    path: 'accounting/:businessId/:agentId',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/agent-runs').then((m) => m.AgentRunsComponent),
  },
  { path: '**', redirectTo: 'dashboard' },
];
