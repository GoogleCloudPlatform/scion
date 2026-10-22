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

const CI = !!process.env.CI;

// The IAP driver hook of the e2e harness (ptone/scion#4240, phase WS2):
// installIapDriver() in ../harness/iap-driver.ts. No hub, no IAP and no
// credentials: the spec starts two local HTTP servers (the "hub" origin and
// another origin) that record the headers they receive, and a synthetic
// token callback.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 30_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in the other suites.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 10 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/iap-driver', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/iap-driver',
  use: {
    // Traces would record the synthetic header values; none are needed.
    trace: 'off',
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
});
