import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };

const c1 = {
  id: 'c1', tenant_root_id: 'b1', primary_email: 'ada@acme.com', display_name: 'Ada Lovelace', company_id: 'co1',
  created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
};
const c2 = {
  id: 'c2', tenant_root_id: 'b1', primary_email: 'bob@acme.com', display_name: 'Bob', company_id: null,
  created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
};
const contacts = { items: [c1, c2], next_cursor: null };
const companies = {
  items: [{ id: 'co1', tenant_root_id: 'b1', name: 'Acme', domain: 'acme.com', created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z' }],
  next_cursor: null,
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('contacts: renders list', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/contacts', (r) => r.fulfill({ json: contacts }));
  await page.goto('/crm/contacts');
  await expect(page.getByTestId('contact-row')).toHaveCount(2);
  await expect(page.getByTestId('contact-email-cell').first()).toContainText('ada@acme.com');
});

test('contact detail: renders + merge flow', async ({ page }) => {
  await auth(page);
  let mergeBody: Record<string, unknown> | null = null;
  // More specific globs first so they win over the bare /contacts list route.
  await page.route('**/api/v1/businesses/b1/contacts/c1/merge', (r) => {
    if (r.request().method() === 'POST') {
      mergeBody = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ json: { status: 'merged' } });
    }
    return r.continue();
  });
  await page.route('**/api/v1/businesses/b1/contacts/c1', (r) => r.fulfill({ json: c1 }));
  await page.route('**/api/v1/businesses/b1/companies', (r) => r.fulfill({ json: companies }));
  await page.route('**/api/v1/businesses/b1/contacts', (r) => r.fulfill({ json: contacts }));

  await page.goto('/crm/b1/contacts/c1');
  await expect(page.getByTestId('contact-detail-email')).toContainText('ada@acme.com');

  // Pick c2 (the only other contact) as the loser and merge it into c1.
  await page.getByTestId('contact-merge-select').selectOption('c2');
  await page.getByTestId('contact-merge-btn').click();

  // On success the component navigates back to the contacts list.
  await expect(page).toHaveURL(/\/crm\/contacts$/);
  // ...and the merge POST fired with the loser id in the body.
  expect(mergeBody).not.toBeNull();
  expect(mergeBody!['loser_id']).toBe('c2');
});

test('companies: renders list', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/companies', (r) => r.fulfill({ json: companies }));
  await page.goto('/crm/companies');
  await expect(page.getByTestId('company-name-cell').first()).toContainText('Acme');
  await expect(page.getByTestId('company-domain-cell').first()).toContainText('acme.com');
});
