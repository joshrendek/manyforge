import { expect, test } from '@playwright/test';

const ACCOUNT_ID = '11111111-1111-4111-8111-111111111111';
const BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const LIST_ID = '33333333-3333-4333-8333-333333333333';
const TEMPLATE_ID = '44444444-4444-4444-8444-444444444444';
const AUTOMATION_ID = '55555555-5555-4555-8555-555555555555';
const VERSION_ID = '66666666-6666-4666-8666-666666666666';
const SUBSCRIBER_ID = '77777777-7777-4777-8777-777777777777';
const ENROLLMENT_ID = '88888888-8888-4888-8888-888888888888';
const CREATED_ENROLLMENT_ID = '99999999-9999-4999-8999-999999999999';

const account = { id: ACCOUNT_ID, email: 'operator@acme.test', display_name: 'Operator', email_verified: true, status: 'active' };
const businesses = { items: [{ id: BUSINESS_ID, parent_id: null, tenant_root_id: BUSINESS_ID, name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const list = { id: LIST_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, slug: 'news', name: 'Product news', description: null, double_opt_in: true, status: 'active', created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const template = { id: TEMPLATE_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, name: 'Welcome email', subject: 'Welcome', preheader: null, body_markdown: 'Hello', track_opens: true, track_clicks: true, created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const automation = { id: AUTOMATION_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, name: 'Welcome journey', description: null, status: 'draft', allow_reenroll: false, active_version_id: null, draft_version_id: VERSION_ID, created_by_principal_id: ACCOUNT_ID, created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };
const version = { id: VERSION_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, automation_id: AUTOMATION_ID, number: 1, status: 'draft', graph: { nodes: [], edges: [] }, trigger_kind: null, trigger_ref: null, activated_at: null, created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-03T12:00:00Z' };

test('automations: insert, edit, and save a graph', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  // The most recently registered matching route wins, so keep the broad fallback first.
  await page.route('**/api/**', (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: account }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) => route.fulfill({ json: { items: [list], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/templates`, (route) => route.fulfill({ json: { items: [template], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations`, (route) => {
    if (route.request().method() === 'POST') return route.fulfill({ status: 201, json: automation });
    return route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}`, (route) => route.fulfill({ json: automation }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}`, (route) => route.fulfill({ json: version }));

  let savedGraph: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}/graph`, (route) => {
    savedGraph = route.request().postDataJSON() as Record<string, unknown>;
    return route.fulfill({ json: { ...version, graph: savedGraph } });
  });

  await page.goto('/mailing/automations');
  await page.getByTestId('automation-name').fill('Welcome journey');
  await page.getByTestId('automation-create').click();
  await expect(page).toHaveURL(new RegExp(`/mailing/${BUSINESS_ID}/automations/${AUTOMATION_ID}$`));
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
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) => route.fulfill({ json: { items: [list], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/templates`, (route) => route.fulfill({ json: { items: [template], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}`, (route) => route.fulfill({ json: automation }));
  const missingTemplate = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
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
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}`, (route) => route.fulfill({ json: brokenVersion }));
  let savedGraph: { nodes: Array<Record<string, unknown>>; edges: Array<Record<string, unknown>> } | null = null;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}/graph`, (route) => {
    savedGraph = route.request().postDataJSON();
    return route.fulfill({ json: { ...brokenVersion, graph: savedGraph } });
  });
  let activateCalls = 0;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}/activate`, (route) => {
    activateCalls += 1;
    if (activateCalls === 1) {
      return route.fulfill({
        status: 422,
        json: { code: 'AUTOMATION_INVALID', message: 'automation graph is invalid', issues: [{ code: 'template_not_found', node_id: 'n_welcome', message: `Template ${missingTemplate} was not found` }] },
      });
    }
    return route.fulfill({ json: { ...automation, status: 'active', active_version_id: VERSION_ID, draft_version_id: null } });
  });

  await page.goto(`/mailing/${BUSINESS_ID}/automations/${AUTOMATION_ID}`);
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

test('automations: stats overlay and enrollments tab on an active automation', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  await page.route('**/api/**', (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: account }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) => route.fulfill({ json: { items: [list], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/templates`, (route) => route.fulfill({ json: { items: [template], next_cursor: null } }));
  const activeAutomation = { ...automation, status: 'active', active_version_id: VERSION_ID, draft_version_id: null };
  const activeVersion = { ...version, status: 'active', graph: {
    nodes: [
      { id: 'trigger', kind: 'trigger', name: 'Trigger', config: { type: 'list_joined', list_id: list.id } },
      { id: 'n_welcome', kind: 'send_email', name: 'Welcome', config: { template_id: template.id, track_opens: true, track_clicks: true } },
      { id: 'exit', kind: 'exit', config: {} },
    ],
    edges: [
      { id: 'e1', from: 'trigger', to: 'n_welcome', branch: null },
      { id: 'e2', from: 'n_welcome', to: 'exit', branch: null },
    ],
  } };
  const subscriber = { id: SUBSCRIBER_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, list_id: list.id, email: 'ada@acme.test', first_name: 'Ada', last_name: null, attributes: {}, status: 'active', contact_id: null, consent_source: 'manual', consent_attested_by: null, consent_at: '', confirmed_at: null, unsubscribed_at: null, status_reason: null, tags: [], created_at: '', updated_at: '' };
  const enrollment = { id: ENROLLMENT_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, automation_id: AUTOMATION_ID, version_id: VERSION_ID, subscriber_id: subscriber.id, status: 'active', current_node_id: 'n_welcome', wake_at: null, node_attempts: 0, last_error: null, exit_reason: null, source_event_id: null, enrolled_at: '2026-09-01T00:00:00Z', finished_at: null, updated_at: '2026-09-01T00:00:00Z' };
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}`, (route) => route.fulfill({ json: activeAutomation }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/versions/${VERSION_ID}`, (route) => route.fulfill({ json: activeVersion }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/stats*`, (route) => route.fulfill({ json: {
    automation_id: AUTOMATION_ID, version_id: VERSION_ID,
    enrollments: { active: 1, completed: 0, exited: 0, errored: 0 },
    nodes: [
      { node_id: 'trigger', node_kind: 'trigger', entered: 1, waiting: 0, advanced: 1, sent: 0, opened: 0, clicked: 0, branch_yes: 0, branch_no: 0, exited: 0, errors: 0 },
      { node_id: 'n_welcome', node_kind: 'send_email', entered: 1, waiting: 0, advanced: 0, sent: 1, opened: 1, clicked: 0, branch_yes: 0, branch_no: 0, exited: 0, errors: 0 },
      { node_id: 'exit', node_kind: 'exit', entered: 0, waiting: 0, advanced: 0, sent: 0, opened: 0, clicked: 0, branch_yes: 0, branch_no: 0, exited: 0, errors: 0 },
    ],
  } }));
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${list.id}/subscribers*`, (route) => {
    const params = new URL(route.request().url()).searchParams;
    const q = params.get('q');
    const items = q && !subscriber.email.includes(q) && !(subscriber.first_name ?? '').toLowerCase().includes(q.toLowerCase()) ? [] : [subscriber];
    return route.fulfill({ json: { items, next_cursor: null } });
  });
  let enrolled: Record<string, unknown> | null = null;
  let exited = false;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/enrollments/${ENROLLMENT_ID}/exit`, (route) => {
    exited = true;
    return route.fulfill({ json: { ...enrollment, status: 'exited', exit_reason: 'manual', current_node_id: null } });
  });
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/automations/${AUTOMATION_ID}/enrollments`, (route) => {
    if (route.request().method() === 'POST') {
      enrolled = route.request().postDataJSON();
      const created = { ...enrollment, id: CREATED_ENROLLMENT_ID, subscriber_id: enrolled.subscriber_id };
      return route.fulfill({ status: 201, json: created });
    }
    const row = exited ? { ...enrollment, status: 'exited', exit_reason: 'manual', current_node_id: null } : enrollment;
    return route.fulfill({ json: { items: [row], next_cursor: null } });
  });
  await page.goto(`/mailing/${BUSINESS_ID}/automations/${AUTOMATION_ID}`);
  await expect(page.getByTestId('node-stats')).toHaveCount(0);
  await page.getByTestId('automation-stats-toggle').check();
  await expect(page.getByTestId('node-stats')).toHaveCount(3);
  const welcomeStats = page.locator('[data-node-id="n_welcome"] + [data-testid="node-stats"]');
  await expect(welcomeStats.getByTestId('node-stat-entered')).toHaveText(/1/);
  await expect(welcomeStats.getByTestId('node-stat-sent')).toHaveText(/1/);
  await expect(welcomeStats.getByTestId('node-stat-opened')).toHaveText(/1/);
  await expect(welcomeStats.getByTestId('node-stat-clicked')).toHaveText(/0/);
  const triggerStats = page.locator('[data-node-id="trigger"] + [data-testid="node-stats"]');
  await expect(triggerStats.getByTestId('node-stat-sent')).toHaveCount(0);
  await page.getByTestId('automation-stats-toggle').uncheck();
  await expect(page.getByTestId('node-stats')).toHaveCount(0);

  // Enrollments: tab switch, table row, manual exit, manual enroll.
  await page.getByTestId('automation-tab-enrollments').click();
  await expect(page.getByTestId('enrollments-tab')).toBeVisible();
  await expect(page.getByTestId('enrollments-table')).toContainText('ada@acme.test');
  await expect(page.getByTestId('enrollments-table')).toContainText('active');
  await expect(page.getByTestId('enrollment-exit')).toBeVisible();
  page.on('dialog', (dialog) => dialog.accept());
  await page.getByTestId('enrollment-exit').click();
  await expect(page.getByTestId('enrollments-table')).not.toContainText('active');

  await page.getByTestId('enrollment-enroll').click();
  await expect(page.getByTestId('enroll-dialog-backdrop')).toBeVisible();
  await page.getByTestId('enroll-search').fill('ada');
  await expect(page.getByTestId('enroll-candidate')).toBeVisible();
  await page.getByTestId('enroll-candidate-select').click();
  await expect.poll(() => enrolled).not.toBeNull();
  expect(enrolled).toMatchObject({ subscriber_id: subscriber.id });
});
