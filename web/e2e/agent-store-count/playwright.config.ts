import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: ['request-count.pw.ts'],
  workers: 1,
  timeout: 60000,
  use: {
    baseURL: 'http://127.0.0.1:4534',
    viewport: { width: 1100, height: 700 },
    launchOptions: { executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4534',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4534/',
    reuseExistingServer: false,
  },
});
