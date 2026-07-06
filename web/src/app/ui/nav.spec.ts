import { describe, expect, it } from 'vitest';
import { NAV_ITEMS } from './nav';

describe('NAV_ITEMS', () => {
  it('includes dashboard, support, approvals, connectors, ai credentials, agents, code review, review setup, github, contacts, companies, mcp and accounting with testids', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/approvals', '/credentials/connector', '/credentials/ai', '/agents', '/code-review', '/code-review/setup', '/settings/github', '/crm/contacts', '/crm/companies', '/mcp', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/approvals')?.testid).toBe('nav-approvals');
    expect(NAV_ITEMS.find((n) => n.route === '/credentials/connector')?.testid).toBe('nav-connectors');
    expect(NAV_ITEMS.find((n) => n.route === '/credentials/ai')?.testid).toBe('nav-ai-credentials');
    expect(NAV_ITEMS.find((n) => n.route === '/code-review')?.testid).toBe('nav-code-review');
    expect(NAV_ITEMS.find((n) => n.route === '/code-review/setup')?.testid).toBe('nav-review-setup');
    expect(NAV_ITEMS.find((n) => n.route === '/settings/github')?.testid).toBe('nav-github');
    expect(NAV_ITEMS.find((n) => n.route === '/crm/contacts')?.testid).toBe('nav-crm-contacts');
    expect(NAV_ITEMS.find((n) => n.route === '/crm/companies')?.testid).toBe('nav-crm-companies');
    expect(NAV_ITEMS.find((n) => n.route === '/mcp')?.testid).toBe('nav-mcp');
    expect(NAV_ITEMS.find((n) => n.route === '/accounting')?.testid).toBe('nav-accounting');
  });
});
