import { expect, test } from '@playwright/test';

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: { id: 'p1', email: 'op@example.com' } }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: { items: [] } }));
}

test('github installed: links successfully', async ({ page }) => {
  await auth(page);
  let posted: unknown = null;
  await page.route('**/api/v1/github/app/installations/link', async (r) => {
    posted = r.request().postDataJSON();
    await r.fulfill({ json: { linked: true } });
  });
  await page.goto('/settings/github/installed?code=oc&installation_id=555&state=sig.tok');
  await expect(page.getByTestId('gh-linked')).toBeVisible();
  expect(posted).toEqual({ code: 'oc', installation_id: '555', state: 'sig.tok' });
});

test('github installed: shows error on rejected proof', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/github/app/installations/link', (r) =>
    r.fulfill({ status: 404, json: { code: 'NOT_FOUND' } }),
  );
  await page.goto('/settings/github/installed?code=oc&installation_id=555&state=sig.tok');
  await expect(page.getByTestId('gh-error')).toBeVisible();
});

test('github installed: pending when awaiting admin approval', async ({ page }) => {
  await auth(page);
  await page.goto('/settings/github/installed?setup_action=request&state=sig.tok');
  await expect(page.getByTestId('gh-pending')).toBeVisible();
});

test('github app created: converts manifest successfully', async ({ page }) => {
  await auth(page);
  let posted: unknown = null;
  await page.route('**/api/v1/github/app/manifest/convert', async (r) => {
    posted = r.request().postDataJSON();
    await r.fulfill({ json: { slug: 'manyforge-bot' } });
  });
  await page.goto('/settings/github/app-created?code=oc&state=sig.tok');
  await expect(page.getByTestId('gh-success')).toBeVisible();
  expect(posted).toEqual({ code: 'oc', state: 'sig.tok' });
});

test('github app created: shows error when a GitHub App already exists', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/github/app/manifest/convert', (r) =>
    r.fulfill({ status: 409, json: { code: 'CONFLICT' } }),
  );
  await page.goto('/settings/github/app-created?code=oc&state=sig.tok');
  await expect(page.getByTestId('gh-error')).toBeVisible();
});

test('github app created: shows error when setup params are missing', async ({ page }) => {
  await auth(page);
  await page.goto('/settings/github/app-created');
  await expect(page.getByTestId('gh-error')).toBeVisible();
});

// manyforge-11e: the success screen previously dead-ended (no way back). A
// "Back to GitHub settings" link must return to /settings/github.
test('github app created: back link returns to GitHub settings', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/*/agents', (r) => r.fulfill({ json: { items: [] } }));
  await page.goto('/settings/github/app-created');
  await page.getByTestId('back-to-github-settings').click();
  await expect(page).toHaveURL(/\/settings\/github$/);
});

// manyforge-11e: the org-linkage landing screen needs the same back link.
test('github installed: back link returns to GitHub settings', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/*/agents', (r) => r.fulfill({ json: { items: [] } }));
  await page.goto('/settings/github/installed?setup_action=request&state=sig.tok');
  await expect(page.getByTestId('gh-pending')).toBeVisible();
  await page.getByTestId('back-to-github-settings').click();
  await expect(page).toHaveURL(/\/settings\/github$/);
});
