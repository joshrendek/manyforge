import { expect, test } from '@playwright/test';

const ACCOUNT_ID = '11111111-1111-4111-8111-111111111111';
const BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const PROFILE_ID = '33333333-3333-4333-8333-333333333333';
const LIST_ID = '44444444-4444-4444-8444-444444444444';
const CAMPAIGN_ID = '55555555-5555-4555-8555-555555555555';
const SUBSCRIBER_ID = '66666666-6666-4666-8666-666666666666';
const SUPPRESSION_ID = '77777777-7777-4777-8777-777777777777';
const CREATED_SUPPRESSION_ID = '88888888-8888-4888-8888-888888888888';
const DELIVERY_ID = '99999999-9999-4999-8999-999999999999';

const profile = {
  id: PROFILE_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  mode: 'resend',
  from_email: 'news@acme.test',
  from_name: 'Acme News',
  reply_to: null,
  postal_address: '1 Main Street',
  email_domain_id: null,
  ses_region: null,
  ses_configuration_set: null,
  sns_topic_arn: null,
  status: 'verified',
  last_verified_at: '2026-08-30T12:00:00Z',
  verify_error: null,
  feedback_status: 'ready',
  feedback_error: null,
  feedback_confirmed_at: '2026-08-30T12:00:00Z',
  has_credentials: true,
  created_at: '2026-08-30T12:00:00Z',
  updated_at: '2026-08-30T12:00:00Z',
};
const account = {
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
  slug: 'product-news',
  name: 'Product news',
  description: null,
  double_opt_in: true,
  status: 'active',
  created_at: '2026-08-30T12:00:00Z',
  updated_at: '2026-08-30T12:00:00Z',
};

function draftCampaign() {
  return {
    id: CAMPAIGN_ID,
    business_id: BUSINESS_ID,
    tenant_root_id: BUSINESS_ID,
    list_id: LIST_ID,
    profile_id: PROFILE_ID,
    name: 'September update',
    subject: '',
    preheader: null,
    body_markdown: '',
    tag_filter: [],
    track_opens: true,
    track_clicks: true,
    status: 'draft',
    scheduled_at: null,
    started_at: null,
    completed_at: null,
    recipient_count: 0,
    sent_count: 0,
    delivered_count: 0,
    bounced_count: 0,
    complained_count: 0,
    opened_count: 0,
    clicked_count: 0,
    unsubscribed_count: 0,
    failed_count: 0,
    last_error: null,
    created_by: 'u1',
    created_at: '2026-08-30T12:00:00Z',
    updated_at: '2026-08-30T12:00:00Z',
  };
}

async function shellRoutes(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/me', (route) => route.fulfill({ json: account }));
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: businesses }));
}

test('campaigns: create, preview, guard edits, test, and confirm send', async ({ page }) => {
  await shellRoutes(page);
  let campaign = draftCampaign();
  let testRecipients: string[] = [];
  let sendCount = 0;

  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) =>
    route.fulfill({ json: { items: [list], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/sending-profile`, (route) =>
    route.fulfill({ json: profile }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns`, (route) => {
    if (route.request().method() === 'POST') {
      const body = route.request().postDataJSON() as Record<string, unknown>;
      campaign = { ...campaign, name: String(body['name']), list_id: String(body['list_id']) };
      return route.fulfill({ status: 201, json: campaign });
    }
    return route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}`,
    (route) => {
      if (route.request().method() === 'PATCH') {
        campaign = { ...campaign, ...(route.request().postDataJSON() as typeof campaign) };
      }
      return route.fulfill({ json: campaign });
    },
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/preview`, (route) => {
    const body = route.request().postDataJSON() as { body_markdown: string };
    return route.fulfill({
      json: {
        html: `<style>body{font-family:sans-serif}</style><main>${body.body_markdown}</main>`,
        text: body.body_markdown,
      },
    });
  });
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}/test-send`,
    (route) => {
      testRecipients = (route.request().postDataJSON() as { to: string[] }).to;
      return route.fulfill({ status: 204 });
    },
  );
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}/send`,
    (route) => {
      sendCount++;
      campaign = { ...campaign, status: 'sending' };
      return route.fulfill({ json: campaign });
    },
  );

  await page.goto('/mailing/campaigns');
  await page.getByTestId('mailing-campaign-name').fill('September update');
  await page.getByTestId('mailing-campaign-create').click();
  await expect(page).toHaveURL(new RegExp(`/mailing/${BUSINESS_ID}/campaigns/${CAMPAIGN_ID}$`));

  await page.getByTestId('mailing-content-subject').fill('What is new');
  await page.getByTestId('mailing-content-body').fill('Hello ');
  await page.getByTestId('mailing-variable-first_name').click();
  await expect(page.getByTestId('mailing-content-body')).toHaveValue('Hello {{first_name}}');

  page.once('dialog', (dialog) => dialog.dismiss());
  await page.getByTestId('campaign-editor-back').click();
  await expect(page).toHaveURL(new RegExp(`/mailing/${BUSINESS_ID}/campaigns/${CAMPAIGN_ID}$`));

  const frame = page.getByTestId('mailing-preview-frame');
  await expect
    .poll(() => frame.evaluate((element: HTMLIFrameElement) => element.srcdoc))
    .toContain('<style>');
  await expect
    .poll(() => frame.evaluate((element: HTMLIFrameElement) => element.srcdoc))
    .toContain('{{first_name}}');

  await page.getByTestId('campaign-save').click();
  await expect.poll(() => campaign.body_markdown).toBe('Hello {{first_name}}');

  await page.getByTestId('campaign-test-to').fill('ada@example.com, grace@example.com');
  await page.getByTestId('campaign-test-send').click();
  await expect.poll(() => testRecipients).toEqual(['ada@example.com', 'grace@example.com']);

  await page.getByTestId('campaign-send-now').click();
  await expect(page.getByTestId('campaign-send-confirmation')).toBeVisible();
  expect(sendCount).toBe(0);
  await page.getByTestId('campaign-send-confirm').click();
  await expect.poll(() => sendCount).toBe(1);
  await expect(page.getByTestId('campaign-cancel')).toBeDisabled();
});

test('campaigns: inspect stats and manage the suppression list through real navigation', async ({
  page,
}) => {
  await shellRoutes(page);
  const sentCampaign = {
    ...draftCampaign(),
    status: 'sent',
    subject: 'What is new',
    recipient_count: 100,
    sent_count: 98,
    delivered_count: 96,
    bounced_count: 2,
    complained_count: 1,
    opened_count: 48,
    clicked_count: 24,
    unsubscribed_count: 3,
    failed_count: 2,
  };
  let suppressions = [
    {
      id: SUPPRESSION_ID,
      business_id: BUSINESS_ID,
      tenant_root_id: BUSINESS_ID,
      email: 'blocked@example.com',
      reason: 'bounce',
      source: 'resend',
      created_at: '2026-09-01T12:00:00Z',
    },
  ];

  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/lists`, (route) =>
    route.fulfill({ json: { items: [list], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/sending-profile`, (route) =>
    route.fulfill({ json: profile }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns`, (route) =>
    route.fulfill({ json: { items: [sentCampaign], next_cursor: null } }),
  );
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}`,
    (route) => route.fulfill({ json: sentCampaign }),
  );
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}/stats`,
    (route) =>
      route.fulfill({
        json: {
          campaign: sentCampaign,
          links: [{ url: 'https://example.com/docs', click_count: 30, unique_click_count: 24 }],
        },
      }),
  );
  await page.route(
    `**/api/v1/businesses/${BUSINESS_ID}/mailing/campaigns/${CAMPAIGN_ID}/deliveries**`,
    (route) =>
      route.fulfill({
        json: {
          items: [
            {
              id: DELIVERY_ID,
              campaign_id: CAMPAIGN_ID,
              subscriber_id: SUBSCRIBER_ID,
              email: 'ada@example.com',
              status: 'delivered',
              attempts: 1,
              not_before: '2026-09-01T12:00:00Z',
              lease_until: null,
              message_id: 'message-1',
              provider_message_id: 'provider-1',
              opened_at: '2026-09-01T12:02:00Z',
              first_clicked_at: '2026-09-01T12:03:00Z',
              last_error: null,
              created_at: '2026-09-01T12:00:00Z',
            },
          ],
          next_cursor: null,
        },
      }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/suppressions**`, (route) => {
    const method = route.request().method();
    const url = new URL(route.request().url());
    if (method === 'POST') {
      const body = route.request().postDataJSON() as { email: string; reason: string };
      const created = {
        ...suppressions[0],
        id: CREATED_SUPPRESSION_ID,
        email: body.email,
        reason: body.reason,
        source: 'manual',
      };
      suppressions = [created, ...suppressions];
      return route.fulfill({ status: 201, json: created });
    }
    if (method === 'DELETE') {
      const id = url.pathname.split('/').at(-1);
      suppressions = suppressions.filter((item) => item.id !== id);
      return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: { items: suppressions, next_cursor: null } });
  });

  await page.goto('/mailing/campaigns');
  await page.getByTestId('mailing-campaign-open').click();
  await expect(page).toHaveURL(new RegExp(`/mailing/${BUSINESS_ID}/campaigns/${CAMPAIGN_ID}$`));
  await page.getByTestId('campaign-view-stats').click();
  await expect(page).toHaveURL(
    new RegExp(`/mailing/${BUSINESS_ID}/campaigns/${CAMPAIGN_ID}/stats$`),
  );
  await expect(page.getByTestId('stat-delivered')).toHaveText('96');
  await expect(page.getByTestId('campaign-link-row')).toContainText('example.com/docs');
  await expect(page.getByTestId('campaign-delivery-row')).toContainText('ada@example.com');

  await page.getByTestId('campaign-stats-back').click();
  await page.getByTestId('campaigns-suppression-link').click();
  await expect(page).toHaveURL(/\/mailing\/suppression$/);
  await page.getByTestId('suppression-email').fill('manual@example.com');
  await page.getByTestId('suppression-create').click();
  await expect(page.getByTestId('suppression-row').first()).toContainText('manual@example.com');
  await page.getByTestId('suppression-delete').first().click();
  await page.getByTestId('suppression-delete-confirm').click();
  await expect(page.getByText('manual@example.com')).toHaveCount(0);
});
