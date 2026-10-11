import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;
const WEB_DIR = new URL('../../', import.meta.url).pathname;
const BASE_PATH_PORT = 4533;

export default defineConfig({
  testDir: '.',
  // Two projects share the settings below. 'default' runs the suite against
  // the dev server at the root path. 'base-path' runs base.pw.ts against a
  // second dev server started with PROXY_BASE_PATH=/tw/, so the app is served
  // under a deployment base path (ptone/scion#4202).
  projects: [
    {
      name: 'default',
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
    },
    {
      name: 'base-path',
      testMatch: 'base.pw.ts',
      use: {
        baseURL: `http://127.0.0.1:${BASE_PATH_PORT}`,
        // Playwright's default viewport, which these tests have always used.
        viewport: { width: 1280, height: 720 },
      },
    },
  ],
  workers: 1,
  forbidOnly: CI,
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
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox'],
    },
  },
  webServer: [
    {
      command: 'npm run dev -- --host 127.0.0.1 --port 4532',
      cwd: WEB_DIR,
      url: 'http://127.0.0.1:4532/',
      reuseExistingServer: false,
      // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
      timeout: CI ? 120_000 : 60_000,
    },
    {
      // Serves the 'base-path' project.
      command: `npm run dev -- --host 127.0.0.1 --port ${BASE_PATH_PORT}`,
      cwd: WEB_DIR,
      env: { PROXY_BASE_PATH: '/tw/' },
      url: `http://127.0.0.1:${BASE_PATH_PORT}/tw/`,
      reuseExistingServer: false,
      timeout: CI ? 120_000 : 60_000,
    },
  ],
});
