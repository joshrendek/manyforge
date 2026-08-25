import { expect, Page, test } from '@playwright/test';

// manyforge-nk50 — the multi-site analytics grid.
//
// The point of this screen is that sites from DIFFERENT businesses appear together. The previous
// screen made you pick one business from a dropdown first, so comparing a parent business against
// its sub-businesses meant one page load each.
//
// These codify the browser verification rather than leaving it as a one-off manual check. The
// grouping and the card→dashboard link are the two things that would break silently: a card that
// links under the wrong business id still LOOKS right on this page and only 404s after the click,
// because the summary endpoint asserts the site belongs to the business in the URL.

const ROOT = '11111111-1111-1111-1111-111111111111';
const SUB_A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const SUB_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

function series(days: number, at: (i: number) => number) {
  return Array.from({ length: days }, (_, i) => ({
    date: `2026-07-${String((i % 28) + 1).padStart(2, '0')}`,
    pageviews: at(i),
    visitors: Math.max(1, Math.floor(at(i) / 2)),
  }));
}

const overview = {
  data_as_of: '2026-07-30T23:59:30Z',
  sites: [
    {
      client_id: 'c1',
      name: 'acme.example',
      business_id: ROOT,
      business_name: 'Acme Holdings',
      pageviews: 762,
      visitors: 22,
      average_daily_visitors: 11.8,
      series: series(30, (i) => 5 + i),
    },
    {
      client_id: 'c2',
      name: 'docs.acme.example',
      business_id: ROOT,
      business_name: 'Acme Holdings',
      pageviews: 555,
      visitors: 13,
      average_daily_visitors: 7.1,
      series: series(30, (i) => 30 - i),
    },
    {
      client_id: 'c3',
      name: 'eng-blog.example',
      business_id: SUB_A,
      business_name: 'Engineering',
      pageviews: 255,
      visitors: 8,
      average_daily_visitors: 4.2,
      series: series(30, () => 9),
    },
    {
      // Registered but never tagged: must still be listed.
      client_id: 'c4',
      name: 'sales-lp.example',
      business_id: SUB_B,
      business_name: 'Sales',
      pageviews: 0,
      visitors: 0,
      average_daily_visitors: 0,
      series: series(30, () => 0).map((point) => ({ ...point, visitors: 0 })),
    },
  ],
};

async function installStack(page: Page, body: unknown = overview) {
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
  // Broad fallback FIRST: unmocked shell calls otherwise 401 and bounce the test to /login.
  await page.route('**/api/v1/**', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/analytics/overview**', (route) => route.fulfill({ json: body }));
}

test('shows sites from several businesses at once, grouped by business', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');

  const groups = page.getByTestId('overview-group');
  await expect(groups).toHaveCount(3);
  await expect(page.getByTestId('overview-group-name')).toHaveText([
    'Acme Holdings',
    'Engineering',
    'Sales',
  ]);

  // Four sites across three businesses, all visible without switching context — the feature.
  await expect(page.getByTestId('overview-card')).toHaveCount(4);
  await expect(groups.nth(0).getByTestId('overview-card')).toHaveCount(2);
  await expect(groups.nth(1).getByTestId('overview-card')).toHaveCount(1);
  await expect(page.getByTestId('overview-freshness')).toContainText('2026-07-30T23:59:30Z');
});

test('a card links to that site’s dashboard under its OWN business', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');

  // The sub-business case is the one that matters: linking under the parent's id would render
  // fine here and only fail after the click.
  const engCard = page.locator('[data-testid="overview-card"][data-client-id="c3"]');
  await expect(engCard).toHaveAttribute('href', `/analytics/${SUB_A}/c3`);

  await engCard.click();
  await expect(page).toHaveURL(new RegExp(`/analytics/${SUB_A}/c3$`));
});

test('renders a sparkline for sites with traffic', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');

  const spark = page
    .locator('[data-testid="overview-card"][data-client-id="c1"]')
    .getByTestId('overview-card-spark');
  await expect(spark).toBeVisible();

  const points = await spark.locator('polyline').getAttribute('points');
  expect(points).toBeTruthy();
  expect(points!.split(' ').length).toBe(30);
  // Coordinates must be finite and inside the viewBox; a NaN silently renders nothing at all.
  for (const pair of points!.split(' ')) {
    const [x, y] = pair.split(',').map(Number);
    expect(Number.isFinite(x)).toBe(true);
    expect(Number.isFinite(y)).toBe(true);
    expect(x).toBeGreaterThanOrEqual(0);
    expect(x).toBeLessThanOrEqual(100);
    expect(y).toBeGreaterThanOrEqual(0);
    expect(y).toBeLessThanOrEqual(28);
  }
});

test('headlines average daily visitors and keeps peak visitors as context', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');

  const card = page.locator('[data-testid="overview-card"][data-client-id="c1"]');
  await expect(card.getByTestId('overview-card-visitors')).toHaveText('11.8');
  await expect(card).toContainText('average daily visitors');
  await expect(card).toContainText('peak 22 visitors');
});

test('a site with no traffic is listed, with an explicit no-data message', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');

  const quiet = page.locator('[data-testid="overview-card"][data-client-id="c4"]');
  await expect(quiet).toBeVisible();
  // Omitting it, or showing a blank card, both read as "the tag is broken" to someone who just
  // installed it.
  await expect(quiet.getByTestId('overview-card-nodata')).toBeVisible();
  await expect(quiet.getByTestId('overview-card-spark')).toHaveCount(0);
});

test('changing the range refetches with the new window', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');
  await expect(page.getByTestId('overview-card').first()).toBeVisible();

  const req = page.waitForRequest((r) => r.url().includes('/analytics/overview?days=7'));
  await page.getByTestId('overview-range').selectOption('7');
  await req;
});

test('empty state when the account has no sites', async ({ page }) => {
  await installStack(page, { sites: [], data_as_of: null });
  await page.goto('/analytics');
  await expect(page.getByTestId('overview-empty')).toBeVisible();
  await expect(page.getByTestId('overview-freshness')).toContainText(
    'Data freshness is not available yet',
  );
});

test('site management is still reachable from the grid', async ({ page }) => {
  await installStack(page);
  await page.goto('/analytics');
  // Registering a site and copying its embed tag lived on this route before the grid replaced it;
  // losing that path would strand the only place a tag can be obtained.
  await expect(page.getByTestId('overview-manage')).toHaveAttribute('href', '/analytics/sites');
});
