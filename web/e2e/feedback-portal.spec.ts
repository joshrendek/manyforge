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
};

test('portal: renders public posts standalone (no admin shell)', async ({ page }) => {
  await page.route(`**/api/v1/feedback/public/${key}/posts`, (r) =>
    r.fulfill({ json: { items: [post] } }),
  );
  await page.goto(`/p/${key}`);
  await expect(page.getByTestId('portal')).toBeVisible();
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0); // no admin chrome
  await expect(page.getByTestId('portal-post-title')).toContainText('Add dark mode');
  await expect(page.getByTestId('portal-vote-count')).toContainText('9');
});

test('portal: unknown/revoked key shows a uniform unavailable state (no /login bounce)', async ({
  page,
}) => {
  await page.route(`**/api/v1/feedback/public/${key}/posts`, (r) =>
    r.fulfill({ status: 401, json: { code: 'UNAUTHORIZED', message: 'unauthorized' } }),
  );
  await page.goto(`/p/${key}`);
  await expect(page.getByTestId('portal-unavailable')).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/p/${key}$`)); // stayed put — not redirected to /login
});

test('portal: submit a new idea (anonymous)', async ({ page }) => {
  let submitBody: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/feedback/public/${key}/posts`, (r) => {
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
  await page.route(`**/api/v1/feedback/public/${key}/posts`, (r) =>
    r.fulfill({ json: { items: [post] } }),
  );
  await page.goto(`/p/${key}`);
  await page.getByTestId('portal-upvote').click();
  await expect(page.getByTestId('portal-vote-count')).toContainText('10');
});
