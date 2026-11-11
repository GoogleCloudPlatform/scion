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

import { parseTimeoutMs } from './journey-lib.js';

const CI = !!process.env.CI;

// Async agent create from the web form, end to end against the real Go hub
// binary. By default the global setup starts a throwaway member-level test
// hub with the co-located runtime broker and the hub.async_agent_launch
// experiment on (../harness/hub.ts); E2E_TARGET_URL points the journey at an existing
// non-IAP hub instead (see state.ts). The spec acts as a member test
// identity. Checks that need a real launch skip, with the reason, where no
// container runtime is usable.
//
// The hub binary (make build) and the web assets (npm run build) must
// exist with E2E_PREBUILT=1; otherwise the global setup builds them.
//
// No traces, videos or HARs: they would hold the test identity's session
// cookie and would be written before the identity is deleted. The spec
// writes its own evidence (DOM timeline, SSE launch payloads, request log;
// no cookies or tokens) after the identity is deleted. One worker, no
// retries: the tests share one identity and project in order.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  // One launch plus the panel, delete and cleanup waits around it.
  timeout:
    parseTimeoutMs(process.env.E2E_LAUNCH_TIMEOUT_MS, 300_000, 'E2E_LAUNCH_TIMEOUT_MS') + 300_000,
  workers: 1,
  retries: 0,
  globalTimeout: CI ? 30 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/async-create', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/async-create',
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
