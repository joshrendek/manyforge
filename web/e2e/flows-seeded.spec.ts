import { expect, Page, test } from '@playwright/test';

// These pin the scenario the rest of the suite structurally missed: a desk with
// MORE THAN ONE ticket, and a thread with MORE THAN ONE message (the empty/single
// blind spot that let "the desk doesn't function" ship green). Shapes mirror what
// `make seed-demo` produces. Mock-backed for CI determinism (no live backend).

const BIZ_ID = 'biz-1';
const THREAD_TID = 'tid-pw';

const profile = {
  id: 'u1',
  email: 'owner@manyforge.test',
  display_name: 'Owner',
  email_verified: true,
  status: 'active',
};

function requester(email: string, name: string) {
  return {
    id: 'r-' + email,
    tenant_root_id: BIZ_ID,
    email,
    display_name: name,
    contact_id: null,
    first_seen_at: '2026-06-02T00:00:00Z',
    last_seen_at: '2026-06-02T00:00:00Z',
  };
}

function ticket(over: Record<string, unknown>) {
  return {
    id: 'tid',
    business_id: BIZ_ID,
    tenant_root_id: BIZ_ID,
    subject: 'Subject',
    status: 'new',
    priority: 'normal',
    assignee_principal_id: null,
    requester: requester('a@b.test', 'A B'),
    tags: [],
    message_count: 1,
    last_message_at: '2026-06-02T17:34:14Z',
    created_at: '2026-06-02T17:34:14Z',
    updated_at: '2026-06-02T17:34:14Z',
    ...over,
  };
}

// The three seeded conversations for one business.
const SEEDED_TICKETS = [
  ticket({ id: 'tid-feature', subject: 'Feature request: CSV export', requester: requester('priya@initech.test', 'Priya Nair'), message_count: 1 }),
  ticket({ id: 'tid-billing', subject: 'Double charged this month', requester: requester('marcus@globex.test', 'Marcus Reed'), message_count: 1 }),
  ticket({ id: THREAD_TID, subject: 'Cannot reset my password', requester: requester('jane@example.com', 'Jane Customer'), message_count: 2 }),
];

function msg(over: Record<string, unknown>) {
  return {
    id: 'm',
    direction: 'inbound',
    body_text: 'body',
    message_id: 'mid@x',
    in_reply_to: null,
    references: [],
    author_principal_id: null,
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: null,
    attachments: [],
    created_at: '2026-06-02T17:34:14Z',
    ...over,
  };
}

// The threaded password ticket: opener + customer reply + an agent outbound.
const THREAD_MESSAGES = [
  msg({ id: 'm-1', direction: 'inbound', body_text: "Hi, the reset link returns 'token expired'. Help?", message_id: 'seed-acme-pw-1@demo.manyforge.test' }),
  msg({ id: 'm-2', direction: 'inbound', body_text: 'Still stuck — I tried three times.', message_id: 'seed-acme-pw-2@demo.manyforge.test', in_reply_to: 'seed-acme-pw-1@demo.manyforge.test' }),
  msg({ id: 'm-3', direction: 'outbound', body_text: "Thanks Jane — I've reset your token.", message_id: 'out-1@manyforge', author_principal_id: 'u1' }),
];

async function install(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) =>
    r.fulfill({ json: { items: [{ id: BIZ_ID, parent_id: null, tenant_root_id: BIZ_ID, name: 'Acme Holdings', status: 'active' }], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/assignable-members`, (r) =>
    r.fulfill({ json: { items: [], next_cursor: null } }),
  );
  // Broad list route FIRST; the specific single-ticket + messages routes are
  // registered AFTER so they win for /tickets/{tid} (last-registered wins in
  // Playwright, and `tickets**` would otherwise also match the sub-paths).
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets**`, (r) =>
    r.fulfill({ json: { items: SEEDED_TICKETS, next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${THREAD_TID}/messages`, (r) =>
    r.fulfill({ json: { items: THREAD_MESSAGES, next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${THREAD_TID}`, (r) =>
    r.fulfill({ json: SEEDED_TICKETS[2] }),
  );
}

test('the support list renders every seeded ticket (not just one)', async ({ page }) => {
  await install(page);
  await page.goto('/support');

  const rows = page.getByTestId('ticket-row');
  await expect(rows).toHaveCount(3);
  await expect(page.getByTestId('ticket-subject')).toContainText([
    'Feature request: CSV export',
    'Double charged this month',
    'Cannot reset my password',
  ]);
  // The threaded ticket advertises its multi-message count.
  await expect(rows.filter({ hasText: 'Cannot reset my password' }).getByTestId('ticket-message-count')).toContainText('2');
});

test('the threaded ticket renders all messages in order (inbound, reply, outbound)', async ({ page }) => {
  await install(page);
  await page.goto(`/support/${BIZ_ID}/${THREAD_TID}`);

  await expect(page.getByTestId('thread-subject')).toHaveText('Cannot reset my password');
  const messages = page.getByTestId('message');
  await expect(messages).toHaveCount(3);
  await expect(messages.nth(0)).toContainText("token expired");
  await expect(messages.nth(1)).toContainText('Still stuck');
  await expect(messages.nth(2)).toContainText("I've reset your token");
  // The agent reply is an outbound message, the customer ones inbound.
  await expect(messages.nth(2)).toHaveAttribute('data-direction', 'outbound');
});
