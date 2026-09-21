import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.pw.ts',
  timeout: 15_000,
  workers: 1,
  reporter: 'list',
  outputDir: '../../test-results/terminal-coordinator',
  use: {
    baseURL: 'http://127.0.0.1:4519',
    launchOptions: { executablePath: process.env.TERMINAL_OWNER_CHROMIUM || '/usr/bin/chromium' },
  },
  webServer: {
    command: 'node e2e/terminal-coordinator/server.mjs',
    cwd: '../..',
    url: 'http://127.0.0.1:4519',
    reuseExistingServer: false,
  },
});
