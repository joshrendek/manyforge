import { expect, test } from '@playwright/test';

const profile = {
  id: '1',
  email: 'a@b.c',
  display_name: 'A',
  email_verified: true,
  status: 'active',
};
const biz = {
  items: [
    {
      id: 'b1',
      parent_id: null,
      tenant_root_id: 'b1',
      name: 'Acme',
      status: 'active',
      is_tenant_root: true,
    },
  ],
  next_cursor: null,
};

const board = {
  id: 'bd1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  slug: 'mobile-app',
  name: 'Mobile App',
  description: 'Tell us what to build',
  is_public: true,
  created_at: '2026-07-01T00:00:00Z',
  updated_at: '2026-07-01T00:00:00Z',
};
const boards = { items: [board], next_cursor: null };
const post = {
  id: 'p1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  board_id: 'bd1',
  title: 'Add Face ID login',
  body: 'Biometric unlock',
  status: 'open',
  vote_count: 7,
  author_kind: 'public',
  author_principal_id: null,
  author_identity: 'device-1',
  ticket_id: null,
  created_at: '2026-07-02T00:00:00Z',
  updated_at: '2026-07-02T00:00:00Z',
};
const posts = { items: [post], next_cursor: null };
const key = {
  id: 'k1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  board_id: 'bd1',
  publishable_key: 'fbk_demo123',
  label: 'iOS app',
  status: 'enabled',
  created_at: '2026-07-01T00:00:00Z',
  revoked_at: null,
};
const keys = { items: [key] };

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

// Register the detail-page routes. More specific globs first so they win over the bare
// list routes (mirrors the ordering discipline in crm.spec.ts).
async function boardDetailRoutes(page: import('@playwright/test').Page) {
  await page.route('**/api/v1/businesses/b1/feedback/posts/p1/convert', (r) =>
    r.fulfill({ json: { ticket_id: 't-42' } }),
  );
  await page.route('**/api/v1/businesses/b1/feedback/posts/p1', (r) =>
    r.fulfill({ json: { ...post, status: r.request().postDataJSON()?.status ?? 'open' } }),
  );
  await page.route('**/api/v1/businesses/b1/feedback/boards/bd1/posts', (r) =>
    r.fulfill({ json: posts }),
  );
  await page.route('**/api/v1/businesses/b1/feedback/boards/bd1/keys', (r) =>
    r.fulfill({ json: keys }),
  );
  await page.route('**/api/v1/businesses/b1/feedback/boards/bd1', (r) =>
    r.fulfill({ json: board }),
  );
}

test('feedback: boards list renders a board row', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/feedback/boards', (r) => r.fulfill({ json: boards }));
  await page.goto('/feedback');
  await expect(page.getByTestId('board-row')).toHaveCount(1);
  await expect(page.getByTestId('board-name-cell').first()).toContainText('Mobile App');
  await expect(page.getByTestId('board-visibility-cell').first()).toContainText('Public');
});

test('feedback: board detail renders posts + keys', async ({ page }) => {
  await auth(page);
  await boardDetailRoutes(page);
  await page.goto('/feedback/b1/bd1');
  await expect(page.getByTestId('board-detail-name')).toContainText('Mobile App');
  await expect(page.getByTestId('post-title').first()).toContainText('Add Face ID login');
  await expect(page.getByTestId('post-votes').first()).toContainText('7');
  await expect(page.getByTestId('key-value').first()).toContainText('fbk_demo123');
});

test('feedback: moderate a post status via the select (PATCH)', async ({ page }) => {
  await auth(page);
  await boardDetailRoutes(page);
  let patchBody: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/feedback/posts/p1', (r) => {
    if (r.request().method() === 'PATCH') {
      patchBody = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ json: { ...post, status: patchBody['status'] } });
    }
    return r.fulfill({ json: post });
  });
  await page.goto('/feedback/b1/bd1');
  await page.getByTestId('post-status-select').selectOption('planned');
  await expect.poll(() => patchBody).not.toBeNull();
  expect(patchBody!['status']).toBe('planned');
});

test('feedback: convert a post to a ticket shows the ticket link', async ({ page }) => {
  await auth(page);
  await boardDetailRoutes(page);
  await page.goto('/feedback/b1/bd1');
  await page.getByTestId('post-convert').click();
  await expect(page.getByTestId('post-ticket-link')).toBeVisible();
});
