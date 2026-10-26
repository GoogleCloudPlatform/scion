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

import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;

// Page script policy smoke (ptone/scion#5912). Runs against the real Go hub
// binary, because the page policy and the shell come from the hub, not from
// the Vite dev server: its global setup builds the binary and the web
// assets (or, with E2E_PREBUILT=1, uses the ones already built) and starts
// a throwaway hub. Set CHROMIUM_EXECUTABLE to use a system Chromium.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 60_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in the other suites.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/page-policy', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/page-policy',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    viewport: { width: 1280, height: 800 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
});
