import { expect, test } from '@playwright/test';

const profile = {
  id: 'p1',
  email: 'operator@acme.test',
  display_name: 'Operator',
  email_verified: true,
  status: 'active',
};
const businesses = {
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
const list = {
  id: 'l1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  slug: 'product-updates',
  name: 'Product updates',
  description: 'Release notes and product news',
  double_opt_in: true,
  status: 'active',
  created_at: '2026-08-27T12:00:00Z',
  updated_at: '2026-08-27T12:00:00Z',
};
const subscriber = {
  id: 's1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  list_id: 'l1',
  email: 'ada@example.com',
  first_name: 'Ada',
  last_name: 'Lovelace',
  attributes: {},
  status: 'active',
  contact_id: null,
  consent_source: 'manual',
  consent_attested_by: null,
  consent_at: '2026-08-27T12:00:00Z',
  confirmed_at: '2026-08-27T12:00:00Z',
  unsubscribed_at: null,
  status_reason: null,
  tags: ['vip'],
  created_at: '2026-08-27T12:00:00Z',
  updated_at: '2026-08-27T12:00:00Z',
};
const key = {
  id: 'k1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  list_id: 'l1',
  publishable_key: 'mlp_demo123',
  label: 'Hosted signup',
  status: 'enabled',
  has_secret: true,
  created_at: '2026-08-27T12:00:00Z',
  revoked_at: null,
};

async function shellRoutes(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  // Register the broad fallback first. Playwright invokes the most recently registered
  // matching route first, so the specific mocks below take precedence.
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
}

test('mailing lists: render and create a double-opt-in list', async ({ page }) => {
  await shellRoutes(page);
  let posted: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/mailing/lists', (route) => {
    if (route.request().method() === 'POST') {
      posted = route.request().postDataJSON() as Record<string, unknown>;
      return route.fulfill({
        status: 201,
        json: { ...list, id: 'l2', slug: 'newsletter', name: posted['name'] },
      });
    }
    return route.fulfill({ json: { items: [list], next_cursor: null } });
  });

  await page.goto('/mailing/lists');
  await expect(page.getByTestId('mailing-list-row')).toHaveCount(1);
  await expect(page.getByTestId('mailing-list-open')).toContainText('Product updates');

  await page.getByTestId('mailing-list-name').fill('Newsletter');
  await page.getByTestId('mailing-list-create').click();
  await expect.poll(() => posted).not.toBeNull();
  expect(posted).toMatchObject({ name: 'Newsletter', double_opt_in: true });
});

test('mailing list detail: signup access, consent gate, tags, and manual add', async ({ page }) => {
  await shellRoutes(page);
  let subscribers = [subscriber];
  let posted: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/mailing/lists/l1/subscribers**', (route) => {
    if (route.request().method() === 'POST') {
      posted = route.request().postDataJSON() as Record<string, unknown>;
      const created = {
        ...subscriber,
        id: 's2',
        email: posted['email'],
        first_name: posted['first_name'],
        last_name: posted['last_name'],
        tags: posted['tags'],
      };
      subscribers = [created, ...subscribers];
      return route.fulfill({ status: 201, json: created });
    }
    return route.fulfill({ json: { items: subscribers, next_cursor: null } });
  });
  await page.route('**/api/v1/businesses/b1/contacts', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/lists/l1/keys', (route) =>
    route.fulfill({ json: { items: [key] } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/lists/l1', (route) =>
    route.fulfill({ json: list }),
  );

  await page.goto('/mailing/b1/lists/l1');
  await expect(page.getByTestId('mailing-publishable-key')).toContainText('mlp_demo123');
  await expect(page.getByTestId('mailing-hosted-url')).toContainText('/m/s/mlp_demo123');
  await expect(page.getByTestId('subscriber-row').first()).toContainText('vip');

  const importSubmit = page.getByTestId('subscriber-import-submit');
  await page.getByTestId('subscriber-import-file').setInputFiles({
    name: 'subscribers.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('email\ngrace@example.com\n'),
  });
  await expect(importSubmit).toBeDisabled();
  await page.getByTestId('subscriber-import-consent').check();
  await expect(importSubmit).toBeEnabled();

  await page.getByTestId('subscriber-email').fill('grace@example.com');
  await page.getByTestId('subscriber-first-name').fill('Grace');
  await page.getByTestId('tag-chip-text').fill('customer');
  await page.getByTestId('tag-chip-text').press('Enter');
  await page.getByTestId('subscriber-add').click();

  await expect.poll(() => posted).not.toBeNull();
  expect(posted).toMatchObject({
    email: 'grace@example.com',
    first_name: 'Grace',
    tags: ['customer'],
    skip_confirmation: false,
  });
  await expect(page.getByTestId('subscriber-row').first()).toContainText('grace@example.com');
  await expect(page.getByTestId('subscriber-row').first()).toContainText('customer');
});
