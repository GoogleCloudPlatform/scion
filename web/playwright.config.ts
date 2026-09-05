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

import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright E2E configuration for the Scion web frontend.
 *
 * Tests run against a real Go hub binary with an ephemeral SQLite store.
 * The hub is started by the global-setup script and torn down by global-teardown.
 *
 * IMPORTANT: Navigation waits use 'domcontentloaded', never 'networkidle',
 * because SSE connections stay open indefinitely.
 */
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',

  /* Maximum time one test can run */
  timeout: 60_000,

  /* Fail the build on CI if test.only is left in source */
  forbidOnly: !!process.env.CI,

  /* Retry on CI to reduce flake noise */
  retries: process.env.CI ? 1 : 0,

  /* Limit parallel workers — the hub is a single process */
  workers: 1,

  /* Reporter: list for local, HTML for CI */
  reporter: process.env.CI ? 'html' : 'list',

  /* Global setup/teardown: builds & starts the real hub */
  globalSetup: './e2e/harness/global-setup.ts',
  globalTeardown: './e2e/harness/global-teardown.ts',

  use: {
    /* Base URL comes from the global setup (written to env) */
    baseURL: process.env.E2E_BASE_URL || 'http://127.0.0.1:4510',

    /* CRITICAL: never use 'networkidle' — SSE stays connected */
    navigationTimeout: 30_000,
    actionTimeout: 15_000,

    /* Trace on first retry for debugging flakes */
    trace: 'on-first-retry',

    /* Screenshots and video only on failure */
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    {
      name: 'desktop-chromium',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1440, height: 900 },
      },
    },
    {
      name: 'mobile-chromium',
      use: {
        ...devices['Pixel 5'],
        viewport: { width: 390, height: 844 },
      },
    },
  ],

  /* Artifacts go to gitignored directories */
  outputDir: './test-results',
});
