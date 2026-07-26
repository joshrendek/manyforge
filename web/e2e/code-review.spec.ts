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

// A multi-dimension (spec 008) review: findings tagged by lane, per-lane accounting
// in dimension_runs, and one configured lane (ui) skipped for want of matching files.
const reviewMultiDim = {
  id: 'r1',
  status: 'succeeded',
  summary: 'Reviewed across security and correctness.',
  review_url: 'https://github.com/acme/api/pull/5#issuecomment-1',
  pr_number: 5,
  model: 'x-ai/grok',
  findings: [
    {
      file: 'src/auth.go',
      line: 10,
      severity: 'error',
      title: 'SQLi',
      detail: 'Parameterize.',
      dimension: 'security',
    },
    {
      file: 'src/auth.go',
      line: 20,
      severity: 'warning',
      title: 'Weak hash',
      detail: 'Use bcrypt.',
      dimension: 'security',
    },
    {
      file: 'src/calc.go',
      line: 5,
      severity: 'warning',
      title: 'Off-by-one',
      detail: 'Loop bound.',
      dimension: 'correctness',
    },
  ],
  findings_count: 3,
  created_at: '2026-06-15T00:00:00Z',
  posted_at: '2026-06-15T00:01:00Z',
  dimension_runs: [
    {
      dimension: 'security',
      model: 'x-ai/grok',
      provider: 'openrouter',
      tokens_in: 100,
      tokens_out: 50,
      cost_cents: 2,
      status: 'succeeded',
      finding_count: 2,
    },
    {
      dimension: 'correctness',
      model: 'x-ai/grok',
      provider: 'openrouter',
      tokens_in: 80,
      tokens_out: 40,
      cost_cents: 1,
      status: 'succeeded',
      finding_count: 1,
    },
    {
      dimension: 'ui',
      tokens_in: 0,
      tokens_out: 0,
      cost_cents: 0,
      status: 'skipped',
      skipped_reason: 'no matching files',
      finding_count: 0,
    },
  ],
};

// ---------------------------------------------------------------------------
// Auth helper — mirrors agents.spec.ts
// ---------------------------------------------------------------------------

// Angular CDK DragDrop listens to pointer/mouse events, so page.mouse must be driven manually —
// the HTML5 dragTo protocol does nothing. Two things make that fragile, and both bit this file:
//
//   1. page.mouse.* takes raw VIEWPORT coordinates and does not auto-scroll the way locator
//      actions do. An element below the fold yields a bounding box whose centre hit-tests to
//      nothing, every event lands on no element, and CDK never sees a drag start. It looks
//      identical to "drag-and-drop is broken".
//   2. Coordinates measured before the scroll settles are stale, which reproduces (1)
//      intermittently — the failure mode that made this read as flaky rather than wrong.
//
// So: scroll, wait for the scroll position to actually stop moving, then ASSERT the handle is the
// element under its own centre before pressing. A silent miss becomes a clear failure.
async function cdkDragOnto(
  page: import('@playwright/test').Page,
  sourceTestId: string,
  targetTestId: string,
): Promise<void> {
  const source = page.getByTestId(sourceTestId);
  const target = page.getByTestId(targetTestId);
  await source.scrollIntoViewIfNeeded();
  await target.scrollIntoViewIfNeeded();

  // Wait for scrolling to come to rest; boundingBox() mid-animation is the stale-coordinate case.
  await page.waitForFunction(() => {
    const w = window as unknown as { __mfLastY?: number; __mfStable?: number };
    const y = window.scrollY;
    if (w.__mfLastY === y) {
      w.__mfStable = (w.__mfStable ?? 0) + 1;
    } else {
      w.__mfStable = 0;
    }
    w.__mfLastY = y;
    return (w.__mfStable ?? 0) >= 2;
  });

  const sb = (await source.boundingBox())!;
  const tb = (await target.boundingBox())!;

  // Both handles must genuinely be under their own centres, or the drag silently targets nothing.
  for (const [name, box] of [
    [sourceTestId, sb],
    [targetTestId, tb],
  ] as const) {
    const hit = await page.evaluate(
      ([x, y, id]) => {
        const el = document.elementFromPoint(x as number, y as number);
        if (!el) return 'NOTHING (off-screen)';
        return el.closest(`[data-testid="${id}"]`)
          ? 'ok'
          : el.getAttribute('data-testid') || el.tagName;
      },
      [box.x + box.width / 2, box.y + box.height / 2, name] as [number, number, string],
    );
    expect(hit, `${name} is not hittable at its own centre — the drag would target ${hit}`).toBe(
      'ok',
    );
  }

  await page.mouse.move(sb.x + sb.width / 2, sb.y + sb.height / 2);
  await page.mouse.down();
  // CDK's default dragStartThreshold is 5px, and the original nudge here was 4 — BELOW it. The
  // drag therefore never started on this move; it started somewhere inside the long move to the
  // target instead, at whatever position CDK first noticed, which is why the sort landed only
  // sometimes. This is the actual root cause of the flake. 12px clears the threshold outright.
  await page.mouse.move(sb.x + sb.width / 2, sb.y + sb.height / 2 - 12, { steps: 4 });

  // Wait for CDK to actually PICK UP the item before moving toward the target. Releasing before
  // this is what made the test flaky: the events were all delivered, but faster than CDK
  // processed them, so the sort never ran and the list reverted.
  await page.locator('.cdk-drag-preview').waitFor({ state: 'attached', timeout: 5000 });

  // Re-measure the TARGET now that the drag has begun. On pickup CDK swaps the dragged row for a
  // placeholder and appends a preview to the body, either of which can shift the remaining rows —
  // so coordinates taken before pickup can point just outside the row they were meant to hit.
  const tb2 = (await target.boundingBox()) ?? tb;
  tb.x = tb2.x;
  tb.y = tb2.y;
  tb.width = tb2.width;
  tb.height = tb2.height;

  // Approach in two stages with a frame yield between them. CDK decides the insertion point on
  // pointermove, and these rows are only ~30px tall, so the whole decision region is crossed in a
  // couple of events if the moves are delivered inside one frame.
  await page.mouse.move(tb.x + tb.width / 2, (sb.y + tb.y) / 2, { steps: 8 });
  await page.waitForTimeout(80);
  await page.mouse.move(tb.x + tb.width / 2, tb.y + tb.height / 2, { steps: 8 });
  // Land just INSIDE the top of the target row. The cdkDropList begins at row 0's top edge, so a
  // point ABOVE it is outside the container and CDK correctly refuses the drop and reverts.
  // Crossing the target's midpoint is what triggers the sort.
  await page.mouse.move(tb.x + tb.width / 2, tb.y + 2, { steps: 4 });

  // No DOM-order assertion here on purpose: CDK sorts VISUALLY, translating siblings with CSS
  // transforms, and only reorders the DOM after cdkDropListDropped fires. So there is no DOM state
  // that says "the sort has landed" mid-drag — waiting for one hangs forever. What CDK does need is
  // for its move handler to run on a separate frame from the pointer events, hence the yields.
  await page.waitForTimeout(120);
  await page.mouse.move(tb.x + tb.width / 2, tb.y + 1, { steps: 2 });
  await page.waitForTimeout(120);

  await page.mouse.up();
  // And wait for the preview to be torn down, so a following assertion sees the settled list.
  await page.locator('.cdk-drag-preview').waitFor({ state: 'detached', timeout: 5000 });
}

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
  let postFired = false;
  let postBody: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/code-reviews', (r) => {
    if (r.request().method() === 'POST') {
      postFired = true;
      postBody = r.request().postDataJSON() as Record<string, unknown>;
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

  // The pending row must come from the trigger POST — not from the GET
  // counter advancing on its own. Assert the POST fired with the right body.
  expect(postFired).toBe(true);
  expect(postBody).not.toBeNull();
  expect(postBody!['agent_id']).toBe('a1');
  expect(postBody!['repo_connector_id']).toBe('c1');
  expect(postBody!['pr_number']).toBe(5);

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

test('code-review detail: groups findings by dimension and surfaces skipped lanes', async ({
  page,
}) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/code-reviews/r1', (r) =>
    r.fulfill({ json: reviewMultiDim }),
  );

  await page.goto('/code-review/b1/r1');

  // Two dimension groups, one per ran lane, each headed by its finding count.
  const groups = page.getByTestId('dimension-group');
  await expect(groups).toHaveCount(2);
  const security = page.getByTestId('dimension-group-header').filter({ hasText: 'security' });
  const correctness = page.getByTestId('dimension-group-header').filter({ hasText: 'correctness' });
  await expect(security).toContainText('2');
  await expect(correctness).toContainText('1');

  // All three tagged findings render as rows across the groups.
  await expect(page.getByTestId('finding-row')).toHaveCount(3);

  // The scoped-out ui lane is surfaced as skipped, with its reason — never silently dropped.
  const skipped = page.getByTestId('skipped-dimensions');
  await expect(skipped).toBeVisible();
  const skippedRow = page.getByTestId('skipped-dimension-row');
  await expect(skippedRow).toHaveCount(1);
  await expect(skippedRow).toContainText('ui');
  await expect(skippedRow).toContainText('no matching files');
});

test('code-review detail a11y: findings are ARIA tables, group headers are headings, skipped is a list, pills are named', async ({
  page,
}) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/code-reviews/r1', (r) =>
    r.fulfill({ json: reviewMultiDim }),
  );

  await page.goto('/code-review/b1/r1');

  // The div-based .mf-table now carries role=table so a screen reader announces it (manyforge-0h0).
  await expect(page.getByRole('table').first()).toBeVisible();
  // Each dimension group header is a real heading element, not a bare div.
  await expect(page.locator('h4[data-testid="dimension-group-header"]')).toHaveCount(2);
  // Finding rows are exposed as table rows.
  await expect(page.getByTestId('finding-row').first()).toHaveAttribute('role', 'row');
  // The severity pill has an accessible name (dot is decorative/aria-hidden).
  await expect(page.getByRole('img', { name: 'severity error' })).toBeVisible();
  // Skipped dimensions render as a semantic list.
  await expect(page.locator('[data-testid="skipped-dimensions"] ul > li')).toHaveCount(1);
});

test('review setup a11y: every per-row control has an accessible name naming its dimension', async ({
  page,
}) => {
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/review-config', (r) =>
    r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
      },
    }),
  );
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) =>
    r.fulfill({ json: { items: [] } }),
  );

  await page.goto('/code-review/setup');
  await page.getByTestId('preset-balanced').click();
  await expect(page.getByTestId('dimension-row')).toHaveCount(4);

  // The dimensions grid is an ARIA table, and the first seeded row (Security) has individually
  // named controls — no more unlabeled checkbox/selects/buttons (manyforge-0h0).
  await expect(page.getByRole('table', { name: 'Review dimensions' })).toBeVisible();
  await expect(page.getByRole('checkbox', { name: 'Enable Security dimension' })).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Provider 1 for Security' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Save Security' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Remove Security' })).toBeVisible();
});

test('review setup: preset seeds rows, save row + config hit the API', async ({ page }) => {
  // Fallback FIRST (Playwright matches most-recently-added first) so unmocked shell calls
  // — nav badge fetches like /approvals, /connectors — return empty instead of hitting the
  // real backend, 401-ing, and tripping the refresh interceptor into a logout redirect.
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/review-config', (r) => {
    if (r.request().method() === 'PUT') {
      return r.fulfill({ json: { ...r.request().postDataJSON() } });
    }
    return r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
      },
    });
  });
  let postedDim: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) => {
    if (r.request().method() === 'POST') {
      postedDim = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ json: { id: 'new1', ...postedDim } });
    }
    return r.fulfill({ json: { items: [] } });
  });

  await page.goto('/code-review/setup');

  // Empty until a preset is applied.
  await expect(page.getByTestId('dimensions-empty')).toBeVisible();

  // Balanced preset seeds four editable rows.
  await page.getByTestId('preset-balanced').click();
  await expect(page.getByTestId('dimension-row')).toHaveCount(4);
  await expect(page.getByTestId('code-review-setup')).toContainText('Performance');

  // Save the first row → POST /review-dimensions with the built input.
  await page.getByTestId('row-save').first().click();
  await expect(page.getByTestId('setup-saved')).toBeVisible();
  expect(postedDim).not.toBeNull();
  expect(postedDim!['dimension']).toBe('security');
  expect(postedDim!['min_severity']).toBe('warning');

  // Save aggregation config → PUT /review-config.
  const putReq = page.waitForRequest(
    (req) => req.url().includes('/review-config') && req.method() === 'PUT',
  );
  await page.getByTestId('config-save').click();
  await putReq;
});

test('review setup: configure a reviewbot fallback chain (add, reorder, remove) and save it', async ({
  page,
}) => {
  // Fallback FIRST so unmocked shell/nav calls return empty instead of 401→logout.
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({
      json: {
        items: [
          {
            ...agent,
            id: 'ag1',
            name: 'LM Studio',
            provider: 'vllm',
            model: 'ornith-1.0-9b',
            max_concurrent_lanes: 1,
          },
          {
            ...agent,
            id: 'ag2',
            name: 'Cloud',
            provider: 'openrouter',
            model: 'x-ai/grok',
            max_concurrent_lanes: 4,
          },
        ],
      },
    }),
  );
  let putBody: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/review-config', (r) => {
    if (r.request().method() === 'PUT') {
      putBody = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ json: { ...putBody } });
    }
    return r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
        review_agent_chain: [],
      },
    });
  });
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) =>
    r.fulfill({ json: { items: [] } }),
  );

  await page.goto('/code-review/setup');

  // No fallback configured initially. The add-picker has an accessible name (a11y).
  await expect(page.getByTestId('chain-empty')).toBeVisible();
  await expect(
    page.getByRole('combobox', { name: 'Add a reviewbot to the fallback chain' }),
  ).toBeVisible();

  // Add LM Studio (primary) then Cloud (fallback) via the picker.
  await page.getByTestId('chain-add').selectOption('ag1');
  await page.getByTestId('chain-add').selectOption('ag2');
  await expect(page.getByTestId('chain-name-0')).toContainText('LM Studio');
  await expect(page.getByTestId('chain-name-1')).toContainText('Cloud');

  // a11y: per-row controls carry accessible names that identify their agent (manyforge-review).
  await expect(
    page.getByRole('button', { name: 'Remove LM Studio from the fallback chain' }),
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Move Cloud up' })).toBeVisible();

  // Reorder: move Cloud up → it becomes primary.
  await page.getByTestId('chain-up-1').click();
  await expect(page.getByTestId('chain-name-0')).toContainText('Cloud');

  // Remove the primary (Cloud) → only LM Studio remains.
  await page.getByTestId('chain-remove-0').click();
  await expect(page.getByTestId('chain-name-0')).toContainText('LM Studio');
  await expect(page.getByTestId('chain-name-1')).toHaveCount(0);

  // Save → the PUT body carries the chain in order.
  const putReq = page.waitForRequest(
    (req) => req.url().includes('/review-config') && req.method() === 'PUT',
  );
  await page.getByTestId('config-save').click();
  await putReq;
  expect(putBody).not.toBeNull();
  expect(putBody!['review_agent_chain']).toEqual(['ag1']);
});

test('review setup: drag-reorder the reviewbot fallback chain by its grip handle', async ({
  page,
}) => {
  // Fallback FIRST so unmocked shell/nav calls return empty instead of 401→logout.
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({
      json: {
        items: [
          {
            ...agent,
            id: 'ag1',
            name: 'LM Studio',
            provider: 'vllm',
            model: 'ornith-1.0-9b',
            max_concurrent_lanes: 1,
          },
          {
            ...agent,
            id: 'ag2',
            name: 'Cloud',
            provider: 'openrouter',
            model: 'x-ai/grok',
            max_concurrent_lanes: 4,
          },
        ],
      },
    }),
  );
  await page.route('**/api/v1/businesses/b1/review-config', (r) =>
    r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
        review_agent_chain: [],
      },
    }),
  );
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) =>
    r.fulfill({ json: { items: [] } }),
  );

  await page.goto('/code-review/setup');

  // Seed two entries: LM Studio (primary), Cloud (fallback).
  await page.getByTestId('chain-add').selectOption('ag1');
  await page.getByTestId('chain-add').selectOption('ag2');
  await expect(page.getByTestId('chain-name-0')).toContainText('LM Studio');
  await expect(page.getByTestId('chain-name-1')).toContainText('Cloud');

  // Drag Cloud (row 1) up onto row 0 by its grip handle. Angular CDK DragDrop uses pointer/mouse
  // events (NOT the HTML5 dragTo protocol), so drive page.mouse manually with incremental moves.
  await cdkDragOnto(page, 'chain-drag-1', 'chain-drag-0');

  // Cloud is now primary; LM Studio moved to fallback.
  await expect(page.getByTestId('chain-name-0')).toContainText('Cloud');
  await expect(page.getByTestId('chain-name-1')).toContainText('LM Studio');
});

test('review setup: drag a fallback to the top of the provider priority list to promote it to primary', async ({
  page,
}) => {
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/review-config', (r) =>
    r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
        review_agent_chain: [],
      },
    }),
  );
  // Seed a dimension: primary anthropic, then two fallbacks (openrouter, vLLM). In the unified
  // priority list that renders as #1 anthropic, #2 openrouter, #3 vLLM.
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) =>
    r.fulfill({
      json: {
        items: [
          {
            id: 'd1',
            dimension: 'security',
            provider: 'anthropic',
            model: 'claude-opus-4-8',
            fallback_chain: [
              { provider: 'openrouter', model: 'gpt-4o' },
              { provider: 'vllm', model: 'qwen' },
            ],
            prompt: 'Security prompt',
            scope_globs: [],
            min_severity: 'warning',
            enabled: true,
            sort_order: 1,
          },
        ],
      },
    }),
  );

  await page.goto('/code-review/setup');
  await expect(page.getByTestId('row-priority-provider-0')).toHaveValue('anthropic'); // #1 primary
  await expect(page.getByTestId('row-priority-provider-1')).toHaveValue('openrouter');
  await expect(page.getByTestId('row-priority-provider-2')).toHaveValue('vllm');

  // Drag vLLM (#3, index 2) up to the very top by its grip handle — this is the whole point: a
  // fallback becomes the primary, across the old primary↔fallback boundary. Angular CDK DragDrop
  // uses pointer/mouse events (not HTML5 dragTo), so drive page.mouse manually, from the handle.
  await cdkDragOnto(page, 'row-priority-drag-2', 'row-priority-drag-0');

  // vLLM promoted to primary (#1); anthropic and openrouter shifted down.
  await expect(page.getByTestId('row-priority-provider-0')).toHaveValue('vllm');
  await expect(page.getByTestId('row-priority-provider-1')).toHaveValue('anthropic');
  await expect(page.getByTestId('row-priority-provider-2')).toHaveValue('openrouter');
});

test('review setup: configure a per-dimension fallback chain (add, reorder, remove) and save it', async ({
  page,
}) => {
  // Fallback FIRST so unmocked shell/nav calls return empty instead of 401→logout.
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await auth(page);
  await page.addInitScript(() => localStorage.setItem('mf-current-business', 'b1'));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }),
  );
  await page.route('**/api/v1/businesses/b1/review-config', (r) =>
    r.fulfill({
      json: {
        dedupe: true,
        verify_enabled: false,
        verify_provider: '',
        verify_model: '',
        cite_rules: false,
        post_mode: 'single',
        review_agent_chain: [],
      },
    }),
  );
  // The OpenRouter model field is free-text with a live typeahead <datalist>, populated from
  // this endpoint the first time a row's (primary or fallback) provider is set to openrouter.
  await page.route('**/api/v1/businesses/b1/agents/provider-models/openrouter', (r) =>
    r.fulfill({ json: { items: [{ provider: 'openrouter', model_id: 'deepseek-v4-pro' }] } }),
  );
  let posted: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/review-dimensions', (r) => {
    if (r.request().method() === 'POST') {
      posted = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ json: { id: 'new1', ...posted } });
    }
    return r.fulfill({
      json: {
        items: [
          {
            id: 'd1',
            dimension: 'security',
            provider: 'vllm',
            model: 'ornith',
            fallback_chain: [],
            prompt: '',
            scope_globs: [],
            min_severity: 'warning',
            enabled: true,
            sort_order: 1,
          },
        ],
      },
    });
  });

  await page.goto('/code-review/setup');
  await expect(page.getByTestId('dimension-row')).toHaveCount(1);

  // The unified list starts with exactly one entry — the primary (seeded vLLM / ornith).
  await expect(page.getByTestId('row-priority-provider-0')).toHaveValue('vllm');
  await expect(page.getByTestId('row-priority-entry-1')).toHaveCount(0);

  // Add ollama (free-text model) as #2, then openrouter (typeahead) as #3.
  await page.getByTestId('row-priority-add').click();
  await page.getByTestId('row-priority-provider-1').selectOption('ollama');
  await page.getByTestId('row-priority-model-text-1').fill('llama3');

  await page.getByTestId('row-priority-add').click();
  await page.getByTestId('row-priority-provider-2').selectOption('openrouter');
  await expect(page.locator('#setup-models-openrouter option')).toHaveCount(1);
  await page.getByTestId('row-priority-model-text-2').fill('deepseek-v4-pro');

  // Promote openrouter (#3) to primary with ↑↑ → order becomes openrouter, vLLM, ollama. Assert the
  // intermediate state between clicks so each reorder settles before the next (avoids a click race).
  await page.getByTestId('row-priority-up-2').click();
  await expect(page.getByTestId('row-priority-provider-1')).toHaveValue('openrouter');
  await page.getByTestId('row-priority-up-1').click();
  await expect(page.getByTestId('row-priority-provider-0')).toHaveValue('openrouter');
  await expect(page.getByTestId('row-priority-provider-1')).toHaveValue('vllm');

  // Add a blank entry then remove it (proves Remove works; blank entries are dropped on save anyway).
  await page.getByTestId('row-priority-add').click();
  await expect(page.getByTestId('row-priority-provider-3')).toHaveValue('');
  await page.getByTestId('row-priority-remove-3').click();
  await expect(page.getByTestId('row-priority-entry-3')).toHaveCount(0);

  await page.getByTestId('row-save').click();
  await expect(page.getByTestId('setup-saved')).toBeVisible();

  expect(posted).not.toBeNull();
  // #1 (primary) → provider/model; the rest → fallback_chain in priority order.
  expect(posted!['provider']).toBe('openrouter');
  expect(posted!['model']).toBe('deepseek-v4-pro');
  expect(posted!['fallback_chain']).toEqual([
    { provider: 'vllm', model: 'ornith' },
    { provider: 'ollama', model: 'llama3' },
  ]);
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
