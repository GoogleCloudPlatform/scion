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

const CI = !!process.env.CI;

// Isolated fixture for the native chat quick command palette. Mounts the
// real scion-page-chat, scion-quick-palette and
// scion-terminal-pane components with endpoint-shaped request interception —
// no live Hub, no project agents or credentials. See e2e/terminal-pane for
// the sibling pattern this follows.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 20_000,
  workers: 1,
  // CI-only settings (local runs are unchanged), as in e2e/chat-mobile: one
  // retry for a timing blip on a shared runner (still reported as flaky), a
  // global timeout below the 20m job timeout so the report is still written,
  // and inline annotations plus an HTML report for the uploaded artifact.
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/chat-palette', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/chat-palette',
  use: {
    // A trace of the retried attempt in CI.
    trace: CI ? 'on-first-retry' : 'off',
    baseURL: 'http://127.0.0.1:4534',
    viewport: { width: 1200, height: 800 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  // WebKit, the engine iOS Safari uses, may not be installed in every
  // sandbox, so its project runs only when explicitly requested, and only
  // the touch keyboard checks. Linux WebKit shows no on-screen keyboard:
  // they check where focus is during and after the tap.
  ...(process.env.PW_WEBKIT === '1'
    ? {
        projects: [
          { name: 'chromium' },
          {
            name: 'webkit-iphone',
            testMatch: 'touch-keyboard.pw.ts',
            use: { ...devices['iPhone 13'], launchOptions: {} },
          },
        ],
      }
    : {}),
  webServer: {
    command: 'node e2e/chat-palette/serve.mjs',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4534/e2e/chat-palette/fixture.html',
    reuseExistingServer: false,
    // A cold start on a CI runner can exceed Playwright's 60s default (kept locally).
    timeout: CI ? 120_000 : 60_000,
  },
});
