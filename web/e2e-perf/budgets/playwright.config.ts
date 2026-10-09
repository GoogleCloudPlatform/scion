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

import { fileURLToPath } from 'node:url';

import { defineConfig } from '@playwright/test';

/**
 * Browser counter budgets for the project agent views (grid, list, graph).
 *
 * Follows the e2e/chat-mobile pattern: the Vite dev server serves the app
 * and every API request is answered by `page.route` mocks (mock-api.ts), so
 * no hub or Go build is needed. The responses come from a deterministic
 * 100-agent generator (fixture.mjs) whose field names are checked against
 * the hub's real responses (fixture-schema.json). Chromium only; one
 * worker, so loads do not compete for CPU. Baselines and margins are in
 * budgets.pw.ts.
 */

const CI = !!process.env.CI;
const PORT = 4537;

export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 120_000,
  workers: 1,
  forbidOnly: CI,
  // No retries: the counters are deterministic except long tasks, which
  // already take the median of several loads.
  retries: 0,
  globalTimeout: CI ? 8 * 60_000 : 0,
  outputDir: '../../test-results/perf-budgets',
  reporter: CI ? [['github'], ['list']] : 'list',
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    browserName: 'chromium',
    viewport: { width: 1440, height: 1000 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  webServer: {
    command: `npm run dev -- --host 127.0.0.1 --port ${PORT} --strictPort`,
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: `http://127.0.0.1:${PORT}/`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
});
