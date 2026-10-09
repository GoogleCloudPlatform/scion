import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: '.',
  testMatch: '*.pw.ts',
  timeout: 15_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in e2e/chat-mobile: one
  // retry for a timing blip on a shared runner (still reported as flaky), a
  // global timeout below the 20m job timeout so the report is still written,
  // and inline annotations plus an HTML report for the uploaded artifact.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/terminal-owner', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/terminal-owner',
  use: {
    // A trace of the retried attempt in CI.
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4517',
    launchOptions: process.env.TERMINAL_OWNER_CHROMIUM
      ? { executablePath: process.env.TERMINAL_OWNER_CHROMIUM }
      : {},
  },
  webServer: {
    command: 'node e2e/terminal-owner/server.mjs',
    cwd: '../..',
    url: 'http://127.0.0.1:4517',
    reuseExistingServer: false,
    // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
    timeout: CI ? 120_000 : 60_000,
  },
});
