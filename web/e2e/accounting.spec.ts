import { expect, Page, test } from '@playwright/test';

const BIZ_ID = '11111111-1111-1111-1111-111111111111';
const AGENT_ID = '22222222-2222-2222-2222-222222222222';

const business = { id: BIZ_ID, parent_id: null, tenant_root_id: BIZ_ID, name: 'Acme', status: 'active', is_tenant_root: true };

const summary = {
  window: { from: '2026-06-01T00:00:00Z', to: '2026-06-05T14:30:00Z' },
  totals: { cost_cents: 200, tokens_in: 150, tokens_out: 260, run_count: 2 },
  agents: [
    { agent_id: AGENT_ID, name: 'Support Agent', monthly_budget_cents: 10000, run_count: 2, tokens_in: 150, tokens_out: 260, cost_cents: 200, budget_pct: 2 },
    { agent_id: '33333333-3333-3333-3333-333333333333', name: 'Idle Agent', monthly_budget_cents: 0, run_count: 0, tokens_in: 0, tokens_out: 0, cost_cents: 0 },
  ],
};

const runsPage = {
  items: [
    { id: 'aaaa1111-0000-0000-0000-000000000001', agent_id: AGENT_ID, trigger: 'manual', status: 'succeeded', tokens_in: 100, tokens_out: 200, cost_cents: 120, correlation_id: 'c1', created_at: '2026-06-05T13:30:00Z' },
    { id: 'aaaa1111-0000-0000-0000-000000000002', agent_id: AGENT_ID, trigger: 'event', status: 'failed', tokens_in: 50, tokens_out: 60, cost_cents: 80, correlation_id: 'c2', created_at: '2026-06-05T12:30:00Z' },
  ],
  next_cursor: null,
};

async function installStack(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({ json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' } }),
  );
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: { items: [business] } }));
  // Specific routes registered AFTER the broad ones win (Playwright: last-registered-first).
  await page.route(`**/api/v1/businesses/${BIZ_ID}/accounting**`, (route) => route.fulfill({ json: summary }));
  await page.route(`**/api/v1/businesses/${BIZ_ID}/agents/${AGENT_ID}/runs**`, (route) => route.fulfill({ json: runsPage }));
}

test('accounting summary renders totals + per-agent rows (incl. zero-run agent)', async ({ page }) => {
  await installStack(page);
  await page.goto('/accounting');

  await expect(page.getByRole('heading', { name: 'Accounting' })).toBeVisible();
  await expect(page.getByTestId('business-select')).toHaveValue(BIZ_ID);
  await expect(page.getByTestId('total-cost')).toContainText('2.00');
  await expect(page.getByTestId('total-runs')).toHaveText('2');

  const rows = page.getByTestId('agent-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.first().getByTestId('agent-name')).toHaveText('Support Agent');
  await expect(rows.first().getByTestId('agent-budget-pct')).toContainText('2%');
});

test('budget-% hint appears only for non-current-month windows (deo.7)', async ({ page }) => {
  await installStack(page);
  await page.goto('/accounting');

  // this_month: budget % pills are populated, so no hint.
  await expect(page.getByTestId('agent-row').first().getByTestId('agent-budget-pct')).toContainText('2%');
  await expect(page.getByTestId('budget-hint')).toHaveCount(0);

  // Switching to last_month: budget % is unavailable, so a hint explains it.
  await page.getByTestId('window-select').selectOption('last_month');
  await expect(page.getByTestId('budget-hint')).toContainText('current month');
});

test('clicking an agent drills into its run list', async ({ page }) => {
  await installStack(page);
  await page.goto('/accounting');
  await page.getByTestId('agent-row').first().click();

  await expect(page).toHaveURL(new RegExp(`/accounting/${BIZ_ID}/${AGENT_ID}`));
  const runRows = page.getByTestId('run-row');
  await expect(runRows).toHaveCount(2);
  await expect(runRows.first().getByTestId('run-status')).toHaveText('succeeded');
  await expect(runRows.first().getByTestId('run-cost')).toContainText('1.20');
});
