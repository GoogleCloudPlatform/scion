import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: [
    'workspace.pw.ts',
    'reconnect.pw.ts',
    'ownership.pw.ts',
    'dm-buttons.pw.ts',
    'toast-lifecycle.pw.ts',
    'url-layout.pw.ts',
  ],
  workers: 1,
  timeout: 30000,
  use: {
    baseURL: 'http://127.0.0.1:4532',
    viewport: { width: 1100, height: 700 },
    launchOptions: { executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4532',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4532/',
    reuseExistingServer: false,
  },
});
