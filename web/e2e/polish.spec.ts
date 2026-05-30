import { Page, expect, test } from '@playwright/test';

// UI-polish regressions: transparent token refresh and recoverable load errors.
// API mocked via page.route for determinism (mirrors us1/us2 specs).

const profile = { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' };
const master = { id: 'r', parent_id: null, tenant_root_id: 'r', name: 'Acme', status: 'active', is_tenant_root: true };

async function seedAuth(page: Page, access: string) {
  await page.addInitScript((tok) => {
    localStorage.setItem('mf_access', tok);
    localStorage.setItem('mf_refresh', 'good-refresh');
  }, access);
}

test('an expired access token is refreshed transparently and the user stays on the dashboard', async ({ page }) => {
  await seedAuth(page, 'stale-access');
  let refreshCount = 0;

  await page.route('**/api/v1/auth/refresh', (route) => {
    refreshCount++;
    return route.fulfill({ json: { access_token: 'fresh-access', refresh_token: 'good-refresh-2', expires_in: 900 } });
  });
  // Both protected calls reject the stale token and accept the refreshed one.
  const guard = (route: import('@playwright/test').Route, ok: object) => {
    const auth = route.request().headers()['authorization'];
    return auth === 'Bearer fresh-access'
      ? route.fulfill({ json: ok })
      : route.fulfill({ status: 401, body: '' });
  };
  await page.route('**/api/v1/me', (route) => guard(route, profile));
  await page.route('**/api/v1/businesses**', (route) => guard(route, { items: [master], next_cursor: null }));

  await page.goto('/dashboard');

  await expect(page.getByRole('heading', { name: 'Your businesses' })).toBeVisible();
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Acme' })).toBeVisible();
  await expect(page).toHaveURL(/\/dashboard$/); // not bounced to login
  expect(refreshCount).toBeGreaterThanOrEqual(1);
  expect(await page.evaluate(() => localStorage.getItem('mf_access'))).toBe('fresh-access');
});

test('a failed business load shows a retry that recovers', async ({ page }) => {
  await seedAuth(page, 'fresh-access');
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: profile }));

  let attempts = 0;
  await page.route('**/api/v1/businesses**', (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    attempts++;
    return attempts === 1
      ? route.fulfill({ status: 500, body: '' })
      : route.fulfill({ json: { items: [master], next_cursor: null } });
  });

  await page.goto('/dashboard');
  await expect(page.getByText(/couldn.t load your businesses/i)).toBeVisible();
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Acme' })).toBeVisible();
});
