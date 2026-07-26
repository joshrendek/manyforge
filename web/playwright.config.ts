import { defineConfig, devices } from '@playwright/test';

// The suite starts its own SPA server; override the port when :4300 is already in use locally.
const PORT = Number(process.env.E2E_PORT ?? 4300);

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  retries: process.env.CI ? 1 : 0,
  // Fail the run if a spec is left focused; in CI that would silently skip everything else.
  forbidOnly: !!process.env.CI,
  use: {
    baseURL: process.env.E2E_BASE_URL ?? `http://localhost:${PORT}`,
    trace: 'on-first-retry',
  },
  // Skipped when E2E_BASE_URL points somewhere else (e.g. running against a deployed environment).
  webServer: process.env.E2E_BASE_URL
    ? undefined
    : {
        command: `npx ng serve --port ${PORT}`,
        url: `http://localhost:${PORT}`,
        // Locally, reuse a dev server that is already up rather than fighting it for the port.
        reuseExistingServer: !process.env.CI,
        timeout: 180_000,
        stdout: 'ignore',
        stderr: 'pipe',
      },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
