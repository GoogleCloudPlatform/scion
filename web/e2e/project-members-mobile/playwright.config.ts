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

// Project settings > Members responsive layout suite (ptone/scion#9571):
// mocked hub verifying that on phone widths the Members header stacks and
// header + row actions remain unclipped, while desktop layout is unchanged.
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
        ['html', { outputFolder: '../../playwright-report/project-members-mobile', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/project-members-mobile',
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4556',
    viewport: { width: 1100, height: 800 },
    launchOptions: chromiumLaunchOptions(),
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4556',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4556/',
    reuseExistingServer: false,
    timeout: CI ? 120_000 : 60_000,
  },
});
