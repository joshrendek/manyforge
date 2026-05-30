import { Page, expect, test } from '@playwright/test';

// US2 hierarchy SPA regression. The backend behaviour (closure rewrites, cycle
// guards, RLS) is covered by Go tests; these specs pin the SPA's tree rendering
// and the wiring of each mutation. The API is mocked via page.route so the flow
// is deterministic and self-contained (mirrors us1.spec.ts's philosophy).

interface MockBiz {
  id: string;
  parent_id: string | null;
  tenant_root_id: string;
  name: string;
  status: string;
  is_tenant_root: boolean;
}

function seed(): MockBiz[] {
  const mk = (id: string, name: string, parent: string | null): MockBiz => ({
    id,
    name,
    parent_id: parent,
    tenant_root_id: 'r',
    status: 'active',
    is_tenant_root: parent === null,
  });
  return [mk('r', 'Acme', null), mk('e', 'Engineering', 'r'), mk('s', 'Sales', 'r'), mk('b', 'Backend', 'e')];
}

// installStack seeds an authenticated session and a stateful businesses API.
// Returns the mutable store so tests can assert/override.
async function installStack(page: Page, businesses: MockBiz[]) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' },
    }),
  );
  await page.route('**/api/v1/businesses**', async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const method = req.method();
    const m = url.pathname.match(/\/businesses\/([^/]+)(\/(\w+))?$/);
    const id = m?.[1];
    const action = m?.[3];

    if (method === 'GET') {
      return route.fulfill({ json: { items: businesses, next_cursor: null } });
    }
    if (method === 'POST' && !id) {
      const body = req.postDataJSON() as { name: string; parent_id?: string };
      businesses.push({
        id: 'new-' + body.name,
        name: body.name,
        parent_id: body.parent_id ?? null,
        tenant_root_id: 'r',
        status: 'active',
        is_tenant_root: !body.parent_id,
      });
      return route.fulfill({ status: 201, json: businesses[businesses.length - 1] });
    }
    const target = businesses.find((b) => b.id === id);
    if (method === 'POST' && action === 'archive' && target) target.status = 'archived';
    if (method === 'POST' && action === 'restore' && target) target.status = 'active';
    if (method === 'POST' && action === 'move' && target) {
      target.parent_id = (req.postDataJSON() as { new_parent_id: string }).new_parent_id;
    }
    if (method === 'PATCH' && target) target.name = (req.postDataJSON() as { name: string }).name;
    if (method === 'DELETE' && id) {
      const i = businesses.findIndex((b) => b.id === id);
      if (i >= 0) businesses.splice(i, 1);
    }
    return route.fulfill({ status: 204, body: '' });
  });
}

test('renders the business hierarchy nested, master tagged, in sorted order', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  await expect(page.getByRole('heading', { name: 'Your businesses' })).toBeVisible();

  const rows = page.getByTestId('biz-row');
  await expect(rows).toHaveCount(4);
  // pre-order, siblings alphabetical: Acme > Engineering > Backend > Sales
  await expect(rows.nth(0)).toContainText('Acme');
  await expect(rows.nth(1)).toContainText('Engineering');
  await expect(rows.nth(2)).toContainText('Backend');
  await expect(rows.nth(3)).toContainText('Sales');
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Acme' }).getByText('master')).toBeVisible();
});

test('collapsing a node hides its descendants', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  await expect(page.getByText('Backend')).toBeVisible();
  // collapse Engineering -> Backend disappears, Sales stays
  await page.getByTestId('biz-row').filter({ hasText: 'Engineering' }).getByRole('button', { name: 'Collapse' }).click();
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Backend' })).toHaveCount(0);
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Sales' })).toBeVisible();
});

test('adding a sub-business posts with the parent_id and shows the new node', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  const acme = page.getByTestId('biz-row').filter({ hasText: 'Acme' });
  await acme.getByRole('button', { name: 'Add sub' }).click();

  const [req] = await Promise.all([
    page.waitForRequest((r) => r.url().endsWith('/api/v1/businesses') && r.method() === 'POST'),
    (async () => {
      await page.getByTestId('sub-name-input').fill('Marketing');
      await page.getByRole('button', { name: 'Create sub-business' }).click();
    })(),
  ]);
  expect(req.postDataJSON()).toMatchObject({ name: 'Marketing', parent_id: 'r' });
  await expect(page.getByText('Marketing')).toBeVisible();
});

test('deleting requires confirmation and sends confirm=true', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  const backend = page.getByTestId('biz-row').filter({ hasText: 'Backend' });
  await backend.getByRole('button', { name: 'Delete' }).click();

  const [req] = await Promise.all([
    page.waitForRequest((r) => /\/businesses\/b$/.test(r.url()) && r.method() === 'DELETE'),
    page.getByRole('button', { name: 'Confirm delete' }).click(),
  ]);
  expect(req.postDataJSON()).toMatchObject({ confirm: true });
  // The row (and its confirm panel) is gone once the delete reloads the list.
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Backend' })).toHaveCount(0);
});

test('a 409 conflict on delete surfaces a friendly message', async ({ page }) => {
  const businesses = seed();
  await installStack(page, businesses);
  // Engineering has a child -> backend refuses; simulate the 409.
  await page.route('**/api/v1/businesses/e', (route) =>
    route.request().method() === 'DELETE'
      ? route.fulfill({ status: 409, json: { code: 'CONFLICT', message: 'has active children' } })
      : route.fallback(),
  );
  await page.goto('/dashboard');
  const eng = page.getByTestId('biz-row').filter({ hasText: 'Engineering' });
  await eng.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('button', { name: 'Confirm delete' }).click();
  await expect(page.getByText(/can.t be deleted while it has active sub-businesses/i)).toBeVisible();
});

test('archiving a node marks it archived and offers restore', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  const sales = page.getByTestId('biz-row').filter({ hasText: 'Sales' });
  await sales.getByRole('button', { name: 'Archive' }).click();
  await expect(sales.getByText('archived')).toBeVisible();
  await expect(sales.getByRole('button', { name: 'Restore' })).toBeVisible();
});

test('moving a sub-business re-parents it; the target menu excludes self and the current parent', async ({ page }) => {
  await installStack(page, seed());
  await page.goto('/dashboard');
  // Backend sits under Engineering — initial order: Acme, Engineering, Backend, Sales.
  await page.getByTestId('biz-row').filter({ hasText: 'Backend' }).getByRole('button', { name: 'Move' }).click();

  // The picker offers a valid new parent (Sales) but never the node itself or its
  // current parent (Engineering) — those would be no-ops / cycles.
  const select = page.locator('#move-b');
  await expect(select.locator('option', { hasText: 'Sales' })).toHaveCount(1);
  await expect(select.locator('option', { hasText: 'Backend' })).toHaveCount(0);
  await expect(select.locator('option', { hasText: 'Engineering' })).toHaveCount(0);

  const [req] = await Promise.all([
    page.waitForRequest((r) => /\/businesses\/b\/move$/.test(r.url()) && r.method() === 'POST'),
    (async () => {
      await select.selectOption('s');
      await page.getByRole('button', { name: 'Move here' }).click();
    })(),
  ]);
  expect(req.postDataJSON()).toMatchObject({ new_parent_id: 's' });

  // After the reload Backend is nested under Sales — order: Acme, Engineering, Sales, Backend.
  const rows = page.getByTestId('biz-row');
  await expect(rows).toHaveCount(4);
  await expect(rows.nth(1)).toContainText('Engineering');
  await expect(rows.nth(2)).toContainText('Sales');
  await expect(rows.nth(3)).toContainText('Backend');
});
