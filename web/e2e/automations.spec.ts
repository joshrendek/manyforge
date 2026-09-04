import { expect, test } from '@playwright/test';

const account = { id: 'u1', email: 'operator@acme.test', display_name: 'Operator', email_verified: true, status: 'active' };
const businesses = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const list = { id: '11111111-1111-4111-8111-111111111111', business_id: 'b1', tenant_root_id: 'b1', slug: 'news', name: 'Product news', description: null, double_opt_in: true, status: 'active', created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const template = { id: '22222222-2222-4222-8222-222222222222', business_id: 'b1', tenant_root_id: 'b1', name: 'Welcome email', subject: 'Welcome', preheader: null, body_markdown: 'Hello', track_opens: true, track_clicks: true, created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const automation = { id: 'a1', business_id: 'b1', tenant_root_id: 'b1', name: 'Welcome journey', description: null, status: 'draft', allow_reenroll: false, active_version_id: null, draft_version_id: 'v1', created_by_principal_id: 'u1', created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const version = { id: 'v1', business_id: 'b1', tenant_root_id: 'b1', automation_id: 'a1', number: 1, status: 'draft', graph: { nodes: [], edges: [] }, trigger_kind: null, trigger_ref: null, activated_at: null, created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };

test('automations: insert, edit, and save a graph', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  // The most recently registered matching route wins, so keep the broad fallback first.
  await page.route('**/api/**', (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: account }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
  await page.route('**/api/v1/businesses/b1/mailing/lists', (route) => route.fulfill({ json: { items: [list], next_cursor: null } }));
  await page.route('**/api/v1/businesses/b1/mailing/templates', (route) => route.fulfill({ json: { items: [template], next_cursor: null } }));
  await page.route('**/api/v1/businesses/b1/mailing/automations', (route) => {
    if (route.request().method() === 'POST') return route.fulfill({ status: 201, json: automation });
    return route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1', (route) => route.fulfill({ json: automation }));
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1/versions/v1', (route) => route.fulfill({ json: version }));

  let savedGraph: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1/versions/v1/graph', (route) => {
    savedGraph = route.request().postDataJSON() as Record<string, unknown>;
    return route.fulfill({ json: { ...version, graph: savedGraph } });
  });

  await page.goto('/mailing/automations');
  await page.getByTestId('automation-name').fill('Welcome journey');
  await page.getByTestId('automation-create').click();
  await expect(page).toHaveURL(/\/mailing\/b1\/automations\/a1$/);
  await expect(page.getByTestId('canvas-node')).toHaveCount(2);

  await page.getByTestId('edge-plus').click();
  await page.getByTestId('insert-send_email').click();
  await expect(page.getByTestId('automation-node-panel')).toBeVisible();
  await page.getByTestId('automation-node-name').fill('First welcome email');
  await page.getByTestId('send-template').selectOption(template.id);
  await page.getByTestId('automation-save').click();

  await expect.poll(() => savedGraph).not.toBeNull();
  const graph = savedGraph as { nodes: Array<Record<string, unknown>>; edges: Array<Record<string, unknown>> };
  expect(graph.nodes).toHaveLength(3);
  expect(graph.edges).toHaveLength(2);
  expect(graph.nodes.find((node) => node['kind'] === 'send_email')).toMatchObject({
    name: 'First welcome email',
    config: { template_id: template.id, track_opens: true, track_clicks: true },
  });
  expect(graph.edges.every((edge) => Object.hasOwn(edge, 'branch'))).toBe(true);
});

test('automations: activation surfaces server validation issues, then activates after the fix', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  await page.route('**/api/**', (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: account }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
  await page.route('**/api/v1/businesses/b1/mailing/lists', (route) => route.fulfill({ json: { items: [list], next_cursor: null } }));
  await page.route('**/api/v1/businesses/b1/mailing/templates', (route) => route.fulfill({ json: { items: [template], next_cursor: null } }));
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1', (route) => route.fulfill({ json: automation }));
  const missingTemplate = '99999999-9999-4999-8999-999999999999';
  const brokenVersion = {
    ...version,
    graph: {
      nodes: [
        { id: 'trigger', kind: 'trigger', name: 'Trigger', config: { type: 'list_joined', list_id: list.id } },
        { id: 'n_welcome', kind: 'send_email', name: 'Welcome', config: { template_id: missingTemplate, track_opens: true, track_clicks: true } },
        { id: 'exit', kind: 'exit', config: {} },
      ],
      edges: [
        { id: 'e1', from: 'trigger', to: 'n_welcome', branch: null },
        { id: 'e2', from: 'n_welcome', to: 'exit', branch: null },
      ],
    },
  };
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1/versions/v1', (route) => route.fulfill({ json: brokenVersion }));
  let savedGraph: { nodes: Array<Record<string, unknown>>; edges: Array<Record<string, unknown>> } | null = null;
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1/versions/v1/graph', (route) => {
    savedGraph = route.request().postDataJSON();
    return route.fulfill({ json: { ...brokenVersion, graph: savedGraph } });
  });
  let activateCalls = 0;
  await page.route('**/api/v1/businesses/b1/mailing/automations/a1/versions/v1/activate', (route) => {
    activateCalls += 1;
    if (activateCalls === 1) {
      return route.fulfill({
        status: 422,
        json: { code: 'AUTOMATION_INVALID', message: 'automation graph is invalid', issues: [{ code: 'template_not_found', node_id: 'n_welcome', message: `Template ${missingTemplate} was not found` }] },
      });
    }
    return route.fulfill({ json: { ...automation, status: 'active', active_version_id: 'v1', draft_version_id: null } });
  });

  await page.goto('/mailing/b1/automations/a1');
  await expect(page.getByTestId('canvas-node')).toHaveCount(3);
  await expect(page.getByTestId('automation-validation-count')).toHaveText('Graph valid');

  await page.getByTestId('automation-activate').click();
  await expect(page.locator('[data-node-id="n_welcome"]')).toHaveAttribute('data-invalid', 'true');
  await expect(page.getByTestId('automation-validation-count')).toHaveText('1 issue(s)');

  await page.locator('[data-node-id="n_welcome"]').dblclick();
  await expect(page.getByTestId('automation-node-panel')).toBeVisible();
  await page.getByTestId('send-template').selectOption(template.id);
  await page.getByTestId('automation-save').click();
  await expect.poll(() => savedGraph).not.toBeNull();
  expect(savedGraph).not.toBeNull();
  const fixedNode = savedGraph.nodes.find((node) => node['id'] === 'n_welcome');
  expect(fixedNode).toBeDefined();
  expect(fixedNode['config']).toMatchObject({ template_id: template.id });
  await page.getByTestId('automation-activate').click();
  await expect(activateCalls).toBe(2);
  await expect(page.getByTestId('automation-pause')).toBeVisible();
  await expect(page.locator('[data-node-id="n_welcome"]')).toHaveAttribute('data-invalid', 'false');
  await expect(page.getByTestId('automation-validation-count')).toHaveText('Graph valid');
  await expect(page.getByText('Version 1 · active')).toBeVisible();
});
