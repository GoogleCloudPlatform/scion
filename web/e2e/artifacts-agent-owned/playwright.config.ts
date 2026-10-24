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

// Access to an artifact owned by an agent (R1-B), end to end against the
// real Go hub binary: a local throwaway member-level test hub with the
// co-located runtime broker, restarted without development sign-in by the
// global setup (state.ts). Agent rows are declared skips where no container
// runtime is usable.
//
// The hub binary (make build) and the web assets (npm run build) must
// exist with E2E_PREBUILT=1; otherwise the global setup builds them.
//
// No traces, videos, screenshots or HARs: they would hold test identity
// session cookies. The spec writes its own evidence after the cleanup. One
// worker, no retries: the identities' web-login codes are single-use.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  // The journey sets its own timeout (launch, owner-pass wait, rows).
  timeout: 60 * 60_000,
  workers: 1,
  retries: 0,
  globalTimeout: CI ? 60 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/artifacts-agent-owned', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/artifacts-agent-owned',
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
