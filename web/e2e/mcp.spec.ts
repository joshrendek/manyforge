import { expect, test, type Page } from '@playwright/test';

const profile = { id: 'u1', email: 'admin@x.test', display_name: 'Admin', email_verified: true, status: 'active' };
const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};

async function auth(page: Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('MCP server list renders + create', async ({ page }) => {
  await auth(page);
  let created = false;
  await page.route('**/api/v1/businesses/b1/mcp_servers', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      return r.fulfill({
        json: { id: 's1', business_id: 'b1', name: 'acme', url: 'https://m', enabled: true, created_at: '', updated_at: '' },
      });
    }
    return r.fulfill({
      json: {
        items: created
          ? [{ id: 's1', business_id: 'b1', name: 'acme', url: 'https://m', enabled: true, created_at: '', updated_at: '' }]
          : [],
      },
    });
  });
  await page.goto('/mcp');
  await expect(page.getByTestId('mcp-page')).toBeVisible();
  await page.getByTestId('mcp-add-toggle').click();
  await page.getByTestId('mcp-name').fill('acme');
  await page.getByTestId('mcp-url').fill('https://m');
  await page.getByTestId('mcp-form-submit').click();
  await expect(page.getByTestId('mcp-row')).toHaveCount(1);
});

test('tool reclassification round-trip', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tools', (r) =>
    r.fulfill({ json: { reachable: true, tools: [{ name: 'get_thing', description: 'reads', effect: 'external' }] } }),
  );
  let putBody: unknown = null;
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tool_policies/get_thing', (r) => {
    putBody = r.request().postDataJSON();
    return r.fulfill({ json: { tool_name: 'get_thing', effect: 'reversible' } });
  });
  await page.goto('/mcp/b1/s1');
  await expect(page.getByTestId('mcp-tool-row')).toHaveCount(1);
  await page.getByTestId('mcp-tool-effect').selectOption('reversible');
  await expect(page.getByTestId('toast')).toContainText('Policy saved');
  expect(putBody).toEqual({ effect: 'reversible' });
});

test('unreachable server shows banner', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tools', (r) =>
    r.fulfill({ json: { reachable: false, tools: [] } }),
  );
  await page.goto('/mcp/b1/s1');
  await expect(page.getByTestId('mcp-unreachable')).toBeVisible();
});
