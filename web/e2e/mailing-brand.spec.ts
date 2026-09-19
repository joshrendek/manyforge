import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

const ACCOUNT_ID = '11111111-1111-4111-8111-111111111111';
const BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const PROFILE_ID = '33333333-3333-4333-8333-333333333333';
const BRAND_ID = '44444444-4444-4444-8444-444444444444';

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

const defaultColors = {
  background: '#f4f6f8',
  surface: '#ffffff',
  text: '#17212b',
  accent: '#1769aa',
  header_background: '#ffffff',
  header_text: '#17212b',
};

function setup(blobStorage: 'ready' | 'blocked') {
  return {
    checks: [
      ...['outbound_enabled', 'mailing_key', 'public_url', 'smtp_relay', 'dkim_key'].map((id) => ({
        id,
        label: id,
        status: 'ready',
        message: 'Available',
        action: '',
        required_for: ['relay', 'resend', 'ses'],
      })),
    ],
    logo_storage: {
      ready: blobStorage === 'ready',
      message: 'Brand logo uploads require object storage.',
      action: blobStorage === 'blocked' ? 'Set MANYFORGE_BLOB_URL.' : '',
    },
  };
}

async function shellRoutes(page: Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: {
        id: ACCOUNT_ID,
        email: 'operator@acme.test',
        display_name: 'Operator',
        email_verified: true,
        status: 'active',
      },
    }),
  );
  await page.route('**/api/v1/businesses', (route) =>
    route.fulfill({
      json: {
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
      },
    }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/sending-profile`, (route) =>
    route.fulfill({ json: profile }),
  );
}

test('a brand is created from the sender defaults and reaches the live preview', async ({
  page,
}) => {
  await shellRoutes(page);
  let stored: Record<string, unknown> | null = null;
  let saved: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/setup`, (route) =>
    route.fulfill({ json: setup('ready') }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/brand`, (route) => {
    if (route.request().method() === 'PUT') {
      saved = route.request().postDataJSON() as Record<string, unknown>;
      stored = {
        id: BRAND_ID,
        business_id: BUSINESS_ID,
        tenant_root_id: BUSINESS_ID,
        logo: null,
        logo_width: 160,
        created_at: '2026-09-01T10:00:00Z',
        updated_at: '2026-09-01T10:00:00Z',
        ...saved,
      };
      return route.fulfill({ json: stored });
    }
    if (stored) return route.fulfill({ json: stored });
    return route.fulfill({ status: 404, json: { error: 'not found' } });
  });
  let previewBrand: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/templates/preview`, (route) => {
    const body = route.request().postDataJSON() as { brand?: Record<string, unknown> };
    previewBrand = body.brand ?? null;
    const accent = (previewBrand?.colors as { accent?: string } | undefined)?.accent ?? '#1769aa';
    return route.fulfill({
      json: {
        html: `<div class="brand" style="color:${accent}">${previewBrand?.name ?? ''}</div>`,
        text: String(previewBrand?.name ?? ''),
      },
    });
  });

  await page.goto('/mailing/brand');
  await expect(page.getByTestId('brand-form')).toBeVisible();
  // The sender name seeds a brand that does not exist yet.
  await expect(page.getByTestId('brand-name')).toHaveValue('Acme News');
  await expect(page.getByTestId('brand-delete')).toHaveCount(0);

  await page.getByTestId('brand-footer').fill('Made in Berlin');
  await page.getByTestId('brand-save').click();

  await expect.poll(() => saved).not.toBeNull();
  expect(saved).toMatchObject({
    name: 'Acme News',
    colors: defaultColors,
    font_stack: 'system',
    footer_markdown: 'Made in Berlin',
  });
  await expect(page.getByTestId('brand-saved-hint')).toBeVisible();
  await expect(page.getByTestId('brand-delete')).toBeVisible();
  // The unsaved-edit override reaches the server preview, so the pane shows the brand.
  await expect.poll(() => previewBrand).not.toBeNull();
  await expect(page.getByTestId('mailing-preview-frame')).toHaveAttribute(
    'srcdoc',
    /Acme News/,
  );
});

test('logo upload is blocked with an explanation when object storage is unconfigured', async ({
  page,
}) => {
  await shellRoutes(page);
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/setup`, (route) =>
    route.fulfill({ json: setup('blocked') }),
  );
  await page.route(`**/api/v1/businesses/${BUSINESS_ID}/mailing/brand`, (route) =>
    route.fulfill({
      json: {
        id: BRAND_ID,
        business_id: BUSINESS_ID,
        tenant_root_id: BUSINESS_ID,
        name: 'Acme Labs',
        logo: null,
        logo_width: 160,
        colors: defaultColors,
        font_stack: 'system',
        footer_markdown: '',
        created_at: '2026-09-01T10:00:00Z',
        updated_at: '2026-09-01T10:00:00Z',
      },
    }),
  );

  await page.goto('/mailing/brand');
  await expect(page.getByTestId('brand-logo-file')).toBeDisabled();
  await expect(page.getByTestId('brand-logo-blocker')).toContainText('storage');
});

test('a branded list shows its own header on the public signup page', async ({ page }) => {
  await page.route('**/api/v1/mailing/public/mlp_brand/brand', (route) =>
    route.fulfill({
      json: {
        brand: {
          name: 'Acme Labs',
          logo_url: 'https://mail.example.test/m/b/brand/logo?v=abcd',
          colors: { ...defaultColors, accent: '#ff0000' },
        },
      },
    }),
  );
  await page.route('**/api/v1/mailing/public/mlp_brand/subscribe', (route) =>
    route.fulfill({ status: 202, json: { accepted: true } }),
  );

  await page.goto('/m/s/mlp_brand?name=Product%20updates');
  const logo = page.getByTestId('mailing-public-brand-logo');
  await expect(logo).toBeVisible();
  await expect(logo).toHaveAttribute('alt', 'Acme Labs');
  await expect(page.getByTestId('mailing-public-brand-name')).toHaveCount(0);
  await expect(page.getByText('Powered by')).toBeVisible();
});
