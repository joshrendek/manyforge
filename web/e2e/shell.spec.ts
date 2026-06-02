import { expect, Page, test } from '@playwright/test';

const BIZ_ID = 'biz-1';

const profile = {
  id: 'u1',
  email: 'owner@manyforge.test',
  display_name: 'Owner',
  email_verified: true,
  status: 'active',
};

async function seedToken(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
}

async function installAuth(page: Page) {
  await seedToken(page);
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses**', (route) =>
    route.fulfill({
      json: {
        items: [{ id: BIZ_ID, parent_id: null, tenant_root_id: BIZ_ID, name: 'Acme', status: 'active' }],
        next_cursor: null,
      },
    }),
  );
  await page.route('**/api/v1/businesses/*/tickets**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
}

test('the sidebar persists across dashboard and support with correct active state', async ({ page }) => {
  await installAuth(page);

  await page.goto('/dashboard');
  await expect(page.getByTestId('app-sidebar')).toBeVisible();
  await expect(page.getByTestId('nav-dashboard')).toHaveClass(/active/);
  await expect(page.getByTestId('sidebar-identity')).toContainText('Owner');

  await page.getByTestId('nav-support').click();
  await expect(page).toHaveURL(/\/support$/);
  await expect(page.getByTestId('app-sidebar')).toBeVisible(); // still there — not an island
  await expect(page.getByTestId('nav-support')).toHaveClass(/active/);
});

test('the sidebar is absent on the login screen when unauthenticated', async ({ page }) => {
  await page.goto('/login');
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Welcome back' })).toBeVisible();
});

// Regression for the bug caught while driving the live app: an authenticated user
// (token present) who navigates to /login must NOT get the app shell wrapped around
// the login form. The shell is gated on route, not just token presence.
test('the sidebar is absent on /login even when a token is present', async ({ page }) => {
  await seedToken(page);
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: profile }));

  await page.goto('/login');
  await expect(page.getByRole('heading', { name: 'Welcome back' })).toBeVisible();
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0);
});
