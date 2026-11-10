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

const CI = !!process.env.CI;

// Brokers area gate (broker UI Option A, slice A1). No hub, agents or
// credentials: every hub request is mocked via page.route in mock-api.ts,
// and each test pins the hub.flat_runtime_brokers experiment through
// window.__SCION_FEATURES__ before the app loads.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 30_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in e2e/chat-mobile and e2e/test-hub-banner: one
  // retry for a timing blip on a shared runner (still reported as flaky), a
  // global timeout below the 20m job timeout so the report is still written,
  // and inline annotations plus an HTML report for the uploaded artifact.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/broker-area', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/broker-area',
  use: {
    // A trace of the retried attempt in CI.
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4548',
    viewport: { width: 1100, height: 700 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4548',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4548/',
    reuseExistingServer: false,
    // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
    timeout: CI ? 120_000 : 60_000,
  },
});
