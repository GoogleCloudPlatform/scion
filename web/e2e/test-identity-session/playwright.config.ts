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

// Test identity browser session smoke (ptone/scion#4240, WS3). Unlike the
// mocked suites, this one runs against the real Go hub binary: its global
// setup starts a throwaway member-level test hub (hub-issued test
// identities on, test-login off, the test infrastructure marker written
// into a temp database) through the shared harness in ../harness/hub.ts.
// The spec issues a test identity and mints a web-login code with the CLI,
// redeems the code in a browser context and runs a UI journey as the
// identity.
//
// The hub binary (make build) and the web assets (npm run build) must
// exist; with E2E_PREBUILT=1 (as in CI) the global setup does not build
// them, otherwise it does.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 90_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in the other suites.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/test-identity-session', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/test-identity-session',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  use: {
    // A trace of the retried attempt in CI. The redeem pauses trace
    // recording, so no trace holds the code or the session Set-Cookie.
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
