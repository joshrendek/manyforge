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
    path: 'tenant-merges/new/:sourceRootId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/tenant-merge').then((m) => m.TenantMergeComponent),
  },
  {
    path: 'tenant-merges/:operationId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/tenant-merge').then((m) => m.TenantMergeComponent),
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
    loadComponent: () =>
      import('./pages/credentials/ai/list').then((m) => m.AICredentialsListComponent),
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
    // Literal 'setup' route must precede the :businessId/:id detail route below, else a
    // /code-review/setup/... URL would bind :businessId='setup'. (Setup is paramless.)
    path: 'code-review/setup',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/code-review/setup').then((m) => m.CodeReviewSetupComponent),
  },
  {
    path: 'code-review/:businessId/:id',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/code-review/detail').then((m) => m.CodeReviewDetailComponent),
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
    path: 'feedback',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/feedback/boards-list').then((m) => m.FeedbackBoardsListComponent),
  },
  {
    path: 'feedback/:businessId/:boardId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/feedback/board-detail').then((m) => m.FeedbackBoardDetailComponent),
  },
  {
    // The landing page is the multi-site grid; site registration moved to /analytics/sites.
    path: 'analytics',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/analytics/overview').then((m) => m.AnalyticsOverviewComponent),
  },
  {
    // Two segments, so this cannot collide with the three-segment dashboard route below.
    path: 'analytics/sites',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/analytics/sites-list').then((m) => m.AnalyticsSitesListComponent),
  },
  {
    path: 'analytics/:businessId/:siteId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/analytics/dashboard').then((m) => m.AnalyticsDashboardComponent),
  },
  {
    // Public, UNAUTHENTICATED feedback portal keyed by a publishable board key. No authGuard:
    // a business links its customers here to submit + upvote feature requests in the browser
    // (the web equivalent of the mobile SDK). Renders with its own bare layout (see app.ts).
    path: 'p/:key',
    loadComponent: () => import('./pages/feedback/portal').then((m) => m.FeedbackPortalComponent),
  },
  {
    path: 'm/s/:key',
    loadComponent: () =>
      import('./pages/mailing/public/subscribe').then((m) => m.MailingSubscribeComponent),
  },
  {
    path: 'm/confirm/:token',
    loadComponent: () =>
      import('./pages/mailing/public/confirm').then((m) => m.MailingConfirmComponent),
  },
  {
    path: 'm/u/:token',
    loadComponent: () =>
      import('./pages/mailing/public/unsubscribe').then((m) => m.MailingUnsubscribeComponent),
  },
  {
    path: 'mailing',
    pathMatch: 'full',
    redirectTo: 'mailing/lists',
  },
  {
    path: 'mailing/lists',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/mailing/lists-list').then((m) => m.MailingListsListComponent),
  },
  {
    path: 'mailing/templates',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/mailing/templates-list').then((m) => m.MailingTemplatesListComponent),
  },
  {
    path: 'mailing/sending',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/mailing/sending-profile').then((m) => m.MailingSendingProfileComponent),
  },
  {
    path: 'mailing/:businessId/lists/:listId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/mailing/list-detail').then((m) => m.MailingListDetailComponent),
  },
  {
    path: 'mailing/:businessId/templates/:templateId',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/mailing/template-editor').then((m) => m.MailingTemplateEditorComponent),
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
    loadComponent: () =>
      import('./pages/accounting/summary').then((m) => m.AccountingSummaryComponent),
  },
  {
    path: 'accounting/:businessId/:agentId',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/agent-runs').then((m) => m.AgentRunsComponent),
  },
  {
    // GitHub's post-manifest-flow browser redirect lands here with ?code&state.
    path: 'settings/github/app-created',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/settings/github-app-created').then((m) => m.GithubAppCreatedComponent),
  },
  {
    // GitHub's post-installation browser redirect lands here with
    // ?code&installation_id&state (or ?setup_action=request&state pending admin approval).
    path: 'settings/github/installed',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/settings/github-installed').then((m) => m.GithubInstalledComponent),
  },
  {
    // Operator/business-admin entry point: create the GitHub App (manifest flow)
    // and connect an organization to a business. Must precede the catch-all.
    path: 'settings/github',
    canActivate: [authGuard],
    loadComponent: () =>
      import('./pages/settings/github-app-settings').then((m) => m.GithubAppSettingsComponent),
  },
  { path: '**', redirectTo: 'dashboard' },
];
