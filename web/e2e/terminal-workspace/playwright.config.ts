import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: '.',
  testMatch: [
    'workspace.pw.ts',
    'reconnect.pw.ts',
    'ownership.pw.ts',
    'dm-buttons.pw.ts',
    'toast-lifecycle.pw.ts',
    'url-layout.pw.ts',
    'url-nav-guard.pw.ts',
    'persistence.pw.ts',
    'jump-to-agent.pw.ts',
  ],
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
        ['html', { outputFolder: '../../playwright-report/terminal-workspace', open: 'never' }],
      ]
    : 'list',
  timeout: 30000,
  use: {
    // A trace of the retried attempt in CI.
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4532',
    viewport: { width: 1100, height: 700 },
    launchOptions: {
      executablePath: process.env.CHROMIUM_EXECUTABLE || '/usr/bin/chromium',
      args: ['--no-sandbox'],
    },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4532',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4532/',
    reuseExistingServer: false,
    // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
    timeout: CI ? 120_000 : 60_000,
  },
});
