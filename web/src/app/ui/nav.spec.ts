import { describe, expect, it } from 'vitest';
import { NAV_ITEMS } from './nav';

describe('NAV_ITEMS', () => {
  it('includes dashboard, support, approvals, connectors, ai credentials, mcp and accounting with testids', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/approvals', '/credentials/connector', '/credentials/ai', '/mcp', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/approvals')?.testid).toBe('nav-approvals');
    expect(NAV_ITEMS.find((n) => n.route === '/credentials/connector')?.testid).toBe('nav-connectors');
    expect(NAV_ITEMS.find((n) => n.route === '/credentials/ai')?.testid).toBe('nav-ai-credentials');
    expect(NAV_ITEMS.find((n) => n.route === '/mcp')?.testid).toBe('nav-mcp');
    expect(NAV_ITEMS.find((n) => n.route === '/accounting')?.testid).toBe('nav-accounting');
  });
});
