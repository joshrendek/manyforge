import {
  BarChart3, Bot, Building2, CheckCheck, GitBranch, GitPullRequest, Inbox, KeyRound,
  LayoutGrid, LucideIconData, Mail, MessageSquare, Plug, Receipt, Send, Server,
  SlidersHorizontal, Users, Workflow,
} from 'lucide-angular';

export interface NavItem {
  label: string;
  route: string;
  testid: string;
  icon: LucideIconData;
  badge?: number;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  { label: 'Workspace', items: [
    { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard', icon: LayoutGrid },
    { label: 'Support', route: '/support', testid: 'nav-support', icon: Inbox },
    { label: 'Approvals', route: '/approvals', testid: 'nav-approvals', icon: CheckCheck },
  ] },
  { label: 'Agents & Review', items: [
    { label: 'Agents', route: '/agents', testid: 'nav-agents', icon: Bot },
    { label: 'Code Review', route: '/code-review', testid: 'nav-code-review', icon: GitPullRequest },
    { label: 'Review Setup', route: '/code-review/setup', testid: 'nav-review-setup', icon: SlidersHorizontal },
    { label: 'Connectors', route: '/credentials/connector', testid: 'nav-connectors', icon: Plug },
    { label: 'AI Credentials', route: '/credentials/ai', testid: 'nav-ai-credentials', icon: KeyRound },
    { label: 'MCP', route: '/mcp', testid: 'nav-mcp', icon: Server },
  ] },
  { label: 'CRM', items: [
    { label: 'Contacts', route: '/crm/contacts', testid: 'nav-crm-contacts', icon: Users },
    { label: 'Companies', route: '/crm/companies', testid: 'nav-crm-companies', icon: Building2 },
    { label: 'Feedback', route: '/feedback', testid: 'nav-feedback', icon: MessageSquare },
  ] },
  { label: 'Growth', items: [
    { label: 'Mailing', route: '/mailing/lists', testid: 'nav-mailing', icon: Mail },
    { label: 'Campaigns', route: '/mailing/campaigns', testid: 'nav-mailing-campaigns', icon: Send },
    { label: 'Automations', route: '/mailing/automations', testid: 'nav-mailing-automations', icon: Workflow },
    { label: 'Analytics', route: '/analytics', testid: 'nav-analytics', icon: BarChart3 },
  ] },
  { label: 'Admin', items: [
    { label: 'GitHub', route: '/settings/github', testid: 'nav-github', icon: GitBranch },
    { label: 'Accounting', route: '/accounting', testid: 'nav-accounting', icon: Receipt },
  ] },
];

export const NAV_ITEMS: NavItem[] = NAV_GROUPS.flatMap((group) => group.items);
