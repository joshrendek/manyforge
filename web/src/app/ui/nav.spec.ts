import { describe, expect, it } from 'vitest';
import { NAV_ITEMS } from './nav';

describe('NAV_ITEMS', () => {
  it('includes dashboard, support and accounting with testids', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/accounting')?.testid).toBe('nav-accounting');
  });
});
