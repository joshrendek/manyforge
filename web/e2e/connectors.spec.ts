import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const connector = {
  id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme Jira', base_url: 'https://acme.atlassian.net',
  allow_private_base_url: false, config: {}, status: 'enabled', last_reconciled_at: null,
  created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
  health: { state: 'healthy', linked_ticket_count: 2, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null },
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('connectors: renders list with health pill', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.goto('/connectors');
  await expect(page.getByTestId('connector-name')).toContainText('Acme Jira');
  await expect(page.getByTestId('connector-health')).toContainText('Healthy');
});

test('connectors: create a connector', async ({ page }) => {
  await auth(page);
  let created = false;
  await page.route('**/api/v1/businesses/b1/connectors', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      return r.fulfill({ status: 201, json: connector });
    }
    return r.fulfill({ json: { items: created ? [connector] : [] } });
  });
  await page.goto('/connectors');
  await page.getByTestId('connector-add-toggle').click();
  await page.getByTestId('conn-display-name').fill('Acme Jira');
  await page.getByTestId('conn-base-url').fill('https://acme.atlassian.net');
  await page.getByTestId('conn-email').fill('a@b.c');
  await page.getByTestId('conn-api-token').fill('tok');
  await page.getByTestId('connector-form-submit').click();
  await expect(page.getByTestId('connector-name')).toContainText('Acme Jira');
});

test('connectors: test action shows a toast', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.route('**/api/v1/businesses/b1/connectors/c1/test', (r) => r.fulfill({ json: { ok: true, detail: 'ok' } }));
  await page.goto('/connectors');
  await page.getByTestId('connector-test').click();
  await expect(page.getByTestId('toast')).toContainText(/OK/i);
});

test('connectors: delete asks to confirm then removes the row', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/connectors', (r) => r.fulfill({ json: { items: [connector] } }));
  await page.route('**/api/v1/businesses/b1/connectors/c1', (r) => {
    if (r.request().method() === 'DELETE') return r.fulfill({ status: 204, body: '' });
    return r.fulfill({ json: connector });
  });
  await page.goto('/connectors');
  await page.getByTestId('connector-delete').click();
  await expect(page.getByTestId('connector-delete-confirm')).toContainText('Detaches 2');
  await page.getByTestId('connector-delete-yes').click();
  await expect(page.getByTestId('connector-row')).toHaveCount(0);
});
