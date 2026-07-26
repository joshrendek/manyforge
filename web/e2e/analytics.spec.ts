import { expect, Page, test } from '@playwright/test';

// as0 analytics: sites screen (embed tag) + dashboard (totals, chart, breakdowns).
//
// NOTE the sites screen lives at /analytics/sites, not /analytics. nk50 made the bare route the
// multi-site grid; site registration moved one level down. See e2e/analytics-overview.spec.ts.
//
// These codify the flow that was previously only verified by hand: register a site → copy the
// embed tag → see traffic. The embed tag assertion matters most — it is the one string a tenant
// physically copies into someone else's website, so a malformed one is a silent total failure.

const BIZ_ID = '11111111-1111-1111-1111-111111111111';
const SITE_ID = '22222222-2222-2222-2222-222222222222';
const KEY = 'mfk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

const business = {
  id: BIZ_ID,
  parent_id: null,
  tenant_root_id: BIZ_ID,
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};

const clients = {
  clients: [
    {
      id: SITE_ID,
      business_id: BIZ_ID,
      tenant_root_id: BIZ_ID,
      kind: 'analytics',
      name: 'garden.gg',
      publishable_key: KEY,
      status: 'active',
      require_signature: false,
      has_secret: false,
      created_at: '2026-07-25T00:00:00Z',
      revoked_at: null,
    },
    {
      id: '33333333-3333-3333-3333-333333333333',
      business_id: BIZ_ID,
      tenant_root_id: BIZ_ID,
      kind: 'crash',
      name: 'iOS app',
      publishable_key: 'mfk_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB',
      status: 'active',
      require_signature: false,
      has_secret: false,
      created_at: '2026-07-25T00:00:00Z',
      revoked_at: null,
    },
  ],
};

const summary = {
  from: '2026-06-26',
  to: '2026-07-25',
  pageviews: 42,
  visitors: 9,
  direct_pageviews: 30,
  series: [
    { date: '2026-07-24', pageviews: 12, visitors: 4 },
    { date: '2026-07-25', pageviews: 30, visitors: 9 },
  ],
  top_pages: [
    { path: '/', pageviews: 25, visitors: 8 },
    { path: '/pricing', pageviews: 17, visitors: 5 },
  ],
  top_referrers: [{ host: 'news.ycombinator.com', pageviews: 12, visitors: 6 }],
  breakdowns: {
    utm_source: [{ value: 'hn', pageviews: 12, visitors: 6 }],
    utm_medium: [],
    utm_campaign: [],
    device: [
      { value: 'desktop', pageviews: 30, visitors: 6 },
      { value: 'mobile', pageviews: 12, visitors: 3 },
    ],
    browser: [{ value: 'Chrome', pageviews: 42, visitors: 9 }],
    country: [],
  },
};

async function installStack(page: Page, opts: { summary?: unknown } = {}) {
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
  // Broad fallback first: unmocked shell calls otherwise 401 and bounce the test to /login.
  await page.route('**/api/v1/**', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/businesses', (route) =>
    route.fulfill({ json: { items: [business] } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/telemetry/clients`, (route) =>
    route.fulfill({ json: clients }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/analytics/summary**`, (route) =>
    route.fulfill({ json: opts.summary ?? summary }),
  );
}

test('sites screen lists analytics sites and renders a complete embed tag', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics/sites');

  await expect(page.getByRole('heading', { name: 'Analytics sites' })).toBeVisible();

  // Crash clients belong to a different surface and must not appear here.
  await expect(page.getByTestId('site-row')).toHaveCount(1);
  await expect(page.getByTestId('site-name-cell')).toContainText('garden.gg');

  // The embed tag is the deliverable: it must be complete, contain the key, and be readable in
  // full rather than truncated — a tag the user cannot see entirely is one they cannot verify
  // before pasting it into their own site.
  const embed = page.getByTestId('site-embed');
  await expect(embed).toBeVisible();
  const tag = (await embed.textContent()) ?? '';
  expect(tag).toContain('<script');
  expect(tag).toContain('defer');
  expect(tag).toContain('/a.js');
  expect(tag).toContain(`data-key="${KEY}"`);
  expect(tag).toContain('</script>');
});

test('revoked sites stop advertising an embed tag', async ({ page }) => {
  await installStack(page);
  await page.route(`**/api/v1/businesses/${BIZ_ID}/telemetry/clients`, (route) =>
    route.fulfill({
      json: {
        clients: [{ ...clients.clients[0], status: 'revoked', revoked_at: '2026-07-25T01:00:00Z' }],
      },
    }),
  );
  await page.goto('/analytics/sites');

  await expect(page.getByTestId('site-row')).toHaveCount(1);
  await expect(page.getByTestId('site-status-cell')).toContainText('Revoked');
  // Pasting a revoked site's tag would collect nothing, so it must not be offered.
  await expect(page.getByTestId('site-embed')).toHaveCount(0);
  await expect(page.getByTestId('site-revoke')).toHaveCount(0);
});

test('dashboard renders totals, chart, top pages and referrers', async ({ page }) => {
  await installStack(page);
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);

  await expect(page.getByTestId('stat-pageviews')).toHaveText('42');
  await expect(page.getByTestId('stat-visitors')).toHaveText('9');
  await expect(page.getByTestId('stat-direct')).toHaveText('30');

  // One bar per day, with an accessible description.
  const chart = page.getByTestId('analytics-chart');
  await expect(chart).toBeVisible();
  await expect(chart).toHaveAttribute('role', 'img');
  await expect(chart.locator('rect')).toHaveCount(2);

  await expect(page.getByTestId('top-page-row')).toHaveCount(2);
  await expect(page.getByTestId('top-referrers')).toContainText('news.ycombinator.com');
});

// The visitor hash rotates daily by design, so no cross-day identifier exists and a deduplicated
// multi-day total cannot be computed. The label must not imply one — this is a correctness
// property of the privacy model, not a wording preference.
test('visitors is labelled as a peak-day figure, never a window total', async ({ page }) => {
  await installStack(page);
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);

  await expect(page.getByTestId('analytics-stats')).toContainText('peak day');
  await expect(page.getByTestId('analytics-stats')).not.toContainText('Unique visitors');
});

test('breakdown panels render only for dimensions that have data', async ({ page }) => {
  await installStack(page);
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);

  await expect(page.getByTestId('breakdown-device')).toBeVisible();
  await expect(page.getByTestId('breakdown-browser')).toBeVisible();
  await expect(page.getByTestId('breakdown-utm_source')).toBeVisible();

  // Empty dimensions are returned by the API but must not become empty tables on screen.
  await expect(page.getByTestId('breakdown-utm_medium')).toHaveCount(0);
  await expect(page.getByTestId('breakdown-country')).toHaveCount(0);

  await expect(page.getByTestId('breakdown-device')).toContainText('desktop');
  await expect(page.getByTestId('breakdown-device')).toContainText('mobile');
});

// With traffic but no country data, the deployment has no GeoIP database — say so, rather than
// leaving the user to conclude their audience is from nowhere.
test('explains a missing country breakdown instead of showing nothing', async ({ page }) => {
  await installStack(page);
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);

  await expect(page.getByTestId('country-unavailable')).toBeVisible();
  await expect(page.getByTestId('country-unavailable')).toContainText('MANYFORGE_GEOIP_DB');
});

test('a site with no traffic yet points back at the embed tag', async ({ page }) => {
  await installStack(page, {
    summary: {
      ...summary,
      pageviews: 0,
      visitors: 0,
      direct_pageviews: 0,
      series: [],
      top_pages: [],
      top_referrers: [],
      breakdowns: {
        utm_source: [],
        utm_medium: [],
        utm_campaign: [],
        device: [],
        browser: [],
        country: [],
      },
    },
  });
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);

  await expect(page.getByTestId('analytics-empty')).toBeVisible();
  await expect(page.getByTestId('analytics-empty')).toContainText('embed tag');
  // No traffic means no basis for the geo hint either — that would be two confusing messages.
  await expect(page.getByTestId('country-unavailable')).toHaveCount(0);
});

test('changing the range refetches', async ({ page }) => {
  await installStack(page);
  await page.goto(`/analytics/${BIZ_ID}/${SITE_ID}`);
  await expect(page.getByTestId('stat-pageviews')).toHaveText('42');

  const request = page.waitForRequest((r) => r.url().includes('days=7'));
  await page.getByTestId('range-select').selectOption('7');
  await request;
});
