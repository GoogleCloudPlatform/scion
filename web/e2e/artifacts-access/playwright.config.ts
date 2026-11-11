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

// Artifact access journey (R1-A), end to end against the real Go hub
// binary. By default the global setup starts a throwaway member-level test
// hub (../harness/hub.ts) with no broker; E2E_TARGET_URL points the journey
// at an existing non-IAP hub instead (see state.ts). Five member test
// identities act in five browser contexts.
//
// The hub binary (make build) and the web assets (npm run build) must
// exist with E2E_PREBUILT=1; otherwise the global setup builds them.
//
// No traces, videos, screenshots or HARs: they would hold a test
// identity's session cookie and would be written before the identity is
// deleted. The spec writes its own evidence (UI observations, API
// statuses, request logs; no cookies, tokens or codes) after the cleanup.
// One worker, no retries: the rows share identities, projects and one
// artifact in order.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 10 * 60_000,
  workers: 1,
  retries: 0,
  globalTimeout: CI ? 30 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/artifacts-access', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/artifacts-access',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  use: {
    trace: 'off',
    video: 'off',
    screenshot: 'off',
    viewport: { width: 1280, height: 800 },
    navigationTimeout: 30_000,
    actionTimeout: 15_000,
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
});
