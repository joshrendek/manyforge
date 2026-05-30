import { defineConfig, devices } from '@playwright/test';

// E2E for the ManyForge SPA. Assumes the API (proxied at /api) and the Angular
// dev server are running on http://localhost:4300 (see web/README / make targets).
export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  retries: process.env.CI ? 1 : 0,
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'http://localhost:4300',
    trace: 'on-first-retry',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
