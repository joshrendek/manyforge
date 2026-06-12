export interface NavItem { label: string; route: string; testid: string; badge?: number; }

export const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard' },
  { label: 'Support', route: '/support', testid: 'nav-support' },
  { label: 'Approvals', route: '/approvals', testid: 'nav-approvals' },
  { label: 'Connectors', route: '/connectors', testid: 'nav-connectors' },
  { label: 'Accounting', route: '/accounting', testid: 'nav-accounting' },
];
