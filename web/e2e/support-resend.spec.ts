import { Page, expect, test } from '@playwright/test';

// Spec 017 UI regression: the Resend DNS checklist on a provider_route domain and
// dropped attachments in the thread. The /api/v1 surface is mocked.
const BIZ_ID = 'biz-1';
const TICKET_ID = 'tkt-1';
// Screenshots for the task report land in the repo root; they are never committed.
const SHOT_DIR = '..';

const business = {
  id: BIZ_ID,
  parent_id: null,
  tenant_root_id: 'root-1',
  name: 'Garden',
  status: 'active',
  is_tenant_root: true,
};

const resendDomain = {
  id: 'ed-resend',
  business_id: BIZ_ID,
  tenant_root_id: 'root-1',
  domain: 'support.garden.example',
  mode: 'provider_route',
  verification: 'unverified',
  verified_at: null,
  dkim_state: 'unknown',
  spf_state: 'unknown',
  dns_challenge: {
    verification_txt: { name: '_manyforge.support.garden.example', value: 'mf-verify=TESTTOKEN' },
    dkim_record: { name: 'mf1._domainkey.support.garden.example', value: 'v=DKIM1; k=ed25519; p=AAAA' },
    spf_hint: 'v=spf1 include:mx.manyforge.test ~all',
    mx_hint: null,
  },
  created_at: '2026-10-10T00:00:00Z',
};

async function installBase(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  // Registered FIRST so it is the lowest-priority match (Playwright runs the
  // last-registered matching handler first): unmocked shell nav-badge calls
  // (approvals, connectors, ...) get an empty 200 instead of a 401 that would
  // trigger refresh -> /login mid-test.
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
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
}

test('provider_route domain shows the Resend DNS checklist and the Received via Resend label', async ({
  page,
}) => {
  await installBase(page);
  await page.route(`**/api/v1/businesses/${BIZ_ID}/email-domains`, (route) =>
    route.fulfill({ json: { items: [resendDomain], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/inbound-addresses`, (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );

  await page.goto(`/support/${BIZ_ID}/settings/inbox`);
  const row = page.getByTestId('domain-row');
  await expect(row.getByTestId('received-via-resend')).toHaveText('Received via Resend');
  const checklist = row.getByTestId('resend-dns-checklist');
  await expect(checklist).toBeVisible();
  await expect(checklist.getByTestId('resend-dns-row')).toHaveCount(5);
  await expect(checklist).toContainText('_manyforge.support.garden.example');
  await expect(checklist).toContainText('mf-verify=TESTTOKEN');
  await expect(checklist).toContainText('resend._domainkey.support.garden.example');
  await expect(checklist).toContainText('send.support.garden.example');
  await expect(row.getByTestId('challenge-dkim-name')).toHaveCount(0);
  await expect(row.getByTestId('verify-domain')).toHaveCount(1);
  await expect(row.getByTestId('verify-domain')).toBeVisible();
  await page.screenshot({ path: `${SHOT_DIR}/support-resend-dns-checklist.png`, fullPage: true });
});

test('a dropped attachment is listed on the inbound message as not stored', async ({ page }) => {
  await installBase(page);
  const ticket = {
    id: TICKET_ID,
    business_id: BIZ_ID,
    tenant_root_id: 'root-1',
    subject: 'Invoice',
    status: 'new',
    priority: 'normal',
    assignee_principal_id: null,
    requester: {
      id: 'req-1',
      tenant_root_id: 'root-1',
      email: 'ada@customer.test',
      display_name: 'Ada',
      contact_id: null,
      first_seen_at: '2026-10-10T00:00:00Z',
      last_seen_at: '2026-10-10T00:00:00Z',
    },
    tags: [],
    message_count: 1,
    last_message_at: '2026-10-10T00:00:00Z',
    created_at: '2026-10-10T00:00:00Z',
    updated_at: '2026-10-10T00:00:00Z',
  };
  const message = {
    id: 'msg-1',
    ticket_id: TICKET_ID,
    direction: 'inbound',
    message_id: '<m1@customer.test>',
    in_reply_to: null,
    references: [],
    author_principal_id: null,
    body_text: 'invoice attached',
    body_html: null,
    attachments: [],
    dropped_attachments: [{ filename: 'invoice.exe', detected_type: 'application/octet-stream', size: 24 }],
    spf_result: 'pass',
    dkim_result: 'pass',
    dmarc_result: 'pass',
    delivery_state: null,
    created_at: '2026-10-10T00:00:00Z',
  };
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}`, (route) =>
    route.fulfill({ json: ticket }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/messages**`, (route) =>
    route.fulfill({ json: { items: [message], next_cursor: null } }),
  );

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  const dropped = page.getByTestId('dropped-attachment');
  await expect(dropped).toHaveCount(1);
  await expect(dropped).toContainText('invoice.exe');
  await expect(dropped).toContainText('not stored');
  await page.screenshot({ path: `${SHOT_DIR}/support-dropped-attachments.png`, fullPage: true });
});
