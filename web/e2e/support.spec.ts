import { Page, expect, test } from '@playwright/test';

// US1 support-desk SPA regression (T034). The backend ingestion path (provider
// HMAC, recipient→business routing, SECURITY DEFINER ingest, idempotent
// threading, RLS) is covered by the Go integration/security tests. This spec
// pins the *SPA* render of an already-ingested message: the support list shows
// the ticket (subject + requester), and opening the row shows the thread with
// the inbound message body + requester. The /api/v1 surface is mocked via
// page.route so the flow is deterministic and needs no live backend (mirrors
// us2.spec.ts / foundation.spec.ts). US2 reply/note is a later task.

const BIZ_ID = 'biz-1';
const TICKET_ID = 'tkt-1';

interface MockBiz {
  id: string;
  parent_id: string | null;
  tenant_root_id: string;
  name: string;
  status: string;
  is_tenant_root: boolean;
}

const business: MockBiz = {
  id: BIZ_ID,
  parent_id: null,
  tenant_root_id: 'root-1',
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};

// One requester, embedded inline on the ticket (openapi.yaml Ticket schema).
const requester = {
  id: 'req-1',
  tenant_root_id: 'root-1',
  email: 'jane.customer@example.com',
  display_name: 'Jane Customer',
  contact_id: null,
  first_seen_at: '2026-05-30T10:00:00Z',
  last_seen_at: '2026-05-31T09:00:00Z',
};

// One inbound ticket produced by an ingested email.
const ticket = {
  id: TICKET_ID,
  business_id: BIZ_ID,
  tenant_root_id: 'root-1',
  subject: 'Cannot reset my password',
  status: 'new',
  priority: 'normal',
  assignee_principal_id: null,
  requester,
  tags: [],
  message_count: 1,
  last_message_at: '2026-05-31T09:00:00Z',
  created_at: '2026-05-31T09:00:00Z',
  updated_at: '2026-05-31T09:00:00Z',
};

// The single inbound message in that ticket's thread (direction inbound,
// a body, the SPF/DKIM/DMARC flags the thread view renders for inbound).
const inboundMessage = {
  id: 'msg-1',
  ticket_id: TICKET_ID,
  direction: 'inbound',
  message_id: '<abc123@example.com>',
  in_reply_to: null,
  references: [],
  author_principal_id: null,
  body_text: 'Hi, I tried the reset link but it never arrives. Please help!',
  body_html: null,
  attachments: [],
  spf_result: 'pass',
  dkim_result: 'pass',
  dmarc_result: 'pass',
  created_at: '2026-05-31T09:00:00Z',
};

// installStack seeds an authenticated session and mocks the /api/v1 reads the
// list + thread pages drive: /me, /businesses, the ticket list page, the single
// ticket, and the message thread page.
async function installStack(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' },
    }),
  );

  // Order matters: Playwright runs the LAST-registered matching handler first,
  // so register the broad /businesses** catch-all FIRST and the more specific
  // ticket/message routes AFTER, so the specific ones win.
  await page.route('**/api/v1/businesses**', (route) =>
    route.fulfill({ json: { items: [business], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets**`, (route) =>
    route.fulfill({ json: { items: [ticket], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}`, (route) =>
    route.fulfill({ json: ticket }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/messages**`, (route) =>
    route.fulfill({ json: { items: [inboundMessage], next_cursor: null } }),
  );
}

test('an ingested ticket renders in the support list with its subject and requester', async ({ page }) => {
  await installStack(page);
  await page.goto('/support');

  // The business auto-selects the first (only) business, scoping the list call.
  await expect(page.getByRole('heading', { name: 'Support' })).toBeVisible();
  await expect(page.getByTestId('business-select')).toHaveValue(BIZ_ID);

  const row = page.getByTestId('ticket-row');
  await expect(row).toHaveCount(1);
  await expect(row).toHaveAttribute('data-ticket-id', TICKET_ID);
  await expect(row.getByTestId('ticket-subject')).toHaveText('Cannot reset my password');
  await expect(row.getByTestId('ticket-requester')).toHaveText('Jane Customer');
  await expect(row.getByTestId('ticket-status')).toHaveText('new');
  await expect(row.getByTestId('ticket-message-count')).toContainText('1 msg');
});

test('opening the ticket shows the inbound message body and the requester in the thread', async ({ page }) => {
  await installStack(page);
  await page.goto('/support');

  // Click the ticket row → navigate to the thread view (/support/:businessId/:tid).
  await page.getByTestId('ticket-row').click();
  await expect(page).toHaveURL(new RegExp(`/support/${BIZ_ID}/${TICKET_ID}$`));

  // Thread header: subject + status + the embedded requester.
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('thread-subject')).toHaveText('Cannot reset my password');
  await expect(page.getByTestId('thread-status')).toHaveText('new');
  await expect(page.getByTestId('thread-requester')).toContainText('Jane Customer');
  await expect(page.getByTestId('thread-requester')).toContainText('jane.customer@example.com');

  // The ingested inbound message renders in the thread with its body + direction.
  const message = page.getByTestId('message');
  await expect(message).toHaveCount(1);
  await expect(message).toHaveAttribute('data-direction', 'inbound');
  await expect(message.getByTestId('message-direction')).toContainText('Received');
  await expect(message.getByTestId('message-body')).toHaveText(
    'Hi, I tried the reset link but it never arrives. Please help!',
  );

  // Inbound auth flags (SPF/DKIM/DMARC) are surfaced, flagged not rejected (FR-019).
  await expect(message.getByTestId('auth-flags')).toBeVisible();
  await expect(message.getByTestId('spf-result')).toContainText('SPF: pass');
  await expect(message.getByTestId('dkim-result')).toContainText('DKIM: pass');
  await expect(message.getByTestId('dmarc-result')).toContainText('DMARC: pass');
});
