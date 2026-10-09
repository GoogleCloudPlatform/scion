import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: '.',
  // Besides the terminal entry points, this suite covers the graph views'
  // "Jump to agent" palette and the quick message dialog's "Open agent DM"
  // button, which share their fixtures.
  testMatch: ['entrypoints.pw.ts', 'graph-palette.pw.ts', 'quick-message-dm.pw.ts'],
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
        ['html', { outputFolder: '../../playwright-report/terminal-entrypoints', open: 'never' }],
      ]
    : 'list',
  timeout: 30000,
  use: {
    // A trace of the retried attempt in CI.
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4533',
    viewport: { width: 1100, height: 700 },
    launchOptions: {
      executablePath: process.env.CHROMIUM_EXECUTABLE || '/usr/bin/chromium',
      args: ['--no-sandbox'],
    },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4533',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4533/',
    reuseExistingServer: false,
    // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
    timeout: CI ? 120_000 : 60_000,
  },
});
