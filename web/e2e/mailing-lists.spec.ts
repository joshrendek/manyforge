import { expect, test } from '@playwright/test';

const ACCOUNT_ID = '11111111-1111-4111-8111-111111111111';
const BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const LIST_ID = '33333333-3333-4333-8333-333333333333';
const SUBSCRIBER_ID = '44444444-4444-4444-8444-444444444444';
const KEY_ID = '55555555-5555-4555-8555-555555555555';
const CREATED_LIST_ID = '66666666-6666-4666-8666-666666666666';
const CREATED_SUBSCRIBER_ID = '77777777-7777-4777-8777-777777777777';

const profile = {
  id: ACCOUNT_ID,
  email: 'operator@acme.test',
  display_name: 'Operator',
  email_verified: true,
  status: 'active',
};
const businesses = {
  items: [
    {
      id: BUSINESS_ID,
      parent_id: null,
      tenant_root_id: BUSINESS_ID,
      name: 'Acme',
      status: 'active',
      is_tenant_root: true,
    },
  ],
  next_cursor: null,
};
const list = {
  id: LIST_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  slug: 'product-updates',
  name: 'Product updates',
  description: 'Release notes and product news',
  double_opt_in: true,
  status: 'active',
  created_at: '2026-08-27T12:00:00Z',
  updated_at: '2026-08-27T12:00:00Z',
};
const subscriber = {
  id: SUBSCRIBER_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  list_id: LIST_ID,
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
  id: KEY_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  list_id: LIST_ID,
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
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) => {
    if (route.request().method() === 'POST') {
      posted = route.request().postDataJSON() as Record<string, unknown>;
      return route.fulfill({
        status: 201,
        json: { ...list, id: CREATED_LIST_ID, slug: 'newsletter', name: posted['name'] },
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
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${LIST_ID}/subscribers**`, (route) => {
    if (route.request().method() === 'POST') {
      posted = route.request().postDataJSON() as Record<string, unknown>;
      const created = {
        ...subscriber,
        id: CREATED_SUBSCRIBER_ID,
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
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/contacts`, (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${LIST_ID}/keys`, (route) =>
    route.fulfill({ json: { items: [key] } }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${LIST_ID}`, (route) =>
    route.fulfill({ json: list }),
  );

  await page.goto(`/mailing/${BUSINESS_ID}/lists/${LIST_ID}`);
  await expect(page.getByTestId('mailing-publishable-key')).toContainText('mlp_demo123');
  await expect(page.getByTestId('mailing-hosted-url')).toContainText(
    '/m/s/mlp_demo123?name=Product%20updates',
  );
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
