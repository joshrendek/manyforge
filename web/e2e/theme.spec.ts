import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };

test('theme toggle flips data-theme and persists across reload', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));

  await page.goto('/dashboard');
  const html = page.locator('html');
  const before = await html.getAttribute('data-theme');
  await page.getByTestId('theme-toggle').click();
  const after = await html.getAttribute('data-theme');
  expect(after).not.toBe(before);

  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', after!);
});

test('dashboard renders without console errors in both themes', async ({ page }) => {
  const errors: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));

  for (const theme of ['light', 'dark']) {
    await page.addInitScript((t) => localStorage.setItem('mf-theme', t), theme);
    await page.goto('/dashboard');
    await expect(page.getByTestId('app-sidebar')).toBeVisible();
  }
  expect(errors).toEqual([]);
});
