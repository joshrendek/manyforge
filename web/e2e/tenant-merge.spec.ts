import { Page, expect, test } from '@playwright/test';

type MergeStatus = 'preflight_required' | 'ready' | 'running' | 'succeeded' | 'failed';

interface MockBusiness {
  id: string;
  parent_id: string | null;
  tenant_root_id: string;
  name: string;
  status: string;
  is_tenant_root: boolean;
}

const source: MockBusiness = {
  id: 'source-root',
  parent_id: null,
  tenant_root_id: 'source-root',
  name: 'Source Holdings',
  status: 'active',
  is_tenant_root: true,
};

const destinationRoot: MockBusiness = {
  id: 'destination-root',
  parent_id: null,
  tenant_root_id: 'destination-root',
  name: 'Destination Group',
  status: 'active',
  is_tenant_root: true,
};

const destination: MockBusiness = {
  id: 'platform',
  parent_id: destinationRoot.id,
  tenant_root_id: destinationRoot.id,
  name: 'Platform Division',
  status: 'active',
  is_tenant_root: false,
};

const authorizedOptions = {
  source_root_id: source.id,
  source_root_name: source.name,
  destinations: [
    {
      id: destinationRoot.id,
      name: destinationRoot.name,
      tenant_root_id: destinationRoot.id,
      tenant_root_name: destinationRoot.name,
      hierarchy_path: destinationRoot.name,
      is_tenant_root: true,
    },
    {
      id: destination.id,
      name: destination.name,
      tenant_root_id: destinationRoot.id,
      tenant_root_name: destinationRoot.name,
      hierarchy_path: `${destinationRoot.name} / ${destination.name}`,
      is_tenant_root: false,
    },
  ],
};

function operation(status: MergeStatus, overrides: Record<string, unknown> = {}) {
  return {
    id: 'merge-operation',
    correlation_id: 'merge-correlation',
    source_root_id: source.id,
    destination_parent_id: destination.id,
    destination_root_id: destinationRoot.id,
    actor_principal_id: 'owner',
    idempotency_key: 'dashboard-e2e',
    status,
    preflight_generation: status === 'preflight_required' ? null : 'generation-1',
    module_counts: {
      tenancy: { rows: 3, bytes: 800 },
      crm: { rows: 12, bytes: 2400 },
      support: { rows: 8, bytes: 1800 },
    },
    conflicts: [],
    warnings: [
      {
        code: 'attachment_prestage_required',
        module: 'support',
        object: 'attachment',
        count: 2,
        bytes: 1200,
      },
    ],
    affected_rows: 23,
    estimated_bytes: 5000,
    source_businesses: 3,
    resulting_depth: 4,
    attachment_count: 2,
    attachment_bytes: 1200,
    preflight_completed_at: '2026-07-29T00:00:00Z',
    ready_at: status === 'ready' ? '2026-07-29T00:00:00Z' : null,
    confirmed_at: null,
    created_at: '2026-07-29T00:00:00Z',
    updated_at: '2026-07-29T00:00:00Z',
    events: [],
    failure: null,
    manifest: null,
    ...overrides,
  };
}

async function authenticate(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'tenant-merge-access');
    localStorage.setItem('mf_refresh', 'tenant-merge-refresh');
  });
}

test('real clicks select a target, review consequences, confirm, and reload the moved hierarchy', async ({
  page,
}) => {
  await authenticate(page);
  const businesses = [{ ...source }, { ...destinationRoot }, { ...destination }];
  let current = operation('ready');

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    if (path === '/api/v1/me') {
      return route.fulfill({
        json: {
          id: 'owner',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      });
    }
    if (path === '/api/v1/tenant-merge-options') {
      return route.fulfill({ json: { sources: [authorizedOptions] } });
    }
    if (path === '/api/v1/businesses' && request.method() === 'GET') {
      return route.fulfill({ json: { items: businesses, next_cursor: null } });
    }
    if (path === `/api/v1/businesses/${source.id}` && request.method() === 'GET') {
      return route.fulfill({ json: source });
    }
    if (path === `/api/v1/businesses/${destination.id}` && request.method() === 'GET') {
      return route.fulfill({ json: destination });
    }
    if (path === `/api/v1/businesses/${source.id}/tenant-merges` && request.method() === 'POST') {
      expect(request.postDataJSON()).toEqual({
        destination_parent_id: destination.id,
      });
      expect(request.headers()['idempotency-key']).toMatch(/^dashboard-/);
      return route.fulfill({ status: 201, json: current });
    }
    if (path === '/api/v1/tenant-merges/merge-operation' && request.method() === 'GET') {
      return route.fulfill({ json: current });
    }
    if (path === '/api/v1/tenant-merges/merge-operation/confirm' && request.method() === 'POST') {
      expect(request.postDataJSON()).toEqual({
        source_name: source.name,
        destination_name: destination.name,
        password: 'current-password',
      });
      current = operation('running');
      await new Promise((resolve) => setTimeout(resolve, 450));
      const movedSource = businesses.find((business) => business.id === source.id)!;
      movedSource.parent_id = destination.id;
      movedSource.tenant_root_id = destinationRoot.id;
      movedSource.is_tenant_root = false;
      current = operation('succeeded');
      return route.fulfill({ json: current });
    }
    return route.fulfill({ status: 404, json: { code: 'NOT_FOUND' } });
  });

  await page.goto('/dashboard');
  const sourceRow = page.getByTestId('biz-row').filter({ hasText: source.name });
  await sourceRow.getByTestId('move-master').click();
  await expect(page).toHaveURL(/\/tenant-merges\/new\/source-root$/);

  await page.getByTestId('destination-search').fill('platform');
  const target = page.getByTestId('destination-option');
  await expect(target).toHaveCount(1);
  await expect(target).toContainText('Destination Group / Platform Division');
  await target.click();
  await page.getByTestId('review-merge').click();

  await expect(page).toHaveURL(/\/tenant-merges\/merge-operation$/);
  await expect(page.getByTestId('merge-review')).toContainText(
    'All source users and data enter the destination tenant boundary',
  );
  await expect(page.getByRole('table', { name: 'Per-module impact' })).toContainText('support');
  await expect(page.getByTestId('resulting-tree-preview')).toContainText(source.name);
  await expect(page.getByTestId('merge-warnings')).toContainText('Attachment Prestage Required');

  const start = page.getByTestId('start-merge');
  await expect(start).toBeDisabled();
  await page.getByTestId('confirm-source').fill(source.name);
  await page.getByTestId('confirm-destination').fill(destination.name);
  await page.getByTestId('confirm-password').fill('current-password');
  await expect(start).toBeEnabled();
  await start.click();
  await expect(page.getByTestId('merge-running')).toContainText(
    'reopening this URL resumes durable status monitoring',
  );
  await expect(page.getByTestId('merge-success')).toBeVisible();
  const movedSource = page
    .getByTestId('result-hierarchy')
    .locator(`[data-business-id="${source.id}"]`);
  await expect(movedSource).toContainText(source.name);
  await expect(movedSource).not.toContainText('master');
});

test('blocked and stale preflights never enable start', async ({ page }) => {
  await authenticate(page);
  let current = operation('preflight_required', {
    conflicts: [
      {
        code: 'company_domain_collision',
        module: 'crm',
        object: 'company',
        count: 2,
      },
    ],
  });

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === '/api/v1/me') {
      return route.fulfill({
        json: {
          id: 'owner',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      });
    }
    if (path === '/api/v1/tenant-merges/merge-operation') {
      return route.fulfill({ json: current });
    }
    if (path === `/api/v1/businesses/${source.id}`) {
      return route.fulfill({ json: source });
    }
    if (path === `/api/v1/businesses/${destination.id}`) {
      return route.fulfill({ json: destination });
    }
    if (path === '/api/v1/tenant-merge-options') {
      return route.fulfill({ json: { sources: [authorizedOptions] } });
    }
    if (path === '/api/v1/businesses') {
      return route.fulfill({ json: { items: [source] } });
    }
    if (path.endsWith('/preflight')) {
      current = operation('ready');
      return route.fulfill({ json: current });
    }
    if (path.endsWith('/confirm')) {
      current = operation('preflight_required');
      return route.fulfill({
        status: 412,
        json: { code: 'STALE_PREFLIGHT', message: 'preflight is stale' },
      });
    }
    return route.fulfill({ status: 404, json: { code: 'NOT_FOUND' } });
  });

  await page.goto('/tenant-merges/merge-operation');
  await expect(page.getByTestId('merge-blockers')).toContainText('Company Domain Collision');
  await expect(page.getByTestId('start-merge')).toBeDisabled();
  await page.getByTestId('rerun-preflight').click();
  await expect(page.getByTestId('merge-blockers')).toHaveCount(0);

  await page.getByTestId('confirm-source').fill(source.name);
  await page.getByTestId('confirm-destination').fill(destination.name);
  await page.getByTestId('confirm-password').fill('current-password');
  await page.getByTestId('start-merge').click();
  await expect(page.getByTestId('merge-review')).toContainText('review required');
  await expect(page.getByTestId('rerun-preflight')).toBeVisible();
  await expect(page.getByTestId('start-merge')).toBeDisabled();
});

test('a stable URL resumes running status and does not suggest an unsafe duplicate', async ({
  page,
}) => {
  await authenticate(page);
  let statusReads = 0;
  let current = operation('running');

  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/v1/me') {
      return route.fulfill({
        json: {
          id: 'owner',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      });
    }
    if (path === '/api/v1/tenant-merges/merge-operation') {
      statusReads++;
      if (statusReads > 1) current = operation('succeeded');
      return route.fulfill({ json: current });
    }
    if (path === `/api/v1/businesses/${source.id}`) {
      return route.fulfill({ json: source });
    }
    if (path === `/api/v1/businesses/${destination.id}`) {
      return route.fulfill({ json: destination });
    }
    if (path === '/api/v1/tenant-merge-options') {
      return route.fulfill({ json: { sources: [authorizedOptions] } });
    }
    if (path === '/api/v1/businesses') {
      return route.fulfill({
        json: { items: [destinationRoot, destination, source] },
      });
    }
    return route.fulfill({ status: 404, json: { code: 'NOT_FOUND' } });
  });

  await page.goto('/tenant-merges/merge-operation');
  await expect(page).toHaveURL(/\/tenant-merges\/merge-operation$/);
  await expect(page.getByTestId('merge-running')).toContainText(
    'operator intervention is required',
  );
  await expect(page.getByTestId('start-merge')).toHaveCount(0);
  await expect(page.getByTestId('merge-success')).toBeVisible({ timeout: 5000 });
});

test('permissions hide Move master and terminal failure explains safe rollback', async ({
  page,
}) => {
  await authenticate(page);
  const failed = operation('failed', {
    failure: {
      code: 'CUTOVER_FAILED',
      stage: 'rewrite_catalog',
      operator_correlation_id: 'merge-correlation',
    },
  });

  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/v1/me') {
      return route.fulfill({
        json: {
          id: 'owner',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      });
    }
    if (path === '/api/v1/businesses') {
      return route.fulfill({ json: { items: [source], next_cursor: null } });
    }
    if (path === '/api/v1/tenant-merge-options') {
      return route.fulfill({ json: { sources: [] } });
    }
    if (path === '/api/v1/tenant-merges/merge-operation') {
      return route.fulfill({ json: failed });
    }
    if (path === `/api/v1/businesses/${source.id}`) {
      return route.fulfill({ json: source });
    }
    if (path === `/api/v1/businesses/${destination.id}`) {
      return route.fulfill({ json: destination });
    }
    return route.fulfill({ status: 404, json: { code: 'NOT_FOUND' } });
  });

  await page.goto('/dashboard');
  await expect(page.getByTestId('move-master')).toHaveCount(0);

  await page.goto('/tenant-merges/merge-operation');
  const failure = page.getByTestId('merge-failed');
  await expect(failure).toContainText('rolled back safely');
  await expect(failure).toContainText('Do not repeat');
  await expect(page.getByTestId('start-merge')).toHaveCount(0);
  await expect(page.getByTestId('rerun-preflight')).toHaveCount(0);
});
