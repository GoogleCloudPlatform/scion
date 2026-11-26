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

// Decision retention UI suite (ptone/scion#8984, PR-3 #9351): mocked hub
// verifying the Admin > Server Config Decision retention panel and the Project
// and Profile Decisions Made notices in light, dark, and 390px mobile viewports.
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
        ['html', { outputFolder: '../../playwright-report/decision-retention', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/decision-retention',
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4548',
    viewport: { width: 1100, height: 800 },
    launchOptions: chromiumLaunchOptions(),
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4548',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4548/',
    reuseExistingServer: false,
    timeout: CI ? 120_000 : 60_000,
  },
});
