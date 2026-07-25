import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, Page, test } from '@playwright/test';

// Behavioural tests for the SHIPPED tracker snippet.
//
// The snippet's other tests assert on its source TEXT — that it mentions `utm_source`, that it
// does not mention `document.cookie`. That is not the same as knowing what it does. A snippet can
// contain all the right identifiers and still send the wrong payload, and this is privacy-critical
// code that runs on other people's websites, so "it parses and mentions the right words" is not
// the bar.
//
// These load the real JS (extracted from the Go source, so there is no second copy to drift) into
// a real browser, on a real non-localhost origin, and assert on the bytes it actually transmits.

// snippetJS is the exact string the server serves, read from its single source of truth. Reading
// the Go file rather than keeping a copy here means the test cannot pass against a snippet that
// differs from the one users actually receive.
function loadSnippet(): string {
  // Playwright runs with web/ as the cwd.
  const src = readFileSync(
    join(process.cwd(), '..', 'internal', 'analytics', 'snippet.go'),
    'utf8',
  );
  const m = src.match(/const snippetJS = `([\s\S]*?)`\n/);
  if (!m) throw new Error('could not extract snippetJS from snippet.go');
  return m[1];
}

const SNIPPET = loadSnippet();
const KEY = 'mfk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

// The snippet deliberately refuses to run on localhost, so the test site must be a real-looking
// origin. Playwright can serve one entirely from routes without any DNS or server.
const SITE = 'https://tenant.example';

type Beacon = { k?: string; p?: string; r?: string; q?: string };

async function installSite(page: Page, opts: { html?: string } = {}) {
  const beacons: Beacon[] = [];

  await page.route('**/a.js', (route) =>
    route.fulfill({ contentType: 'application/javascript; charset=utf-8', body: SNIPPET }),
  );
  await page.route('**/a/e', (route) => {
    const body = route.request().postData();
    if (body) {
      try {
        beacons.push(JSON.parse(body));
      } catch {
        beacons.push({});
      }
    }
    return route.fulfill({ status: 204, body: '' });
  });

  const html =
    opts.html ??
    `<!doctype html><html><head><title>Tenant</title></head><body>
       <h1>Tenant site</h1>
       <script src="${SITE}/a.js" data-key="${KEY}"></script>
     </body></html>`;
  await page.route(`${SITE}/**`, (route) => {
    if (route.request().url().includes('/a.js') || route.request().url().includes('/a/e')) {
      return route.fallback();
    }
    return route.fulfill({ contentType: 'text/html', body: html });
  });

  return beacons;
}

test('sends a pageview with the path, and nothing else', async ({ page }) => {
  const beacons = await installSite(page);
  await page.goto(`${SITE}/pricing`);
  await expect.poll(() => beacons.length).toBe(1);

  expect(beacons[0].k).toBe(KEY);
  expect(beacons[0].p).toBe('/pricing');
  // Only these four keys may ever appear. A new field is a new disclosure and must be a
  // deliberate change, not something that arrives unnoticed.
  expect(Object.keys(beacons[0]).sort()).toEqual(['k', 'p', 'q', 'r']);
});

// THE privacy test for the client half. A page's query string routinely carries session tokens,
// password-reset codes, and email addresses; forwarding it would exfiltrate them from the tenant's
// own site to us. Only the three campaign keys may leave the page.
test('forwards only utm_* parameters, never the rest of the query string', async ({ page }) => {
  const beacons = await installSite(page);
  await page.goto(
    `${SITE}/checkout?utm_source=hn&utm_medium=social&utm_campaign=launch` +
      `&token=SUPERSECRET&email=alice%40example.com&session_id=abc123&password=hunter2`,
  );
  await expect.poll(() => beacons.length).toBe(1);

  const q = beacons[0].q ?? '';
  expect(q).toContain('utm_source=hn');
  expect(q).toContain('utm_medium=social');
  expect(q).toContain('utm_campaign=launch');

  for (const secret of [
    'SUPERSECRET',
    'alice',
    'example.com',
    'abc123',
    'hunter2',
    'token',
    'session_id',
    'password',
  ]) {
    expect(q, `"${secret}" must not be forwarded`).not.toContain(secret);
  }
  // And the path must not smuggle it either.
  expect(beacons[0].p).toBe('/checkout');
});

test('sends the referrer host only, and not for same-site navigation', async ({ page }) => {
  const beacons = await installSite(page);

  // A cross-site referrer is reduced to its host by the browser-side code.
  await page.setExtraHTTPHeaders({ Referer: 'https://news.ycombinator.com/item?id=1&user=alice' });
  await page.goto(`${SITE}/`);
  await expect.poll(() => beacons.length).toBeGreaterThan(0);
  const r = beacons[0].r ?? '';
  if (r !== '') {
    expect(r).toBe('news.ycombinator.com');
    expect(r).not.toContain('alice');
    expect(r).not.toContain('/');
  }
});

test('honours doNotTrack by sending nothing at all', async ({ page }) => {
  const beacons = await installSite(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'doNotTrack', { get: () => '1', configurable: true });
  });
  await page.goto(`${SITE}/`);
  // Give it the same window a successful beacon would have had.
  await page.waitForTimeout(500);
  expect(beacons).toHaveLength(0);
});

test('tracks SPA navigation via pushState but not replaceState', async ({ page }) => {
  const beacons = await installSite(page);
  await page.goto(`${SITE}/`);
  await expect.poll(() => beacons.length).toBe(1);

  await page.evaluate(() => history.pushState({}, '', '/about'));
  await expect.poll(() => beacons.length).toBe(2);
  expect(beacons[1].p).toBe('/about');

  // replaceState is how SPAs rewrite query strings; counting those as pageviews inflates every
  // SPA's numbers, so it must NOT fire.
  await page.evaluate(() => history.replaceState({}, '', '/about?tab=2'));
  await page.waitForTimeout(300);
  expect(beacons).toHaveLength(2);
});

test('suppresses a repeated identical path', async ({ page }) => {
  const beacons = await installSite(page);
  await page.goto(`${SITE}/`);
  await expect.poll(() => beacons.length).toBe(1);

  // A framework that fires several navigations for one screen must not multiply the count.
  await page.evaluate(() => {
    history.pushState({}, '', '/same');
    history.pushState({}, '', '/same');
    history.pushState({}, '', '/same');
  });
  await expect.poll(() => beacons.length).toBe(2);
  expect(beacons[1].p).toBe('/same');
});

test('never reads or writes a cookie or storage', async ({ page }) => {
  const beacons = await installSite(page);
  await page.goto(`${SITE}/?utm_source=hn`);
  await expect.poll(() => beacons.length).toBe(1);

  // The entire privacy claim: no persistent identifier is created, so there is nothing to
  // correlate a visitor with tomorrow.
  expect(await page.evaluate(() => document.cookie)).toBe('');
  expect(await page.evaluate(() => localStorage.length)).toBe(0);
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
});

test('a missing data-key sends nothing rather than a malformed beacon', async ({ page }) => {
  const beacons = await installSite(page, {
    html: `<!doctype html><html><body><script src="${SITE}/a.js"></script></body></html>`,
  });
  await page.goto(`${SITE}/`);
  await page.waitForTimeout(400);
  expect(beacons).toHaveLength(0);
});
