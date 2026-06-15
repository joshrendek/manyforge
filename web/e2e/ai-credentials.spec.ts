import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const cred = {
  id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
  allow_private_base_url: false, created_at: '2026-06-15T00:00:00Z', updated_at: '2026-06-15T00:00:00Z',
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('ai-credentials: lists configured providers', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => r.fulfill({ json: { items: [cred] } }));
  await page.goto('/credentials/ai');
  await expect(page.getByTestId('credential-provider')).toContainText('anthropic');
});

test('ai-credentials: create a credential', async ({ page }) => {
  await auth(page);
  let created = false;
  let body: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      body = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ status: 201, json: cred });
    }
    return r.fulfill({ json: { items: created ? [cred] : [] } });
  });
  await page.goto('/credentials/ai');
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-api-key').fill('sk-ant-secret');
  await page.getByTestId('cred-default-model').fill('claude-opus-4-8');
  await page.getByTestId('credential-form-submit').click();
  await expect(page.getByTestId('credential-provider')).toContainText('anthropic');
  expect(body).not.toBeNull();
  expect(body!['api_key']).toBe('sk-ant-secret');
});

test('ai-credentials: delete asks to confirm then removes the row', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => r.fulfill({ json: { items: [cred] } }));
  await page.route('**/api/v1/businesses/b1/ai_credentials/cred1', (r) =>
    r.request().method() === 'DELETE' ? r.fulfill({ status: 204, body: '' }) : r.fulfill({ json: cred }),
  );
  await page.goto('/credentials/ai');
  await page.getByTestId('credential-delete').click();
  await expect(page.getByTestId('credential-delete-confirm')).toContainText('Delete anthropic');
  await page.getByTestId('credential-delete-yes').click();
  await expect(page.getByTestId('credential-row')).toHaveCount(0);
});
