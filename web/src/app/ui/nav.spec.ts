import { describe, expect, it } from 'vitest';
import { NAV_ITEMS } from './nav';

describe('NAV_ITEMS', () => {
  it('includes dashboard, support, approvals, connectors and accounting with testids', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/approvals', '/connectors', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/approvals')?.testid).toBe('nav-approvals');
    expect(NAV_ITEMS.find((n) => n.route === '/connectors')?.testid).toBe('nav-connectors');
    expect(NAV_ITEMS.find((n) => n.route === '/accounting')?.testid).toBe('nav-accounting');
  });
});
