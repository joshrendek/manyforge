import { defineConfig, devices } from '@playwright/test';

// E2E for the ManyForge SPA.
//
// The suite is self-contained: every spec intercepts `**/api/**`, so no backend is required — only
// the SPA has to be served. Playwright therefore starts the dev server itself. Before this, the
// config merely ASSUMED something was already listening on :4300, which meant the suite could only
// be run by hand, which meant CI never ran it at all (manyforge-m07j). A test suite nothing starts
// is a test suite nothing enforces.
// Port is overridable so a run cannot collide with a long-lived local dev server. CI gets its own
// port by default; developers keep :4300.
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
