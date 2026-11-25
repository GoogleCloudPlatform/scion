/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { defineConfig } from '@playwright/test';

import { chromiumLaunchOptions } from '../../playwright-chromium.js';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: '.',
  testMatch: ['agent-graph.pw.ts'],
  workers: 1,
  forbidOnly: CI,
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/agent-graph', open: 'never' }],
      ]
    : 'list',
  timeout: 60000,
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4553',
    viewport: { width: 1280, height: 860 },
    launchOptions: chromiumLaunchOptions(),
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4553',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4553/',
    reuseExistingServer: false,
    timeout: CI ? 120_000 : 60_000,
  },
});
