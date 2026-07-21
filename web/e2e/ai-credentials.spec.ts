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

// huggingface targets the HF Inference Providers router, which has one canonical endpoint, so
// selecting it prefills base_url the way openrouter does. The model id pins the routed partner
// with a ":" suffix and must survive the form untouched. See manyforge-bhx.
test('ai-credentials: huggingface prefills the router base URL', async ({ page }) => {
  await auth(page);
  const hfBase = 'https://router.huggingface.co/v1';
  const hfModel = 'zai-org/GLM-5.2:fireworks-ai';
  const hfCred = { ...cred, id: 'cred2', provider: 'huggingface', base_url: hfBase, default_model: hfModel };
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
  await expect(page.getByTestId('cred-base-url')).toHaveValue(hfBase);

  await page.getByTestId('cred-api-key').fill('hf_test');
  await page.getByTestId('cred-default-model').fill(hfModel);
  await expect(page.getByTestId('credential-form-submit')).toBeEnabled();
  await page.getByTestId('credential-form-submit').click();

  await expect(page.getByTestId('credential-provider')).toContainText('huggingface');
  expect(body).not.toBeNull();
  expect(body!['provider']).toBe('huggingface');
  expect(body!['base_url']).toBe(hfBase);
  expect(body!['default_model']).toBe(hfModel);
});

// Providers with no server-side default must not be submittable without a base_url — the form
// blocks it rather than round-tripping a 400.
test('ai-credentials: vllm blocks submit until a base URL is supplied', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => r.fulfill({ json: { items: [] } }));
  await page.goto('/credentials/ai');
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-provider').selectOption('vllm');
  await page.getByTestId('cred-api-key').fill('k');
  await page.getByTestId('cred-default-model').fill('Qwen/Qwen3-14B');

  await expect(page.getByTestId('cred-base-url')).toHaveValue('');
  await expect(page.getByTestId('credential-form-submit')).toBeDisabled();

  await page.getByTestId('cred-base-url').fill('http://192.168.1.171:8000/v1');
  await expect(page.getByTestId('credential-form-submit')).toBeEnabled();
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

test('ai-credentials: connect an openai_codex credential via Sign in with ChatGPT (PKCE paste)', async ({ page }) => {
  await auth(page);
  const codexCred = {
    id: 'cx1', business_id: 'b1', provider: 'openai_codex', base_url: '', default_model: 'gpt-5.4',
    allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '2026-07-19T00:00:00Z', updated_at: '2026-07-19T00:00:00Z',
    chatgpt_plan: 'plus', connection_status: 'connected', oauth_access_expiry: '2026-08-01T00:00:00Z',
  };
  let connected = false;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) =>
    r.fulfill({ json: { items: connected ? [codexCred] : [] } }),
  );
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'openai_codex', model_id: 'gpt-5.4' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/ai_credentials/codex/pkce/start', (r) =>
    r.fulfill({ json: { pending_id: 'p2', authorize_url: 'https://auth.openai.com/oauth/authorize?x=1' } }),
  );
  await page.route('**/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange', (r) => {
    connected = true;
    return r.fulfill({ json: { status: 'approved', credential_id: 'cx1' } });
  });

  await page.goto('/credentials/ai');
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-provider').selectOption('openai_codex');
  await page.getByTestId('codex-model').selectOption('gpt-5.4');
  // Primary "Sign in with ChatGPT" starts PKCE and reveals the authorize link + paste field.
  await page.getByTestId('codex-signin').click();
  await expect(page.getByTestId('codex-open')).toHaveAttribute('href', 'https://auth.openai.com/oauth/authorize?x=1');
  // paste the redirect URL from the localhost:1455 tab and finish
  await page.getByTestId('codex-paste-url').fill('http://localhost:1455/auth/callback?code=z&state=p2');
  await page.getByTestId('codex-paste-submit').click();
  await expect(page.getByTestId('codex-health')).toContainText('connected');
});

test('ai-credentials: edit a credential concurrency limit', async ({ page }) => {
  await auth(page);
  const base = { id: 'cred1', business_id: 'b1', provider: 'openai', base_url: 'https://api.openai.com/v1', default_model: 'gpt-4o', allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '2026-07-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' };
  let edited = false;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) =>
    r.fulfill({ json: { items: [edited ? { ...base, max_concurrent_lanes: 9 } : base] } }),
  );
  await page.route('**/api/v1/businesses/b1/ai_credentials/cred1', (r) => {
    if (r.request().method() === 'PATCH') { edited = true; return r.fulfill({ json: { ...base, max_concurrent_lanes: 9 } }); }
    return r.fallback();
  });
  await page.goto('/credentials/ai');
  await expect(page.getByTestId('credential-lanes')).toHaveText('4');
  await page.getByTestId('credential-edit').click();
  await page.getByTestId('credential-edit-lanes').fill('9');
  await page.getByTestId('credential-edit-save').click();
  await expect(page.getByTestId('credential-lanes')).toHaveText('9');
});

test('ai-credentials: editing an openai_codex credential offers a model dropdown (not free text)', async ({ page }) => {
  await auth(page);
  const cx = {
    id: 'cx1', business_id: 'b1', provider: 'openai_codex', base_url: '', default_model: 'gpt-5.4',
    allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '2026-07-19T00:00:00Z', updated_at: '2026-07-19T00:00:00Z',
    chatgpt_plan: 'plus', connection_status: 'connected', oauth_access_expiry: '2026-08-01T00:00:00Z',
  };
  let edited = false;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) =>
    r.fulfill({ json: { items: [edited ? { ...cx, default_model: 'gpt-5.6-sol' } : cx] } }),
  );
  // editor prefers the live per-account catalog
  await page.route('**/api/v1/businesses/b1/ai_credentials/codex/models', (r) =>
    r.fulfill({ json: { items: [
      { provider: 'openai_codex', model_id: 'gpt-5.6-sol' },
      { provider: 'openai_codex', model_id: 'gpt-5.6-terra' },
      { provider: 'openai_codex', model_id: 'gpt-5.5' },
    ] } }),
  );
  await page.route('**/api/v1/businesses/b1/ai_credentials/cx1', (r) => {
    if (r.request().method() === 'PATCH') { edited = true; return r.fulfill({ json: { ...cx, default_model: 'gpt-5.6-sol' } }); }
    return r.fallback();
  });

  await page.goto('/credentials/ai');
  await page.getByTestId('credential-edit').click();
  const sel = page.getByTestId('credential-edit-model');
  await expect(sel).toBeVisible();
  // it is a <select> populated from the LIVE per-account codex catalog, not a text box
  await expect(sel.locator('option')).toHaveText(['gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.5']);
  await sel.selectOption('gpt-5.6-sol');
  await page.getByTestId('credential-edit-save').click();
  await expect(page.getByTestId('credential-edit-model')).toHaveCount(0);
  await expect(page.getByText('gpt-5.6-sol')).toBeVisible();
});
