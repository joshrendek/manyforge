import { expect, Page, test } from '@playwright/test';

// Spec 019 — cross-business support overview. The point of the page is that tickets from
// DIFFERENT businesses appear together, and that each row leads to the right business: an
// attention link built under the wrong business id renders fine here and only 404s after the click.

const B1 = '11111111-1111-4111-8111-111111111111';
const B2 = '22222222-2222-4222-8222-222222222222';
const T9 = '99999999-9999-4999-8999-999999999999';
const AS_OF = '2026-10-26T12:00:00Z';

function counts(over: Record<string, unknown> = {}) {
  return {
    open: 0,
    new: 0,
    pending: 0,
    awaiting_reply: 0,
    unassigned: 0,
    urgent: 0,
    high: 0,
    oldest_awaiting_at: null,
    created_7d: 0,
    resolved_7d: 0,
    median_first_response_seconds_7d: null,
    ...over,
  };
}

const overview = {
  as_of: AS_OF,
  totals: counts({
    open: 6,
    new: 2,
    pending: 1,
    awaiting_reply: 3,
    unassigned: 2,
    urgent: 1,
    high: 1,
    oldest_awaiting_at: '2026-10-24T08:00:00Z',
    created_7d: 8,
    resolved_7d: 5,
    median_first_response_seconds_7d: 5400,
  }),
  businesses: [
    { business_id: B2, business_name: 'garden.gg', ...counts({ open: 4, awaiting_reply: 2, urgent: 1, oldest_awaiting_at: '2026-10-24T08:00:00Z' }) },
    { business_id: B1, business_name: 'Bluescripts', ...counts({ open: 2, awaiting_reply: 1 }) },
  ],
  attention: [
    {
      business_id: B2,
      business_name: 'garden.gg',
      ticket_id: T9,
      subject: 'Seeds never arrived',
      status: 'open',
      priority: 'urgent',
      assigned: false,
      waiting_since: '2026-10-24T08:00:00Z',
    },
  ],
};

async function installStack(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  // Broad fallback FIRST (Playwright runs the last-registered match first): unmocked shell calls
  // otherwise 401 and bounce the test to /login.
  await page.route('**/api/v1/**', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' },
    }),
  );
  const biz = (id: string, name: string) => ({ id, name, parent_id: null, tenant_root_id: id, status: 'active' });
  await page.route('**/api/v1/businesses**', (route) =>
    route.fulfill({ json: { items: [biz(B1, 'Bluescripts'), biz(B2, 'garden.gg')], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/*/tickets**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/support/overview', (route) => route.fulfill({ json: overview }));
}

test('shows pooled KPIs and every business together', async ({ page }) => {
  await installStack(page);
  await page.goto('/support/overview');

  await expect(page.getByRole('heading', { name: 'Support overview' })).toBeVisible();
  await expect(page.getByTestId('nav-support-overview')).toHaveClass(/active/);
  await expect(page.getByTestId('kpi-awaiting')).toHaveText('3');
  await expect(page.getByTestId('kpi-awaiting-tile')).toContainText('oldest 2d 4h');
  await expect(page.getByTestId('kpi-frt')).toHaveText('1h 30m');

  const rows = page.getByTestId('support-overview-business-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toHaveAttribute('data-business-id', B2);
  await expect(rows.nth(0).getByTestId('biz-awaiting')).toHaveText('2');
  await expect(rows.nth(1)).toHaveAttribute('data-business-id', B1);
});

test('an attention ticket opens its thread under its OWN business', async ({ page }) => {
  await installStack(page);
  await page.goto('/support/overview');

  const row = page.getByTestId('support-overview-attention-row');
  await expect(row).toHaveCount(1);
  await expect(row).toContainText('Seeds never arrived');
  await expect(row).toContainText('Unassigned');
  await expect(row).toHaveAttribute('href', `/support/${B2}/${T9}`);
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/support/${B2}/${T9}$`));
});

test('a business row switches the support inbox to that business', async ({ page }) => {
  await installStack(page);
  await page.goto('/support/overview');

  const listed = page.waitForRequest((r) => r.url().includes(`/api/v1/businesses/${B1}/tickets`));
  await page.locator(`[data-testid="support-overview-business-row"][data-business-id="${B1}"]`).click();
  await expect(page).toHaveURL(/\/support$/);
  await listed;
});

test('the per-business inbox links back to the overview', async ({ page }) => {
  await installStack(page);
  await page.goto('/support');
  await page.getByTestId('support-overview-link').click();
  await expect(page).toHaveURL(/\/support\/overview$/);
  await expect(page.getByTestId('kpi-awaiting')).toHaveText('3');
});
