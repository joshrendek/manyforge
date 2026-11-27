import { Page, expect, test } from '@playwright/test';

// Portal custom-domain settings section on the inbox settings page. The /api/v1
// surface is mocked; the mock tracks state so add / verify / remove round-trip.
const BIZ_ID = 'biz-1';
const HOST = 'portal.example.test';

const business = {
  id: BIZ_ID,
  parent_id: null,
  tenant_root_id: 'root-1',
  name: 'Garden',
  status: 'active',
  is_tenant_root: true,
};

function portalDomain(verified: boolean) {
  return {
    id: 'pd-1',
    hostname: HOST,
    verified,
    verified_at: verified ? '2026-01-02T00:00:00Z' : null,
    dns: {
      verification_txt: { name: `_manyforge-portal.${HOST}`, value: 'mf-portal-verify=TESTTOKEN' },
      target: { type: 'CNAME', name: HOST, value: 'app.example.test' },
    },
    created_at: '2026-01-01T00:00:00Z',
  };
}

async function installBase(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  // Registered FIRST (lowest priority): unmocked shell calls get an empty 200
  // instead of a 401 that would redirect to /login mid-test.
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

test('add, verify (pending then verified) and remove a portal domain', async ({ page }) => {
  await installBase(page);

  let current: ReturnType<typeof portalDomain> | null = null;
  let verifyResult = false;
  const posted: unknown[] = [];

  await page.route(`**/api/v1/businesses/${BIZ_ID}/portal-domains**`, async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    const method = req.method();
    if (method === 'GET') {
      return route.fulfill({ json: { items: current ? [current] : [], next_cursor: null } });
    }
    if (method === 'POST' && path.endsWith('/verify')) {
      current = portalDomain(verifyResult);
      return route.fulfill({ json: current });
    }
    if (method === 'POST') {
      posted.push(req.postDataJSON());
      current = portalDomain(false);
      return route.fulfill({ status: 201, json: current });
    }
    if (method === 'DELETE') {
      current = null;
      return route.fulfill({ status: 204, body: '' });
    }
    return route.fallback();
  });

  await page.goto(`/support/${BIZ_ID}/settings/inbox`);
  await expect(page.getByTestId('portal-domain-empty')).toBeVisible();

  // Add
  await page.getByLabel('Hostname').fill(HOST);
  await page.getByTestId('portal-domain-add').click();
  const row = page.getByTestId('portal-domain-row');
  await expect(row).toHaveCount(1);
  expect(posted).toEqual([{ hostname: HOST }]);
  await expect(row.getByTestId('portal-domain-status')).toContainText('Pending');
  const dns = row.getByTestId('portal-domain-dns');
  await expect(dns.locator('.mf-dns-rec')).toHaveCount(2);
  await expect(dns).toContainText('CNAME');
  await expect(dns).toContainText('app.example.test');
  await expect(dns).toContainText('TXT');
  await expect(dns).toContainText(`_manyforge-portal.${HOST}`);
  await expect(dns).toContainText('mf-portal-verify=TESTTOKEN');
  await expect(row.getByTestId('portal-domain-routing-hint')).toContainText(
    'Verify only after your administrator confirms this hostname is routed here.',
  );

  // Verify, still pending: hint, no error
  await row.getByTestId('portal-domain-verify').click();
  await expect(row.getByTestId('portal-domain-verify-hint')).toContainText('Not verified yet');
  await expect(page.getByTestId('portal-domain-error')).toHaveCount(0);

  // Verify, now verified
  verifyResult = true;
  await row.getByTestId('portal-domain-verify').click();
  await expect(row.getByTestId('portal-domain-status')).toContainText('Verified');
  // The target record stays visible once verified; the TXT challenge does not.
  await expect(row.getByTestId('portal-domain-dns').locator('.mf-dns-rec')).toHaveCount(1);
  await expect(row.getByTestId('portal-domain-dns')).toContainText('CNAME');
  await expect(row.getByTestId('portal-domain-dns')).not.toContainText('TXT');
  await expect(row.getByTestId('portal-domain-routing-hint')).toHaveCount(0);
  await expect(row.getByTestId('portal-domain-verify')).toHaveCount(0);

  // Remove, accepting the confirm
  page.once('dialog', (d) => d.accept());
  await row.getByTestId('portal-domain-remove').click();
  await expect(page.getByTestId('portal-domain-row')).toHaveCount(0);
  await expect(page.getByTestId('portal-domain-empty')).toBeVisible();
});
