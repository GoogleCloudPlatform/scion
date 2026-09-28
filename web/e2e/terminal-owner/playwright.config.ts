import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.pw.ts',
  timeout: 15_000,
  workers: 1,
  forbidOnly: !!process.env.CI,
  reporter: 'list',
  outputDir: '../../test-results/terminal-owner',
  use: {
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
  },
});
