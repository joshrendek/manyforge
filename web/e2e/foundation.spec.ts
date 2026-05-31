import { Page, expect, test } from '@playwright/test';

// Foundation flow (T081): the cohesive SPA journey signup → verify → login →
// create master → add sub, plus the scoped-access outcome an invited member sees
// on login. Backend behaviour (closure, RLS, invitations) is covered by the Go
// integration/security tests; these specs pin the SPA wiring end-to-end against a
// mocked /api/v1 so the flow is deterministic (mirrors us1/us2). Note: the SPA has
// no invitation-management UI yet, so the "invite" step is exercised at the API
// layer (Go) and its *result* — a scoped login — is asserted here.

interface MockBiz {
  id: string;
  parent_id: string | null;
  tenant_root_id: string;
  name: string;
  status: string;
  is_tenant_root: boolean;
}

// mockBusinesses installs a stateful businesses API over the given store.
async function mockBusinesses(page: Page, store: MockBiz[]) {
  await page.route('**/api/v1/businesses**', async (route) => {
    const req = route.request();
    if (req.method() === 'GET') {
      return route.fulfill({ json: { items: store, next_cursor: null } });
    }
    if (req.method() === 'POST' && /\/businesses$/.test(new URL(req.url()).pathname)) {
      const body = req.postDataJSON() as { name: string; parent_id?: string };
      const biz: MockBiz = {
        id: 'id-' + body.name,
        name: body.name,
        parent_id: body.parent_id ?? null,
        tenant_root_id: body.parent_id ? store[0].tenant_root_id : 'id-' + body.name,
        status: 'active',
        is_tenant_root: !body.parent_id,
      };
      store.push(biz);
      return route.fulfill({ status: 201, json: biz });
    }
    return route.fulfill({ status: 204, body: '' });
  });
}

const profile = {
  id: 'u1',
  email: 'founder@manyforge.test',
  display_name: 'Founder',
  email_verified: true,
  status: 'active',
};

test('foundation journey: signup → verify → login → create master → add sub', async ({ page }) => {
  // Auth endpoints for the full sign-up/verify/login handshake.
  await page.route('**/api/v1/auth/signup', (route) => route.fulfill({ status: 202, body: '' }));
  await page.route('**/api/v1/auth/verify-email', (route) => route.fulfill({ status: 204, body: '' }));
  await page.route('**/api/v1/auth/login', (route) =>
    route.fulfill({ json: { access_token: 'access-1', refresh_token: 'refresh-1', expires_in: 900 } }),
  );
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: profile }));
  const store: MockBiz[] = [];
  await mockBusinesses(page, store);

  // 1) Sign up.
  await page.goto('/signup');
  await page.locator('#email').fill('founder@manyforge.test');
  await page.locator('#displayName').fill('Founder');
  await page.locator('#password').fill('supersecretpassword');
  await page.getByRole('button', { name: 'Create account' }).click();

  // 2) Verify (the SPA reveals the verification-token step).
  await page.locator('#token').fill('verification-token');
  await page.getByRole('button', { name: 'Verify email' }).click();
  await expect(page).toHaveURL(/\/login$/);

  // 3) Log in → dashboard.
  await page.locator('#email').fill('founder@manyforge.test');
  await page.locator('#password').fill('supersecretpassword');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByRole('heading', { name: 'Your businesses' })).toBeVisible();

  // 4) Create the master business (empty-state form), posting with no parent_id.
  await expect(page.getByText('No businesses yet — create your master business below.')).toBeVisible();
  const [masterReq] = await Promise.all([
    page.waitForRequest((r) => /\/api\/v1\/businesses$/.test(r.url()) && r.method() === 'POST'),
    (async () => {
      await page.locator('#bizname').fill('Acme');
      await page.getByRole('button', { name: 'Create master business' }).click();
    })(),
  ]);
  expect(masterReq.postDataJSON()).toMatchObject({ name: 'Acme' });
  expect(masterReq.postDataJSON()).not.toHaveProperty('parent_id');
  const master = page.getByTestId('biz-row').filter({ hasText: 'Acme' });
  await expect(master).toBeVisible();
  await expect(master.getByText('master')).toBeVisible();

  // 5) Add a sub-business under the master, posting with its parent_id.
  await master.getByRole('button', { name: 'Add sub' }).click();
  const [subReq] = await Promise.all([
    page.waitForRequest((r) => /\/api\/v1\/businesses$/.test(r.url()) && r.method() === 'POST'),
    (async () => {
      await page.getByTestId('sub-name-input').fill('Engineering');
      await page.getByRole('button', { name: 'Create sub-business' }).click();
    })(),
  ]);
  expect(subReq.postDataJSON()).toMatchObject({ name: 'Engineering', parent_id: 'id-Acme' });
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Engineering' })).toBeVisible();
  await expect(page.getByTestId('biz-row')).toHaveCount(2);
});

test('an invited member, on login, sees only the business they were scoped to', async ({ page }) => {
  // A member invited to "Engineering" only: their RLS-scoped /businesses returns
  // that subtree and nothing of the master tenant above/beside it (FR-011).
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'member-access');
    localStorage.setItem('mf_refresh', 'member-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({ json: { ...profile, id: 'm1', email: 'member@manyforge.test', display_name: 'Member' } }),
  );
  const scoped: MockBiz[] = [
    { id: 'eng', parent_id: null, tenant_root_id: 'eng', name: 'Engineering', status: 'active', is_tenant_root: true },
  ];
  await mockBusinesses(page, scoped);

  await page.goto('/dashboard');
  await expect(page.getByTestId('biz-row')).toHaveCount(1);
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Engineering' })).toBeVisible();
  // The master tenant and any sibling are never visible to this member.
  await expect(page.getByTestId('biz-row').filter({ hasText: 'Acme' })).toHaveCount(0);
});
