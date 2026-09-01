import { expect, test } from '@playwright/test';

const profile = {
  id: 'p1',
  business_id: 'b1',
  tenant_root_id: 'b1',
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
  has_credentials: true,
  created_at: '2026-08-30T12:00:00Z',
  updated_at: '2026-08-30T12:00:00Z',
};
const account = {
  id: 'u1',
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
    id: 'c1',
    business_id: 'b1',
    tenant_root_id: 'b1',
    list_id: 'l1',
    profile_id: 'p1',
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

  await page.route('**/api/v1/businesses/b1/mailing/lists', (route) =>
    route.fulfill({ json: { items: [list], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/sending-profile', (route) =>
    route.fulfill({ json: profile }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/campaigns', (route) => {
    if (route.request().method() === 'POST') {
      const body = route.request().postDataJSON() as Record<string, unknown>;
      campaign = { ...campaign, name: String(body['name']), list_id: String(body['list_id']) };
      return route.fulfill({ status: 201, json: campaign });
    }
    return route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1', (route) => {
    if (route.request().method() === 'PATCH') {
      campaign = { ...campaign, ...(route.request().postDataJSON() as typeof campaign) };
    }
    return route.fulfill({ json: campaign });
  });
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/preview', (route) => {
    const body = route.request().postDataJSON() as { body_markdown: string };
    return route.fulfill({
      json: {
        html: `<style>body{font-family:sans-serif}</style><main>${body.body_markdown}</main>`,
        text: body.body_markdown,
      },
    });
  });
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1/test-send', (route) => {
    testRecipients = (route.request().postDataJSON() as { to: string[] }).to;
    return route.fulfill({ status: 204 });
  });
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1/send', (route) => {
    sendCount++;
    campaign = { ...campaign, status: 'sending' };
    return route.fulfill({ json: campaign });
  });

  await page.goto('/mailing/campaigns');
  await page.getByTestId('mailing-campaign-name').fill('September update');
  await page.getByTestId('mailing-campaign-create').click();
  await expect(page).toHaveURL(/\/mailing\/b1\/campaigns\/c1$/);

  await page.getByTestId('mailing-content-subject').fill('What is new');
  await page.getByTestId('mailing-content-body').fill('Hello ');
  await page.getByTestId('mailing-variable-first_name').click();
  await expect(page.getByTestId('mailing-content-body')).toHaveValue('Hello {{first_name}}');

  page.once('dialog', (dialog) => dialog.dismiss());
  await page.getByTestId('campaign-editor-back').click();
  await expect(page).toHaveURL(/\/mailing\/b1\/campaigns\/c1$/);

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
      id: 'sup1',
      business_id: 'b1',
      tenant_root_id: 'b1',
      email: 'blocked@example.com',
      reason: 'bounce',
      source: 'resend',
      created_at: '2026-09-01T12:00:00Z',
    },
  ];

  await page.route('**/api/v1/businesses/b1/mailing/lists', (route) =>
    route.fulfill({ json: { items: [list], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/sending-profile', (route) =>
    route.fulfill({ json: profile }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/campaigns', (route) =>
    route.fulfill({ json: { items: [sentCampaign], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1', (route) =>
    route.fulfill({ json: sentCampaign }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1/stats', (route) =>
    route.fulfill({
      json: {
        campaign: sentCampaign,
        links: [{ url: 'https://example.com/docs', click_count: 30, unique_click_count: 24 }],
      },
    }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/campaigns/c1/deliveries**', (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: 'd1',
            campaign_id: 'c1',
            subscriber_id: 's1',
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
  await page.route('**/api/v1/businesses/b1/mailing/suppressions**', (route) => {
    const method = route.request().method();
    const url = new URL(route.request().url());
    if (method === 'POST') {
      const body = route.request().postDataJSON() as { email: string; reason: string };
      const created = {
        ...suppressions[0],
        id: 'sup2',
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
  await expect(page).toHaveURL(/\/mailing\/b1\/campaigns\/c1$/);
  await page.getByTestId('campaign-view-stats').click();
  await expect(page).toHaveURL(/\/mailing\/b1\/campaigns\/c1\/stats$/);
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
