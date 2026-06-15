import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const agent = {
  id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage', provider: 'anthropic', model: 'claude-opus-4-8',
  system_prompt: '', allowed_tools: ['read_ticket'], autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500,
  allowed_mcp_servers: [], retriage_on_reply: false, created_at: '2026-06-15T00:00:00Z', updated_at: '2026-06-15T00:00:00Z',
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

async function metadata(page: import('@playwright/test').Page) {
  await page.route('**/api/v1/businesses/b1/agents/tools', (r) =>
    r.fulfill({ json: { items: [{ name: 'read_ticket', description: 'read', effect: 'read', required_perm: 'tickets.read' }] } }));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }));
  await page.route('**/api/v1/businesses/b1/mcp_servers', (r) => r.fulfill({ json: { items: [] } }));
}

test('agents: lists agents', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.goto('/agents');
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage');
});

test('agents: create an agent (tools + budget→cents)', async ({ page }) => {
  await auth(page);
  await metadata(page);
  let created = false;
  let body: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/agents', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      body = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ status: 201, json: agent });
    }
    return r.fulfill({ json: { items: created ? [agent] : [] } });
  });
  await page.goto('/agents');
  await page.getByTestId('agent-add-toggle').click();
  await page.getByTestId('agent-name').fill('Triage');
  await page.getByTestId('agent-model-select').selectOption('claude-opus-4-8');
  await page.getByTestId('agent-tool-read_ticket').check();
  await page.getByTestId('agent-budget').fill('25');
  await page.getByTestId('agent-form-submit').click();
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage');
  expect(body).not.toBeNull();
  expect(body!['allowed_tools']).toEqual(['read_ticket']);
  expect(body!['monthly_budget_cents']).toBe(2500);
});

test('agents: edit an agent', async ({ page }) => {
  await auth(page);
  await metadata(page);
  let patched = false;
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ json: { items: [patched ? { ...agent, name: 'Triage 2' } : agent] } }));
  await page.route('**/api/v1/businesses/b1/agents/a1', (r) => {
    if (r.request().method() === 'PATCH') {
      patched = true;
      return r.fulfill({ json: { ...agent, name: 'Triage 2' } });
    }
    return r.fulfill({ json: agent });
  });
  await page.goto('/agents');
  await page.getByTestId('agent-edit').click();
  await page.getByTestId('agent-name').fill('Triage 2');
  await page.getByTestId('agent-form-submit').click();
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage 2');
});

test('agents: delete with confirm', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.route('**/api/v1/businesses/b1/agents/a1', (r) =>
    r.request().method() === 'DELETE' ? r.fulfill({ status: 204, body: '' }) : r.fulfill({ json: agent }));
  await page.goto('/agents');
  await page.getByTestId('agent-delete').click();
  await expect(page.getByTestId('agent-delete-confirm')).toContainText('Delete Triage');
  await page.getByTestId('agent-delete-yes').click();
  await expect(page.getByTestId('agent-row')).toHaveCount(0);
});

test('agents: no access shows an error', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: 'not found' } }));
  await page.goto('/agents');
  await expect(page.getByTestId('agents-error')).toContainText('Could not load agents');
});
