import { defineConfig, devices } from '@playwright/test';

const isCI = Boolean(process.env.CI);
const baseURL = 'http://localhost:5173';

/**
 * Every spec in e2e/ runs once per viewport project (AGENTS.md §1.4): desktop
 * 1280x800 (sidebar layout) and mobile 390x844 (bottom tab bar, touch).
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: isCI,
  retries: isCI ? 2 : 0,
  ...(isCI ? { workers: 1 } : {}),
  reporter: isCI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL,
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'desktop',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 800 },
      },
    },
    {
      name: 'mobile',
      use: {
        ...devices['Pixel 7'],
        viewport: { width: 390, height: 844 },
      },
    },
  ],
  webServer: {
    // Starts compose PostgreSQL, seeds per-project fixture users at E2E_DEV_NOW,
    // and runs polling pulse-worker + pulse-api (dev login + pinned clock) + Vite.
    // Never reuse a standalone `npm run dev`, which lacks the E2E backend and fixtures.
    command: 'npm run e2e:stack',
    url: `${baseURL}/api/v1/health`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
});
