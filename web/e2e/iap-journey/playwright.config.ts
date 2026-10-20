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

// IAP browser journey (ptone/scion#4240, X5). Operator-run only against a
// live IAP-fronted test hub (README.md); it skips without IAP_JOURNEY=1 and
// its environment, and no CI job runs it. No trace, screenshot, video or
// storage state: the journey writes no file that could hold a token or a
// session cookie.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 180_000,
  workers: 1,
  retries: 0,
  reporter: 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/iap-journey',
  use: {
    trace: 'off',
    screenshot: 'off',
    video: 'off',
    viewport: { width: 1280, height: 800 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
});
