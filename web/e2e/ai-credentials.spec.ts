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
  // Catch-all FIRST (later routes take precedence in Playwright): the app shell polls
  // /approvals and /connectors for nav badges. Left unmocked they 401 → token refresh →
  // redirect to /login mid-test, which looks like an unrelated failure.
  await page.route('**/api/**', (r) => r.fulfill({ json: {} }));
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

// A huggingface credential targets the operator's own ZeroGPU Space, so base_url is required
// and nothing is prefilled — the Space host is per-user. Submit stays disabled until it is
// supplied, rather than round-tripping a server 400. See manyforge-bhx.
test('ai-credentials: huggingface requires an explicit ZeroGPU Space base URL', async ({ page }) => {
  await auth(page);
  const hfCred = { ...cred, id: 'cred2', provider: 'huggingface', base_url: 'https://josh-reviewbot.hf.space/v1', default_model: 'Qwen/Qwen3-14B' };
  let body: Record<string, unknown> | null = null;
  let created = false;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      body = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ status: 201, json: hfCred });
    }
    return r.fulfill({ json: { items: created ? [hfCred] : [] } });
  });

  await page.goto('/credentials/ai');
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-provider').selectOption('huggingface');
  await page.getByTestId('cred-api-key').fill('hf_test');
  await page.getByTestId('cred-default-model').fill('Qwen/Qwen3-14B');

  // No base_url yet: the field is empty (no prefill) and submit is blocked.
  await expect(page.getByTestId('cred-base-url')).toHaveValue('');
  await expect(page.getByTestId('credential-form-submit')).toBeDisabled();

  await page.getByTestId('cred-base-url').fill('https://josh-reviewbot.hf.space/v1');
  await expect(page.getByTestId('credential-form-submit')).toBeEnabled();
  await page.getByTestId('credential-form-submit').click();

  await expect(page.getByTestId('credential-provider')).toContainText('huggingface');
  expect(body).not.toBeNull();
  expect(body!['provider']).toBe('huggingface');
  expect(body!['base_url']).toBe('https://josh-reviewbot.hf.space/v1');
  // The namespaced model id must survive the form untouched.
  expect(body!['default_model']).toBe('Qwen/Qwen3-14B');
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
