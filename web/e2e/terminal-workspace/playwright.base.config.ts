import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: 'base.pw.ts',
  workers: 1,
  timeout: 30000,
  use: {
    baseURL: 'http://127.0.0.1:4533',
    launchOptions: { executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] },
  },
  webServer: {
    command: 'PROXY_BASE_PATH=/tw/ npm run dev -- --host 127.0.0.1 --port 4533',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4533/tw/',
    reuseExistingServer: false,
  },
});
