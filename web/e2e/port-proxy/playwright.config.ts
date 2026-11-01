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

// Port proxy journeys (ptone/scion#5568), end to end against the real Go
// hub binary. The global setup starts a throwaway member-level test hub
// (../harness/hub.ts) with hub.conduit on, the fixture app server and the
// fake agent helper (hack/e2e/fakeagent), which serves the fixture port
// from one agent per transport. A member test identity opens it in a
// browser context with its session cookie only.
//
// The hub binary (make build), the web assets (npm run build) and the
// helper (go build -o build/e2e-fakeagent ./hack/e2e/fakeagent) must exist
// with E2E_PREBUILT=1 (as in CI); otherwise the global setup builds them.
//
// No traces, videos or screenshots: they would hold the session cookie.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  // The page view journeys run under playwright.view.config.ts.
  testIgnore: '**/port-view.pw.ts',
  timeout: 90_000,
  workers: 1,
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/port-proxy', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/port-proxy',
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
