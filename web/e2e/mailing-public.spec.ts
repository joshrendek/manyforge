import { expect, test } from '@playwright/test';

const profile = {
  id: 'sp1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  mode: 'resend',
  from_email: 'news@example.test',
  from_name: 'Acme News',
  reply_to: null,
  postal_address: null,
  email_domain_id: null,
  ses_region: null,
  ses_configuration_set: null,
  sns_topic_arn: null,
  status: 'unverified',
  last_verified_at: null,
  verify_error: null,
  has_credentials: true,
  created_at: '2026-08-28T10:00:00Z',
  updated_at: '2026-08-28T10:00:00Z',
};

async function shellRoutes(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'test-token'));
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: {
        id: 'u1',
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
            id: 'b1',
            parent_id: null,
            tenant_root_id: 'b1',
            name: 'Acme',
            status: 'active',
            is_tenant_root: true,
          },
        ],
      },
    }),
  );
}

test('sending profile keeps credentials write-only and supports verification', async ({ page }) => {
  await shellRoutes(page);
  let currentProfile = { ...profile };
  await page.route('**/api/v1/businesses/b1/email-domains**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/b1/mailing/sending-profile', (route) => {
    if (route.request().method() === 'PUT') {
      currentProfile = {
        ...currentProfile,
        ...route.request().postDataJSON(),
        status: 'unverified',
      };
      return route.fulfill({ json: currentProfile });
    }
    return route.fulfill({ json: currentProfile });
  });
  await page.route('**/api/v1/businesses/b1/mailing/sending-profile/verify', (route) => {
    currentProfile = { ...currentProfile, status: 'verified' };
    return route.fulfill({ json: currentProfile });
  });

  await page.goto('/mailing/sending');
  await expect(page.getByTestId('sending-credentials-stored')).toBeVisible();
  await expect(page.getByTestId('sending-resend-key')).toHaveCount(0);
  await expect(page.getByTestId('sending-postal-warning')).toBeVisible();
  await page.getByTestId('sending-profile-verify').click();
  await expect(page.getByTestId('sending-profile-status')).toContainText('Verified');
});

test('public mailing signup always lands on the generic check-inbox state', async ({ page }) => {
  let submitted: Record<string, unknown> | null = null;
  await page.route('**/api/v1/mailing/public/mlp_demo/subscribe', (route) => {
    submitted = route.request().postDataJSON() as Record<string, unknown>;
    return route.fulfill({ status: 202, json: { accepted: true } });
  });

  await page.goto('/m/s/mlp_demo?name=Product%20updates');
  await expect(page.getByTestId('mailing-public-shell')).toBeVisible();
  await expect(page.getByTestId('portal-main')).toBeVisible();
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Join Product updates' })).toBeVisible();
  await page.getByTestId('mailing-public-email').fill('ada@example.test');
  await page.getByTestId('mailing-public-first-name').fill('Ada');
  await page.getByTestId('mailing-public-submit').click();

  await expect.poll(() => submitted).not.toBeNull();
  expect(submitted).toMatchObject({ email: 'ada@example.test', first_name: 'Ada' });
  await expect(page.getByTestId('mailing-public-done')).toContainText('Check your inbox');
  await expect(page.getByTestId('mailing-public-done')).not.toContainText('ada@example.test');
});

test('confirmation and unsubscribe use the same token-free completion copy', async ({ page }) => {
  await page.route('**/m/confirm/confirm-token', (route) => {
    if (route.request().method() === 'POST') return route.fulfill({ status: 500, body: 'failed' });
    return route.continue();
  });
  await page.route('**/m/u/unsubscribe-token', (route) => {
    if (route.request().method() === 'POST') return route.fulfill({ status: 200, body: '' });
    return route.continue();
  });

  await page.goto('/m/confirm/confirm-token');
  await page.getByTestId('mailing-confirm-submit').click();
  const confirmCopy = (await page.getByTestId('mailing-public-done').innerText()).trim();
  expect(confirmCopy).not.toContain('confirm-token');

  await page.goto('/m/u/unsubscribe-token');
  await page.getByTestId('mailing-unsubscribe-submit').click();
  const unsubscribeCopy = (await page.getByTestId('mailing-public-done').innerText()).trim();
  expect(unsubscribeCopy).toBe(confirmCopy);
  expect(unsubscribeCopy).not.toContain('unsubscribe-token');
});
