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

// Two realistic activity entries for c1, newest first (matches the backend's
// descending occurred_at ordering). Shapes mirror the ActivityEntry DTO exactly.
const a1 = {
  id: 'act1', tenant_root_id: 'b1', business_id: 'b1', contact_id: 'c1',
  kind: 'ticket_created', occurred_at: '2026-06-13T10:00:00Z', actor: 'ada@acme.com',
  source_type: 'ticket', source_id: 't1', summary: 'Opened ticket: Cannot log in',
  created_at: '2026-06-13T10:00:00Z',
};
const a2 = {
  id: 'act2', tenant_root_id: 'b1', business_id: 'b1', contact_id: 'c1',
  kind: 'email_received', occurred_at: '2026-06-12T09:00:00Z', actor: 'ada@acme.com',
  source_type: 'email', source_id: 'e1', summary: 'Re: welcome aboard',
  created_at: '2026-06-12T09:00:00Z',
};
const activity = { items: [a1, a2], next_cursor: null };
const activityEmpty = { items: [], next_cursor: null };

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
  // The detail page also fires the activity GET on load; mock it (more specific
  // glob, registered before the bare /contacts route) so it never leaks to the network.
  await page.route('**/api/v1/businesses/b1/contacts/c1/activity', (r) => r.fulfill({ json: activity }));
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

test('contact detail: renders activity timeline', async ({ page }) => {
  await auth(page);
  // More specific globs first so they win over the bare /contacts list route.
  await page.route('**/api/v1/businesses/b1/contacts/c1/activity', (r) => r.fulfill({ json: activity }));
  await page.route('**/api/v1/businesses/b1/contacts/c1', (r) => r.fulfill({ json: c1 }));
  await page.route('**/api/v1/businesses/b1/companies', (r) => r.fulfill({ json: companies }));
  await page.route('**/api/v1/businesses/b1/contacts', (r) => r.fulfill({ json: contacts }));

  await page.goto('/crm/b1/contacts/c1');
  await expect(page.getByTestId('contact-detail-email')).toContainText('ada@acme.com');

  // The timeline renders one row per entry, newest first.
  await expect(page.getByTestId('activity-timeline')).toBeVisible();
  await expect(page.getByTestId('activity-row')).toHaveCount(2);
  // The first row carries the kind LABEL and the summary text of the newest entry.
  const firstRow = page.getByTestId('activity-row').first();
  await expect(firstRow).toContainText('Ticket created');
  await expect(firstRow).toContainText('Opened ticket: Cannot log in');
});

test('contact detail: empty activity timeline', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/contacts/c1/activity', (r) => r.fulfill({ json: activityEmpty }));
  await page.route('**/api/v1/businesses/b1/contacts/c1', (r) => r.fulfill({ json: c1 }));
  await page.route('**/api/v1/businesses/b1/companies', (r) => r.fulfill({ json: companies }));
  await page.route('**/api/v1/businesses/b1/contacts', (r) => r.fulfill({ json: contacts }));

  await page.goto('/crm/b1/contacts/c1');
  await expect(page.getByTestId('contact-detail-email')).toContainText('ada@acme.com');

  // With no entries the empty state shows and no rows render.
  await expect(page.getByTestId('activity-empty')).toBeVisible();
  await expect(page.getByTestId('activity-row')).toHaveCount(0);
});

test('companies: renders list', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/companies', (r) => r.fulfill({ json: companies }));
  await page.goto('/crm/companies');
  await expect(page.getByTestId('company-name-cell').first()).toContainText('Acme');
  await expect(page.getByTestId('company-domain-cell').first()).toContainText('acme.com');
});
