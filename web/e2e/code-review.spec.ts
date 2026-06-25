import { expect, test } from '@playwright/test';

// ---------------------------------------------------------------------------
// Shared fixtures
// ---------------------------------------------------------------------------

const profile = {
  id: '1',
  email: 'a@b.c',
  display_name: 'A',
  email_verified: true,
  status: 'active',
};

const biz = {
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

const agent = {
  id: 'a1',
  business_id: 'b1',
  principal_id: 'p1',
  name: 'Code Bot',
  provider: 'anthropic',
  model: 'claude-opus-4-8',
  system_prompt: '',
  allowed_tools: [],
  autonomy_mode: 1,
  enabled: true,
  monthly_budget_cents: 2500,
  allowed_mcp_servers: [],
  retriage_on_reply: false,
  created_at: '2026-06-15T00:00:00Z',
  updated_at: '2026-06-15T00:00:00Z',
};

const connector = {
  id: 'c1',
  type: 'github',
  display_name: 'acme/api',
  base_url: 'https://api.github.com',
  repo: 'acme/api',
  allow_private_base_url: false,
  status: 'active' as const,
  created_at: '2026-06-15T00:00:00Z',
};

const reviewPending = {
  id: 'r1',
  status: 'pending',
  summary: '',
  review_url: '',
  pr_number: 5,
  findings: [],
  findings_count: 0,
  created_at: '2026-06-15T00:00:00Z',
  posted_at: null,
};

const reviewSucceeded = {
  id: 'r1',
  status: 'succeeded',
  summary: 'Looks good overall.',
  review_url: 'https://github.com/acme/api/pull/5#issuecomment-1',
  pr_number: 5,
  findings: [
    {
      file: 'src/main.go',
      line: 42,
      severity: 'warning',
      title: 'Unused variable',
      detail: 'Variable x is declared but never used.',
    },
  ],
  findings_count: 1,
  created_at: '2026-06-15T00:00:00Z',
  posted_at: '2026-06-15T00:01:00Z',
};

// ---------------------------------------------------------------------------
// Auth helper — mirrors agents.spec.ts
// ---------------------------------------------------------------------------

async function auth(page: import('@playwright/test').Page): Promise<void> {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('code-review: trigger a review and watch it complete', async ({ page }) => {
  await auth(page);

  // -- Static reads -------------------------------------------------------
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ json: { items: [agent] } }),
  );
  await page.route('**/api/v1/businesses/b1/repo-connectors', (r) =>
    r.fulfill({ json: { items: [connector] } }),
  );

  // -- code-reviews list endpoint: counter-based pending→succeeded ---------
  // GET call count (POST excluded):
  //   1st → page init → []         (no polling started; empty list)
  //   2nd → first poll after POST  → [pending]  (polling continues)
  //   3rd → second poll            → [succeeded] (polling stops)
  let listGetCount = 0;
  await page.route('**/api/v1/businesses/b1/code-reviews', (r) => {
    if (r.request().method() === 'POST') {
      return r.fulfill({
        status: 202,
        json: { id: 'r1', status: 'pending', review_url: '' },
      });
    }
    listGetCount += 1;
    if (listGetCount <= 1) {
      return r.fulfill({ json: { items: [] } });
    }
    if (listGetCount === 2) {
      return r.fulfill({ json: { items: [reviewPending] } });
    }
    return r.fulfill({ json: { items: [reviewSucceeded] } });
  });

  // -- GET /code-reviews/r1 (detail page) → succeeded + findings ----------
  await page.route('**/api/v1/businesses/b1/code-reviews/r1', (r) =>
    r.fulfill({ json: reviewSucceeded }),
  );

  // -----------------------------------------------------------------------
  // 1. Navigate to the list page
  // -----------------------------------------------------------------------
  await page.goto('/code-review');

  // -----------------------------------------------------------------------
  // 2. Fill the trigger form and submit
  // -----------------------------------------------------------------------
  await page.getByTestId('cr-agent').selectOption('a1');
  await page.getByTestId('cr-connector').selectOption('c1');
  await page.getByTestId('cr-pr-number').fill('5');
  await page.getByTestId('cr-submit').click();

  // -----------------------------------------------------------------------
  // 3. Optimistic row appears immediately with status pending.
  //    toContainText retries until the row appears, confirming the POST
  //    response arrived and startPolling() registered the 3-s interval.
  // -----------------------------------------------------------------------
  await expect(page.getByTestId('review-row')).toContainText('pending');

  // -----------------------------------------------------------------------
  // 4. Wait for the real poll interval (3 s × 2 cycles + margin) to fire.
  //    Polling: cycle 1 → [pending] (still non-terminal → keeps polling);
  //             cycle 2 → [succeeded] → row updates, polling stops.
  //    The test timeout is 30 s so 10 s here is well within budget.
  // -----------------------------------------------------------------------
  await expect(page.getByTestId('review-row')).toContainText('succeeded', { timeout: 10_000 });

  // -----------------------------------------------------------------------
  // 5. Navigate to the detail page by clicking the row
  // -----------------------------------------------------------------------
  await page.getByTestId('review-row').click();

  // Detail page: ≥1 finding-row and view-on-github link
  await expect(page.getByTestId('finding-row')).toHaveCount(1);
  await expect(page.getByTestId('view-on-github')).toBeVisible();
  await expect(page.getByTestId('view-on-github')).toHaveAttribute(
    'href',
    'https://github.com/acme/api/pull/5#issuecomment-1',
  );
});

test('code-review: connector list renders rows', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ json: { items: [agent] } }),
  );
  await page.route('**/api/v1/businesses/b1/repo-connectors', (r) =>
    r.fulfill({ json: { items: [connector] } }),
  );
  await page.route('**/api/v1/businesses/b1/code-reviews', (r) =>
    r.fulfill({ json: { items: [] } }),
  );
  await page.goto('/code-review');
  await expect(page.getByTestId('connector-row')).toContainText('acme/api');
});
