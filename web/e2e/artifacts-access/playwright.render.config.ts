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

// Artifact render journey (artifacts-render.pw.ts), end to end against the
// real Go hub binary. It shares the access journey's global setup (a
// throwaway member-level test hub with no broker, or E2E_TARGET_URL) and
// output dir.
//
// It has its own config for one reason: the reporters. The isolation rows
// must not reveal which check failed. Their verdict is one generic
// assertion, but the HTML reporter also renders every inner browser action
// and its error. So this config has no HTML reporter, only list (and
// github in CI), which print the generic test errors alone. The journey
// skips itself unless it runs under this config (E2E_R2_RENDER_CONFIG is
// set here and inherited by the workers).
process.env.E2E_R2_RENDER_CONFIG = '1';

// No traces, videos, screenshots or HARs: they would hold a test
// identity's session cookie and the view capabilities, and would be
// written before the identities are deleted.
export default defineConfig({
  testDir: '.',
  testMatch: '**/artifacts-render.pw.ts',
  timeout: 20 * 60_000,
  workers: 1,
  retries: 0,
  globalTimeout: CI ? 30 * 60_000 : 0,
  reporter: CI ? [['github'], ['list']] : 'list',
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
