import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  // Besides the terminal entry points, this suite covers the graph views'
  // "Jump to agent" palette and the quick message dialog's "Open agent DM"
  // button, which share their fixtures.
  testMatch: ['entrypoints.pw.ts', 'graph-palette.pw.ts', 'quick-message-dm.pw.ts'],
  workers: 1,
  timeout: 30000,
  use: {
    baseURL: 'http://127.0.0.1:4533',
    viewport: { width: 1100, height: 700 },
    launchOptions: { executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4533',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4533/',
    reuseExistingServer: false,
  },
});
