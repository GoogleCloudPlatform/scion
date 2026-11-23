// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { fileURLToPath } from 'node:url';

import { defineConfig } from '@playwright/test';

import { chromiumLaunchOptions } from '../../playwright-chromium.js';

const CI = !!process.env.CI;

// Mocked-hub Playwright suite for Admin Maintenance preflight findings,
// run banners, and force re-run confirmation flow. No Go binary required.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 30_000,
  workers: 1,
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/maintenance-preflight', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/maintenance-preflight',
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4547',
    viewport: { width: 1200, height: 800 },
    launchOptions: chromiumLaunchOptions(),
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4547',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4547/',
    reuseExistingServer: false,
    timeout: CI ? 120_000 : 60_000,
  },
});
