export interface NavItem { label: string; route: string; testid: string; badge?: number; }

export const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard' },
  { label: 'Support', route: '/support', testid: 'nav-support' },
  { label: 'Approvals', route: '/approvals', testid: 'nav-approvals' },
  { label: 'Connectors', route: '/credentials/connector', testid: 'nav-connectors' },
  { label: 'AI Credentials', route: '/credentials/ai', testid: 'nav-ai-credentials' },
  { label: 'Agents', route: '/agents', testid: 'nav-agents' },
  { label: 'Contacts', route: '/crm/contacts', testid: 'nav-crm-contacts' },
  { label: 'Companies', route: '/crm/companies', testid: 'nav-crm-companies' },
  { label: 'MCP', route: '/mcp', testid: 'nav-mcp' },
  { label: 'Accounting', route: '/accounting', testid: 'nav-accounting' },
];
