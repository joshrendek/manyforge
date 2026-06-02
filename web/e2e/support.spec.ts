import { Page, expect, test } from '@playwright/test';

// US1 support-desk SPA regression (T034). The backend ingestion path (provider
// HMAC, recipient→business routing, SECURITY DEFINER ingest, idempotent
// threading, RLS) is covered by the Go integration/security tests. This spec
// pins the *SPA* render of an already-ingested message: the support list shows
// the ticket (subject + requester), and opening the row shows the thread with
// the inbound message body + requester. The /api/v1 surface is mocked via
// page.route so the flow is deterministic and needs no live backend (mirrors
// us2.spec.ts / foundation.spec.ts).
//
// US2 reply/note composer tests (T043) follow in the second describe block.

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

// ── US2 composer tests (T043) ─────────────────────────────────────────────────
//
// installThreadStack seeds auth + all routes needed to land on the thread view
// directly, including mutable POST overrides passed per-test. Caller registers
// the reply/note POST mocks AFTER calling this helper (last-registered wins in
// Playwright), so per-test POST mocks override the catch-all.
async function installThreadStack(page: Page, initialMessages: object[] = [inboundMessage]) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: {
        id: 'u1',
        email: 'owner@manyforge.test',
        display_name: 'Owner',
        email_verified: true,
        status: 'active',
      },
    }),
  );
  await page.route('**/api/v1/businesses**', (route) =>
    route.fulfill({ json: { items: [business], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}`, (route) =>
    route.fulfill({ json: ticket }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/messages**`, (route) =>
    route.fulfill({ json: { items: initialMessages, next_cursor: null } }),
  );
}

// (a) Reply renders outbound in the thread after submit.
test('US2: reply submit POSTs to /reply and renders an outbound message', async ({ page }) => {
  await installThreadStack(page);

  const replyMessage = {
    id: 'msg-reply-1',
    ticket_id: TICKET_ID,
    direction: 'outbound',
    message_id: '<reply-1@manyforge.test>',
    in_reply_to: '<abc123@example.com>',
    references: ['<abc123@example.com>'],
    author_principal_id: 'u1',
    body_text: 'Thanks for reaching out! We will look into this right away.',
    body_html: null,
    attachments: [],
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: 'pending',
    created_at: '2026-05-31T10:00:00Z',
  };

  let replyEndpointHit = false;
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/reply`, (route) => {
    replyEndpointHit = true;
    route.fulfill({ status: 201, json: replyMessage });
  });

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();

  // Default mode is reply — verify the toggle shows Reply active.
  await expect(page.getByTestId('toggle-reply')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByTestId('toggle-note')).toHaveAttribute('aria-pressed', 'false');

  // Type a reply and submit.
  await page.getByTestId('composer-body').fill('Thanks for reaching out! We will look into this right away.');
  await page.getByTestId('composer-submit').click();

  // The new outbound message must appear in the thread.
  const outbound = page.locator('[data-testid="message"][data-direction="outbound"]');
  await expect(outbound).toBeVisible();
  await expect(outbound.getByTestId('message-body')).toHaveText(
    'Thanks for reaching out! We will look into this right away.',
  );
  await expect(outbound.getByTestId('message-direction')).toContainText('Reply');

  // Assert the correct endpoint was called.
  expect(replyEndpointHit).toBe(true);

  // Composer clears after send.
  await expect(page.getByTestId('composer-body')).toHaveValue('');
});

// (b) Note renders distinct (direction="note", "Internal note" label).
test('US2: note submit POSTs to /note and renders a note-direction message', async ({ page }) => {
  await installThreadStack(page);

  const noteMessage = {
    id: 'msg-note-1',
    ticket_id: TICKET_ID,
    direction: 'note',
    message_id: null,
    in_reply_to: null,
    references: [],
    author_principal_id: 'u1',
    body_text: 'Internal: escalate to tier-2.',
    body_html: null,
    attachments: [],
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: null,
    created_at: '2026-05-31T11:00:00Z',
  };

  let noteEndpointHit = false;
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/note`, (route) => {
    noteEndpointHit = true;
    route.fulfill({ status: 201, json: noteMessage });
  });

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();

  // Switch to note mode.
  await page.getByTestId('toggle-note').click();
  await expect(page.getByTestId('toggle-note')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByTestId('toggle-reply')).toHaveAttribute('aria-pressed', 'false');

  // Placeholder text changes to reflect note mode.
  await expect(page.getByTestId('composer-body')).toHaveAttribute('placeholder', 'Add an internal note…');

  await page.getByTestId('composer-body').fill('Internal: escalate to tier-2.');
  await page.getByTestId('composer-submit').click();

  // The note message must render with direction="note".
  const note = page.locator('[data-testid="message"][data-direction="note"]');
  await expect(note).toBeVisible();
  await expect(note.getByTestId('message-body')).toHaveText('Internal: escalate to tier-2.');
  await expect(note.getByTestId('message-direction')).toContainText('Internal note');

  // Notes are structurally distinct: no auth-flags block (inbound-only).
  await expect(note.getByTestId('auth-flags')).not.toBeVisible();

  // Assert the correct endpoint was called.
  expect(noteEndpointHit).toBe(true);
});

// (c) Delivery-failed badge is visible on a message with delivery_state="failed".
test('US2: failed delivery_state renders the delivery-failed badge', async ({ page }) => {
  const failedMessage = {
    id: 'msg-failed-1',
    ticket_id: TICKET_ID,
    direction: 'outbound',
    message_id: '<failed-1@manyforge.test>',
    in_reply_to: null,
    references: [],
    author_principal_id: 'u1',
    body_text: 'This reply could not be delivered.',
    body_html: null,
    attachments: [],
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: 'failed',
    created_at: '2026-05-31T09:30:00Z',
  };

  // Seed the thread with both the inbound message and the failed outbound.
  await installThreadStack(page, [inboundMessage, failedMessage]);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();

  // The failed outbound message must render the delivery-failed badge.
  const failedMsg = page.locator('[data-testid="message"][data-direction="outbound"]');
  await expect(failedMsg).toBeVisible();
  await expect(failedMsg.getByTestId('delivery-failed')).toBeVisible();
  await expect(failedMsg.getByTestId('delivery-failed')).toContainText('Failed to send');

  // The inbound message must NOT show a delivery-failed badge.
  const inbound = page.locator('[data-testid="message"][data-direction="inbound"]');
  await expect(inbound).toBeVisible();
  await expect(inbound.getByTestId('delivery-failed')).not.toBeVisible();
});

// (d) Toggle routing: reply hits /reply; note hits /note (not the other endpoint).
test('US2: toggle correctly routes to /reply vs /note endpoint', async ({ page }) => {
  await installThreadStack(page);

  const outboundMsg = {
    id: 'msg-out-2',
    ticket_id: TICKET_ID,
    direction: 'outbound',
    message_id: null,
    in_reply_to: null,
    references: [],
    author_principal_id: 'u1',
    body_text: 'Reply text',
    body_html: null,
    attachments: [],
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: 'sent',
    created_at: '2026-05-31T12:00:00Z',
  };
  const noteMsg = {
    id: 'msg-note-2',
    ticket_id: TICKET_ID,
    direction: 'note',
    message_id: null,
    in_reply_to: null,
    references: [],
    author_principal_id: 'u1',
    body_text: 'Note text',
    body_html: null,
    attachments: [],
    spf_result: 'unknown',
    dkim_result: 'unknown',
    dmarc_result: 'unknown',
    delivery_state: null,
    created_at: '2026-05-31T12:01:00Z',
  };

  let replyHits = 0;
  let noteHits = 0;
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/reply`, (route) => {
    replyHits++;
    route.fulfill({ status: 201, json: outboundMsg });
  });
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/note`, (route) => {
    noteHits++;
    route.fulfill({ status: 201, json: noteMsg });
  });

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();

  // 1. Send a reply in default (reply) mode.
  await page.getByTestId('composer-body').fill('Reply text');
  await page.getByTestId('composer-submit').click();
  await expect(page.locator('[data-testid="message"][data-direction="outbound"]')).toBeVisible();

  // 2. Switch to note mode and send a note.
  await page.getByTestId('toggle-note').click();
  await page.getByTestId('composer-body').fill('Note text');
  await page.getByTestId('composer-submit').click();
  await expect(page.locator('[data-testid="message"][data-direction="note"]')).toBeVisible();

  // Each endpoint hit exactly once; no cross-routing.
  expect(replyHits).toBe(1);
  expect(noteHits).toBe(1);
});

// (e) Submit button is disabled when the composer textarea is blank.
test('US2: submit is disabled when composer body is empty', async ({ page }) => {
  await installThreadStack(page);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();

  // Initially empty — submit must be disabled.
  await expect(page.getByTestId('composer-body')).toHaveValue('');
  await expect(page.getByTestId('composer-submit')).toBeDisabled();

  // Type something — submit becomes enabled.
  await page.getByTestId('composer-body').fill('Hello');
  await expect(page.getByTestId('composer-submit')).toBeEnabled();

  // Clear it back to empty — disabled again.
  await page.getByTestId('composer-body').fill('');
  await expect(page.getByTestId('composer-submit')).toBeDisabled();
});

// ── US3 triage tests (T051) ───────────────────────────────────────────────────
//
// installTriageStack seeds auth + the read mocks AND a *stateful* ticket mock:
// a mutable `currentTicket` object backs the GET /tickets/{tid} handler, and the
// PATCH /tickets/{tid} handler merges the request body into it (then returns it,
// mirroring the real PATCH that returns the updated Ticket). Because GET reads
// the same object, page.reload() re-fetches the mutated ticket — proving the
// change "persisted" exactly as a real backend would. The PATCH body the route
// observed is captured per-test so we can assert the exact wire payload.
//
// Both GET and PATCH match the SAME url glob (`**/tickets/tkt-1`); the handler
// branches on route.request().method(). Returns the captured-body getter so the
// test can assert the last PATCH payload after a mutation settles.
function installTriageStack(page: Page) {
  // A deep-enough copy so mutations here don't bleed into the shared fixture.
  const currentTicket: Record<string, unknown> = JSON.parse(JSON.stringify(ticket));
  const state = { lastPatchBody: null as Record<string, unknown> | null };

  return (async () => {
    await page.addInitScript(() => {
      localStorage.setItem('mf_access', 'test-access');
      localStorage.setItem('mf_refresh', 'test-refresh');
    });
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        json: {
          id: 'u1',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      }),
    );
    await page.route('**/api/v1/businesses**', (route) =>
      route.fulfill({ json: { items: [business], next_cursor: null } }),
    );
    await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/messages**`, (route) =>
      route.fulfill({ json: { items: [inboundMessage], next_cursor: null } }),
    );
    // Stateful ticket handler: GET returns currentTicket; PATCH merges the body
    // into currentTicket and returns it (200) — the same updated-Ticket contract
    // the component reflects after each triage mutation.
    await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}`, (route) => {
      const req = route.request();
      if (req.method() === 'PATCH') {
        const body = (req.postDataJSON() ?? {}) as Record<string, unknown>;
        state.lastPatchBody = body;
        Object.assign(currentTicket, body);
        route.fulfill({ status: 200, json: currentTicket });
        return;
      }
      route.fulfill({ json: currentTicket });
    });
    return state;
  })();
}

// 1. Status change persists on reload.
test('US3: changing status PATCHes {status} and persists across reload', async ({ page }) => {
  const state = await installTriageStack(page);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();
  // Seeded status is "new" (see fixture).
  await expect(page.getByTestId('thread-status')).toHaveText('new');
  await expect(page.getByTestId('triage-status')).toHaveValue('new');

  // Select a new status → drives changeStatus($event) → PATCH {status:'pending'}.
  await page.getByTestId('triage-status').selectOption('pending');

  // The header reflects the returned Ticket immediately (no stale UI).
  await expect(page.getByTestId('thread-status')).toHaveText('pending');
  expect(state.lastPatchBody).toEqual({ status: 'pending' });

  // Reload re-fetches the *mutated* ticket from the stateful GET → still pending.
  await page.reload();
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('thread-status')).toHaveText('pending');
  await expect(page.getByTestId('triage-status')).toHaveValue('pending');
});

// 2. Priority change persists on reload.
test('US3: changing priority PATCHes {priority} and persists across reload', async ({ page }) => {
  const state = await installTriageStack(page);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();
  // Seeded priority is "normal".
  await expect(page.getByTestId('thread-priority')).toHaveText('normal');
  await expect(page.getByTestId('triage-priority')).toHaveValue('normal');

  await page.getByTestId('triage-priority').selectOption('high');

  await expect(page.getByTestId('thread-priority')).toHaveText('high');
  expect(state.lastPatchBody).toEqual({ priority: 'high' });

  await page.reload();
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('thread-priority')).toHaveText('high');
  await expect(page.getByTestId('triage-priority')).toHaveValue('high');
});

// 3. Assign to me shows the assignee; reload keeps it; unassign reverses it.
test('US3: assign-to-me PATCHes the assignee id, shows it, and persists; unassign reverses', async ({
  page,
}) => {
  const state = await installTriageStack(page);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();
  // Seeded unassigned → the "unassigned" badge is shown.
  await expect(page.getByTestId('thread-unassigned')).toBeVisible();

  // Assign to me → PATCH {assignee_principal_id:'u1'} (the /me principal id).
  await page.getByTestId('assign-to-me').click();
  await expect(page.getByTestId('thread-unassigned')).toHaveCount(0);
  expect(state.lastPatchBody).toEqual({ assignee_principal_id: 'u1' });

  // Reload re-fetches the mutated ticket → still assigned (no unassigned badge).
  await page.reload();
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('thread-unassigned')).toHaveCount(0);
  // Assign-to-me is now disabled (already assigned to me); unassign is enabled.
  await expect(page.getByTestId('assign-to-me')).toBeDisabled();
  await expect(page.getByTestId('unassign')).toBeEnabled();

  // Unassign → PATCH {assignee_principal_id:null} → the unassigned badge returns.
  await page.getByTestId('unassign').click();
  await expect(page.getByTestId('thread-unassigned')).toBeVisible();
  expect(state.lastPatchBody).toEqual({ assignee_principal_id: null });

  // Persisted: reload still shows unassigned.
  await page.reload();
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('thread-unassigned')).toBeVisible();
});

// 4. Tag add PATCHes the full tag set and renders a new chip; persists on reload.
test('US3: adding a tag PATCHes the full tag set, renders a chip, and persists', async ({
  page,
}) => {
  const state = await installTriageStack(page);

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();
  // Seeded with no tags.
  await expect(page.getByTestId('triage-chip')).toHaveCount(0);

  // Type a tag + Enter → addTag() sends the FULL resulting set (replacement).
  await page.getByTestId('triage-tag-input').fill('billing');
  await page.getByTestId('triage-tag-input').press('Enter');

  // A chip renders for the new tag (triage controls) and the header badge too.
  await expect(page.getByTestId('triage-chip')).toHaveCount(1);
  await expect(page.getByTestId('triage-chip')).toContainText('billing');
  await expect(page.getByTestId('thread-tag')).toHaveText('billing');
  expect(state.lastPatchBody).toEqual({ tags: ['billing'] });

  // Reload re-fetches the mutated ticket → the tag survives.
  await page.reload();
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await expect(page.getByTestId('triage-chip')).toHaveCount(1);
  await expect(page.getByTestId('triage-chip')).toContainText('billing');
});

// ── US4 inbox-settings tests (T061) ───────────────────────────────────────────
//
// The inbox-settings page (/support/:businessId/settings/inbox) manages a
// business's custom email domains and inbound addresses (FR-012/FR-013). The
// backend (domain CRUD, DNS-challenge generation, DEFINER verify, RLS) is covered
// by the Go integration/security tests; this spec pins the *SPA* flow against a
// mocked /api/v1 surface: add a forward_in domain → the DNS challenge renders →
// (stub-verified) the domain flips to verified → a custom inbound address can then
// be added on it and appears in the addresses list.
//
// installInboxStack is the US4 analogue of installTriageStack: a *stateful* mock
// where local `domains`/`addresses` arrays back the GET handlers, and the POST
// handlers (add-domain, add-address) plus the verify handler mutate those arrays
// so a subsequent GET — or the in-place signal update the component does —
// reflects the new state exactly as a real backend would. The email-domains GET
// and POST share one url glob, as do the inbound-addresses GET and POST, so each
// handler branches on route.request().method() (mirrors the US3 PATCH handler).
// Returns getters for the last add-domain / add-address POST bodies so the test
// can assert the wire payloads.
const ACME_DOMAIN = 'acme.example';
const ACME_DOMAIN_ID = 'dom-acme-1';

function installInboxStack(page: Page) {
  // System inbound address always present; the custom one is added post-verify.
  const systemAddress = {
    id: 'addr-sys-1',
    business_id: BIZ_ID,
    tenant_root_id: 'root-1',
    address: 'biz-1@inbound.manyforge.test',
    kind: 'system',
    email_domain_id: null,
    active: true,
    created_at: '2026-05-01T00:00:00Z',
  };
  const customAddress = {
    id: 'addr-custom-1',
    business_id: BIZ_ID,
    tenant_root_id: 'root-1',
    address: 'support@acme.example',
    kind: 'custom',
    email_domain_id: ACME_DOMAIN_ID,
    active: true,
    created_at: '2026-06-01T00:00:00Z',
  };

  // The forward_in domain as returned by POST: unverified, with a FULL DNS
  // challenge (ownership TXT + DKIM TXT + SPF hint; no MX for forward_in).
  const unverifiedDomain = {
    id: ACME_DOMAIN_ID,
    business_id: BIZ_ID,
    tenant_root_id: 'root-1',
    domain: ACME_DOMAIN,
    mode: 'forward_in',
    verification: 'unverified',
    verified_at: null,
    dkim_state: 'pending',
    spf_state: 'unknown',
    dns_challenge: {
      verification_txt: {
        name: '_manyforge.acme.example',
        value: 'mf-verify=TESTTOKEN',
      },
      dkim_record: {
        name: 'mf1._domainkey.acme.example',
        value: 'v=DKIM1; k=ed25519; p=AAAA',
      },
      spf_hint: 'v=spf1 include:mx.manyforge.test ~all',
      mx_hint: null,
    },
    created_at: '2026-06-01T00:00:00Z',
  };
  // Same domain after a successful verify: verified, verified_at set, DKIM pass.
  const verifiedDomain = {
    ...unverifiedDomain,
    verification: 'verified',
    verified_at: '2026-06-01T00:05:00Z',
    dkim_state: 'pass',
    spf_state: 'pass',
  };

  // Mutable state the GET handlers read from.
  const domains: Record<string, unknown>[] = [];
  const addresses: Record<string, unknown>[] = [systemAddress];
  const state = {
    lastCreateBody: null as Record<string, unknown> | null,
    lastAddressBody: null as Record<string, unknown> | null,
  };

  return (async () => {
    await page.addInitScript(() => {
      localStorage.setItem('mf_access', 'test-access');
      localStorage.setItem('mf_refresh', 'test-refresh');
    });
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        json: {
          id: 'u1',
          email: 'owner@manyforge.test',
          display_name: 'Owner',
          email_verified: true,
          status: 'active',
        },
      }),
    );
    await page.route('**/api/v1/businesses**', (route) =>
      route.fulfill({ json: { items: [business], next_cursor: null } }),
    );

    // Verify endpoint (distinct path: .../email-domains/{id}/verify). Flip the
    // stored domain to verified — the component reflects the returned verified
    // EmailDomain in place. Verifying does NOT itself create an address; the
    // operator adds the custom address next (now that the domain is verified the
    // add-address <select> offers it).
    await page.route(
      `**/api/v1/businesses/${BIZ_ID}/email-domains/*/verify`,
      (route) => {
        const idx = domains.findIndex((d) => d['id'] === ACME_DOMAIN_ID);
        if (idx >= 0) domains[idx] = { ...verifiedDomain };
        route.fulfill({ status: 200, json: verifiedDomain });
      },
    );

    // email-domains list (GET) + create (POST) share this glob → branch on method.
    // POST appends the unverified domain to the stored list and returns it (201);
    // GET returns the current list. Registered AFTER the /verify route but the
    // /verify path is more specific (extra segments) so this never shadows it.
    await page.route(`**/api/v1/businesses/${BIZ_ID}/email-domains`, (route) => {
      const req = route.request();
      if (req.method() === 'POST') {
        state.lastCreateBody = (req.postDataJSON() ?? {}) as Record<string, unknown>;
        if (!domains.some((d) => d['id'] === ACME_DOMAIN_ID)) {
          domains.push({ ...unverifiedDomain });
        }
        route.fulfill({ status: 201, json: unverifiedDomain });
        return;
      }
      route.fulfill({ json: { items: domains, next_cursor: null } });
    });

    // inbound-addresses list (GET) + create (POST) share this glob → branch on
    // method. GET returns the current list (starts with the system address);
    // POST appends the custom address bound to the verified domain and returns
    // it (201), exactly the created InboundAddress the component prepends.
    await page.route(`**/api/v1/businesses/${BIZ_ID}/inbound-addresses`, (route) => {
      const req = route.request();
      if (req.method() === 'POST') {
        state.lastAddressBody = (req.postDataJSON() ?? {}) as Record<string, unknown>;
        if (!addresses.some((a) => a['id'] === customAddress.id)) {
          addresses.push({ ...customAddress });
        }
        route.fulfill({ status: 201, json: customAddress });
        return;
      }
      route.fulfill({ json: { items: addresses, next_cursor: null } });
    });
    return state;
  })();
}

// Add a forward_in domain → challenge shown → (stub) verify → domain verified
// and its custom inbound address listed. This is the T061 acceptance flow.
test('US4: add forward_in domain → DNS challenge shown → verify → domain verified + address listed', async ({
  page,
}) => {
  const state = await installInboxStack(page);

  await page.goto(`/support/${BIZ_ID}/settings/inbox`);

  // The page renders: heading + add-domain form + (empty) domain list + the
  // system inbound address.
  await expect(page.getByRole('heading', { name: 'Inbox settings' })).toBeVisible();
  await expect(page.getByTestId('add-domain-form')).toBeVisible();
  await expect(page.getByTestId('email-domain-list')).toBeVisible();
  await expect(page.getByTestId('domain-row')).toHaveCount(0);
  await expect(page.getByTestId('domain-empty')).toBeVisible();
  // The system inbound address is present from the start.
  await expect(page.getByTestId('address-row')).toHaveCount(1);
  await expect(page.getByTestId('address-value')).toHaveText('biz-1@inbound.manyforge.test');

  // Fill the add-domain form: domain + forward_in mode, then submit.
  await page.getByTestId('domain-input').fill(ACME_DOMAIN);
  await page.getByTestId('mode-select').selectOption('forward_in');
  await page.getByTestId('add-domain-submit').click();

  // The new domain row appears, unverified, with its mode + DKIM/SPF state.
  const domainRow = page.getByTestId('domain-row');
  await expect(domainRow).toHaveCount(1);
  await expect(domainRow).toHaveAttribute('data-domain-id', ACME_DOMAIN_ID);
  await expect(domainRow.getByTestId('domain-name')).toHaveText(ACME_DOMAIN);
  await expect(domainRow.getByTestId('domain-mode')).toHaveText('forward_in');
  await expect(domainRow.getByTestId('domain-status')).toHaveText('unverified');
  await expect(domainRow.getByTestId('dkim-state')).toContainText('pending');
  await expect(domainRow.getByTestId('spf-state')).toContainText('unknown');
  // The POST wire payload carried the domain + mode.
  expect(state.lastCreateBody).toEqual({ domain: ACME_DOMAIN, mode: 'forward_in' });

  // The DNS challenge for an unverified domain renders all records to publish.
  const challenge = domainRow.getByTestId('dns-challenge');
  await expect(challenge).toBeVisible();
  await expect(challenge.getByTestId('challenge-txt-name')).toHaveText('_manyforge.acme.example');
  await expect(challenge.getByTestId('challenge-txt-value')).toHaveText('mf-verify=TESTTOKEN');
  await expect(challenge.getByTestId('challenge-dkim-name')).toHaveText(
    'mf1._domainkey.acme.example',
  );
  await expect(challenge.getByTestId('challenge-dkim-value')).toHaveText(
    'v=DKIM1; k=ed25519; p=AAAA',
  );

  // Visual artifact for the controller: the rendered page with the challenge shown.
  await page.screenshot({ path: 'e2e/.artifacts/us4-inbox-settings.png', fullPage: true });

  // Click Verify → the stub returns the verified domain → status flips to verified
  // and the challenge panel (unverified-only) disappears.
  await domainRow.getByTestId('verify-domain').click();
  await expect(domainRow.getByTestId('domain-status')).toHaveText('verified');
  await expect(domainRow.getByTestId('dkim-state')).toContainText('pass');
  await expect(domainRow.getByTestId('dns-challenge')).toHaveCount(0);

  // Now that the domain is verified, the add-address <select> offers it (the
  // form only lists verified domains). Add a custom address on that domain.
  await page.getByTestId('address-input').fill('support@acme.example');
  await page.getByTestId('address-domain-select').selectOption(ACME_DOMAIN_ID);
  await page.getByTestId('add-address-submit').click();

  // The custom inbound address on the verified domain is now listed alongside
  // the system address. The wire payload bound it to the verified domain.
  await expect(page.getByTestId('address-row')).toHaveCount(2);
  expect(state.lastAddressBody).toEqual({
    address: 'support@acme.example',
    email_domain_id: ACME_DOMAIN_ID,
  });
  await expect(page.getByTestId('inbound-address-list')).toContainText('support@acme.example');
  const customRow = page.locator('[data-testid="address-row"][data-address-id="addr-custom-1"]');
  await expect(customRow.getByTestId('address-value')).toHaveText('support@acme.example');
  await expect(customRow.getByTestId('address-kind')).toHaveText('custom');

  // Reload re-fetches the stateful GETs → the verified domain + custom address
  // persist exactly as a real backend would return them.
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Inbox settings' })).toBeVisible();
  await expect(page.getByTestId('domain-row').getByTestId('domain-status')).toHaveText('verified');
  await expect(page.getByTestId('address-row')).toHaveCount(2);
  await expect(page.getByTestId('inbound-address-list')).toContainText('support@acme.example');
});
