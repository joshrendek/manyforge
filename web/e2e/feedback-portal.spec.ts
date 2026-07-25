import { expect, test } from '@playwright/test';

// The public feedback portal (/p/:key) is UNAUTHENTICATED — no login, no token. These specs
// mock only the public ingress endpoints and never set mf_access, so they also prove the
// portal renders standalone (no admin sidebar) and never bounces to /login.

const key = 'fbk_demo';
const post = {
  id: 'p1',
  title: 'Add dark mode',
  body: 'Please add a dark theme',
  status: 'planned',
  vote_count: 9,
  created_at: '2026-07-02T00:00:00Z',
  viewer_voted: false,
  identity_verified: false,
};

// The list GET now carries ?voter_identity=<device-id> (a stable id persisted to localStorage
// by the portal, not a secret). Match with or without a trailing query string — but anchored at
// the end so it doesn't also swallow the /posts/:id/votes sub-path.
const postsPattern = new RegExp(`/api/v1/feedback/public/${key}/posts(\\?.*)?$`);

test('portal: renders public posts standalone (no admin shell)', async ({ page }) => {
  await page.route(postsPattern, (r) => r.fulfill({ json: { items: [post] } }));
  await page.goto(`/p/${key}`);
  await expect(page.getByTestId('portal')).toBeVisible();
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0); // no admin chrome
  await expect(page.getByTestId('portal-post-title')).toContainText('Add dark mode');
  await expect(page.getByTestId('portal-vote-count')).toContainText('9');
});

test('portal: unknown/revoked key shows a uniform unavailable state (no /login bounce)', async ({
  page,
}) => {
  await page.route(postsPattern, (r) =>
    r.fulfill({ status: 401, json: { code: 'UNAUTHORIZED', message: 'unauthorized' } }),
  );
  await page.goto(`/p/${key}`);
  await expect(page.getByTestId('portal-unavailable')).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/p/${key}$`)); // stayed put — not redirected to /login
});

test('portal: submit a new idea (anonymous)', async ({ page }) => {
  let submitBody: Record<string, unknown> | null = null;
  await page.route(postsPattern, (r) => {
    if (r.request().method() === 'POST') {
      submitBody = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({
        json: { id: 'p2', title: submitBody['title'], status: 'open', vote_count: 0 },
      });
    }
    return r.fulfill({ json: { items: [post] } });
  });
  await page.goto(`/p/${key}`);
  await page.getByTestId('portal-title-input').fill('Native iPad app');
  await page.getByTestId('portal-submit-btn').click();
  await expect.poll(() => submitBody).not.toBeNull();
  expect(submitBody!['title']).toBe('Native iPad app');
  expect(typeof submitBody!['author_identity']).toBe('string');
});

test('portal: upvote a post', async ({ page }) => {
  await page.route(`**/api/v1/feedback/public/${key}/posts/p1/votes`, (r) =>
    r.fulfill({ json: { voted: true, vote_count: 10 } }),
  );
  await page.route(postsPattern, (r) => r.fulfill({ json: { items: [post] } }));
  await page.goto(`/p/${key}`);
  await page.getByTestId('portal-upvote').click();
  await expect(page.getByTestId('portal-vote-count')).toContainText('10');
});

test('portal: vote persists as "voted" after reload (server truth via viewer_voted)', async ({
  page,
}) => {
  // The mock backend: vote_count/viewer_voted flip once a vote is recorded, mirroring the real
  // server (GET …/posts?voter_identity=<id> → viewer_voted: true once that identity has voted).
  let voted = false;
  const seenVoterIdentities = new Set<string>();

  await page.route(postsPattern, (r) => {
    const vid = new URL(r.request().url()).searchParams.get('voter_identity');
    if (vid) seenVoterIdentities.add(vid);
    return r.fulfill({
      json: { items: [{ ...post, vote_count: voted ? 10 : 9, viewer_voted: voted }] },
    });
  });
  await page.route(`**/api/v1/feedback/public/${key}/posts/p1/votes`, (r) => {
    const body = r.request().postDataJSON() as Record<string, unknown>;
    seenVoterIdentities.add(body['voter_identity'] as string);
    voted = true;
    return r.fulfill({ json: { voted: true, vote_count: 10 } });
  });

  await page.goto(`/p/${key}`);
  await expect(page.getByTestId('portal-post-title')).toContainText('Add dark mode');
  await expect(page.getByTestId('portal-upvote')).not.toHaveClass(/voted/);

  await page.getByTestId('portal-upvote').click();
  await expect(page.getByTestId('portal-vote-count')).toContainText('10');
  await expect(page.getByTestId('portal-upvote')).toHaveClass(/voted/);

  // Reload: a brand new page load re-fetches the list from scratch. The button must still show
  // "voted" from the server's viewer_voted — proving it's server truth, not just the optimistic
  // client-side state set by the click above (which a fresh load wouldn't otherwise carry).
  await page.reload();
  await expect(page.getByTestId('portal-post-title')).toContainText('Add dark mode');
  await expect(page.getByTestId('portal-upvote')).toHaveClass(/voted/);

  // Exactly one device id was used for the vote and both list loads — the portal's device id
  // is stable/persisted (localStorage), not regenerated per request or per reload.
  expect(seenVoterIdentities.size).toBe(1);
});
