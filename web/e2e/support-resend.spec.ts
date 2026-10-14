import { Page, expect, test } from '@playwright/test';

// Spec 017 UI regression: the Resend DNS checklist on a provider_route domain, the
// per-address "send replies as" override, and dropped attachments in the thread.
// The /api/v1 surface is mocked.
const BIZ_ID = 'biz-1';
const TICKET_ID = 'tkt-1';

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
});

test('a custom address saves, rejects, and clears its send-as address', async ({ page }) => {
  await installBase(page);
  const custom = {
    id: 'ia-custom',
    business_id: BIZ_ID,
    tenant_root_id: 'root-1',
    address: 'support@support.garden.example',
    kind: 'custom',
    email_domain_id: 'ed-resend',
    send_as: null as string | null,
    active: true,
    created_at: '2026-10-10T00:00:00Z',
  };
  const system = { ...custom, id: 'ia-system', address: 'b-1@inbound.localhost', kind: 'system', email_domain_id: null };
  const patches: unknown[] = [];
  await page.route(`**/api/v1/businesses/${BIZ_ID}/email-domains`, (route) =>
    route.fulfill({ json: { items: [{ ...resendDomain, verification: 'verified' }], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/inbound-addresses`, (route) =>
    route.fulfill({ json: { items: [system, custom], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/inbound-addresses/${custom.id}`, async (route) => {
    const body = route.request().postDataJSON() as { send_as: string | null };
    patches.push({ method: route.request().method(), body });
    if (body.send_as && !body.send_as.endsWith('@garden.example')) {
      return route.fulfill({
        status: 400,
        json: { code: 'VALIDATION', message: 'send_as must be on the domain of the verified Resend sending profile' },
      });
    }
    custom.send_as = body.send_as;
    return route.fulfill({ json: custom });
  });

  await page.goto(`/support/${BIZ_ID}/settings/inbox`);
  const rows = page.getByTestId('address-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0).getByTestId('send-as')).toHaveCount(0);
  const row = rows.nth(1);
  const input = row.getByTestId('send-as-input');
  await expect(input).toHaveValue('');

  await input.fill('help@elsewhere.example');
  await row.getByTestId('send-as-save').click();
  await expect(row.getByTestId('send-as-error')).toContainText('verified Resend sending profile');
  await expect(row.getByTestId('send-as-current')).toHaveCount(0);

  await input.fill('support@garden.example');
  await row.getByTestId('send-as-save').click();
  await expect(row.getByTestId('send-as-current')).toHaveText('Replies from support@garden.example');
  await expect(row.getByTestId('send-as-error')).toHaveCount(0);
  await expect(input).toHaveValue('support@garden.example');

  await input.fill('');
  await row.getByTestId('send-as-save').click();
  await expect(row.getByTestId('send-as-current')).toHaveCount(0);
  expect(patches).toEqual([
    { method: 'PATCH', body: { send_as: 'help@elsewhere.example' } },
    { method: 'PATCH', body: { send_as: 'support@garden.example' } },
    { method: 'PATCH', body: { send_as: null } },
  ]);
});

test('dropped attachments are listed with why they were not stored, and DMARC none reads as no policy', async ({ page }) => {
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
    dropped_attachments: [
      { filename: 'invoice.exe', detected_type: 'application/octet-stream', size: 24, reason: 'disallowed_type' },
      { filename: 'photo-51.png', detected_type: 'image/png', size: 2048, reason: 'limit_exceeded' },
    ],
    spf_result: 'pass',
    dkim_result: 'pass',
    dmarc_result: 'none',
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
  await expect(dropped).toHaveCount(2);
  await expect(dropped.nth(0)).toContainText('invoice.exe');
  await expect(dropped.nth(0)).toContainText('not stored: file type not allowed');
  await expect(dropped.nth(1)).toContainText('photo-51.png');
  await expect(dropped.nth(1)).toContainText('not stored: attachment limit reached');

  // manyforge-clk7: a sender with no DMARC record shows "no policy", not "unknown".
  const dmarc = page.getByTestId('dmarc-result');
  await expect(dmarc).toHaveText(/DMARC: no policy/i);
  await expect(dmarc).toHaveClass(/\bnone\b/);
  await expect(dmarc).toHaveAttribute('title', /no DMARC policy/);
});
