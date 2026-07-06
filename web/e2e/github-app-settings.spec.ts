import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const agent = { id: 'ag1', business_id: 'b1', name: 'Reviewer', provider: 'anthropic', model: 'claude', enabled: true };

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

// manyforge-11e: the settings page was only reachable by typing the URL. It must
// be reachable by CLICKING a nav entry — so start elsewhere (/dashboard) and
// navigate via the sidebar link, landing on the rendered settings page.
test('github app settings: reachable by clicking the GitHub nav entry', async ({ page }) => {
  // Specific, safe-shaped mocks (mirrors shell.spec) instead of a blanket {} stub
  // (PR #22 review): the approvals/connectors nav badges only fetch when a current
  // business is set (app.ts hasBiz), which it isn't here, so those calls never fire.
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  await page.route('**/api/v1/businesses/*/tickets**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));

  await page.goto('/dashboard');
  const navLink = page.getByTestId('nav-github');
  await expect(navLink).toBeVisible();
  await expect(navLink).toHaveAttribute('href', '/settings/github');
  await navLink.click();
  await expect(page).toHaveURL(/\/settings\/github$/);
  await expect(page.getByTestId('create-app-button')).toBeVisible();
});

test('github app settings: create-app button submits manifest form to GitHub', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.route('**/api/v1/github/app/manifest', (r) =>
    r.fulfill({ json: { action_url: 'https://github.com/settings/apps/new', manifest: '{"name":"manyforge-bot"}', state: 'sig.tok' } }),
  );

  let capturedUrl = '';
  let capturedBody = '';
  await page.route('https://github.com/settings/apps/new**', async (r) => {
    capturedUrl = r.request().url();
    capturedBody = r.request().postData() ?? '';
    await r.fulfill({ contentType: 'text/html', body: '<html><body>github stub</body></html>' });
  });

  await page.goto('/settings/github');
  await page.getByTestId('create-app-button').click();
  await expect(page).toHaveURL(/settings\/apps\/new/);

  expect(capturedUrl).toBe('https://github.com/settings/apps/new?state=sig.tok');
  expect(capturedBody).toContain('manifest=');
  expect(decodeURIComponent(capturedBody.replace('manifest=', ''))).toBe('{"name":"manyforge-bot"}');
});

test('github app settings: create-app shows operator-only message on 404', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [] } }));
  await page.route('**/api/v1/github/app/manifest', (r) => r.fulfill({ status: 404, json: { code: 'NOT_FOUND' } }));

  await page.goto('/settings/github');
  await page.getByTestId('create-app-button').click();
  await expect(page.getByTestId('create-app-error')).toContainText('Only the instance operator');
});

test('github app settings: connect section lists agents and mints an install URL', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));

  let requestedUrl = '';
  await page.route('**/api/v1/businesses/b1/github/app/install-url**', (r) => {
    requestedUrl = r.request().url();
    return r.fulfill({ json: { install_url: 'https://github.com/apps/manyforge-bot/installations/new?state=sig2' } });
  });
  await page.route('https://github.com/apps/manyforge-bot/installations/new**', (r) =>
    r.fulfill({ contentType: 'text/html', body: '<html><body>install stub</body></html>' }),
  );

  await page.goto('/settings/github');
  await expect(page.getByTestId('gh-agent-select')).toBeVisible();

  // Connect button stays disabled until an agent is chosen.
  await expect(page.getByTestId('connect-github-button')).toBeDisabled();
  await page.getByTestId('gh-agent-select').selectOption('ag1');
  await expect(page.getByTestId('connect-github-button')).toBeEnabled();

  await page.getByTestId('connect-github-button').click();
  await expect(page).toHaveURL(/installations\/new/);
  expect(requestedUrl).toContain('agent_id=ag1');
});

test('github app settings: connect section shows a hint when the business has no agents', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [] } }));

  await page.goto('/settings/github');
  await expect(page.getByTestId('gh-no-agents-hint')).toBeVisible();
  await expect(page.getByTestId('gh-agents-link')).toHaveAttribute('href', '/agents');
  await expect(page.getByTestId('connect-github-button')).toBeDisabled();
});

test('github app settings: connect section shows guidance when the App is not created yet', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.route('**/api/v1/businesses/b1/github/app/install-url**', (r) =>
    r.fulfill({ status: 404, json: { code: 'NOT_FOUND' } }),
  );

  await page.goto('/settings/github');
  await page.getByTestId('gh-agent-select').selectOption('ag1');
  await page.getByTestId('connect-github-button').click();
  await expect(page.getByTestId('connect-error')).toContainText('Create the GitHub App first');
});
