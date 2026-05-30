import { expect, test } from '@playwright/test';

// US1 SPA regression. The full happy path (signup -> verify -> create business)
// is covered end-to-end at the API layer by the Go HTTP integration test
// (internal/account: TestUS1_HTTPFlow); these specs cover the SPA's routing,
// guard, forms, and error handling against the running stack.

test('unauthenticated visit is redirected to login (auth guard)', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole('heading', { name: 'Welcome back' })).toBeVisible();
});

test('signup page renders and links back to login', async ({ page }) => {
  await page.goto('/signup');
  await expect(page.getByRole('heading', { name: 'Create your account' })).toBeVisible();
  await page.getByRole('textbox', { name: 'Email' }).fill('x@y.test');
  await page.getByRole('link', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/login$/);
});

test('invalid login surfaces a generic error (no oracle)', async ({ page }) => {
  await page.goto('/login');
  await page.locator('input[name="email"]').fill('nobody@manyforge.test');
  await page.locator('input[name="password"]').fill('definitely-wrong');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByText('Invalid email or password.')).toBeVisible();
  await expect(page).toHaveURL(/\/login$/);
});

test('dashboard is protected', async ({ page }) => {
  await page.goto('/dashboard');
  await expect(page).toHaveURL(/\/login$/);
});
