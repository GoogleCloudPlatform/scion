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

import { defineConfig, devices } from '@playwright/test';
import type { Project } from '@playwright/test';

const CI = !!process.env.CI;

// No failure page snapshot (error-context.md) either: it records page
// content. Inherited by the workers.
process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1';

// Graphical review editor journeys (ptone/scion#5913), against the real Go
// hub binary with the page script policy: its global setup builds the
// binary and the web assets (or, with E2E_PREBUILT=1, uses the ones
// already built) and starts a throwaway dev-auth hub.
//
// One project per engine. E2E_BROWSERS picks a subset (for example
// "chromium" or "firefox,webkit"); the default runs all three. Set
// CHROMIUM_EXECUTABLE, FIREFOX_EXECUTABLE or WEBKIT_EXECUTABLE to run a
// given browser binary; otherwise Playwright's own (PLAYWRIGHT_BROWSERS_PATH)
// are used.
//
// No traces, videos, screenshots or HARs: they would hold the session
// cookie.
const ENGINES: Project[] = [
  {
    name: 'chromium',
    use: {
      ...devices['Desktop Chrome'],
      launchOptions: {
        ...(process.env.CHROMIUM_EXECUTABLE
          ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
          : {}),
        args: ['--no-sandbox', '--disable-setuid-sandbox'],
      },
    },
  },
  {
    name: 'firefox',
    use: {
      ...devices['Desktop Firefox'],
      launchOptions: process.env.FIREFOX_EXECUTABLE
        ? { executablePath: process.env.FIREFOX_EXECUTABLE }
        : {},
    },
  },
  {
    name: 'webkit',
    use: {
      ...devices['Desktop Safari'],
      launchOptions: process.env.WEBKIT_EXECUTABLE
        ? { executablePath: process.env.WEBKIT_EXECUTABLE }
        : {},
    },
  },
];

const wanted = (process.env.E2E_BROWSERS ?? 'chromium,firefox,webkit')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);

export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 120_000,
  workers: 1,
  fullyParallel: false,
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 30 * 60_000 : 0,
  reporter: CI ? [['github'], ['list']] : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/artifacts-review',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  use: {
    trace: 'off',
    video: 'off',
    screenshot: 'off',
    viewport: { width: 1280, height: 900 },
    navigationTimeout: 30_000,
    actionTimeout: 15_000,
  },
  projects: ENGINES.filter((p) => wanted.includes(p.name!)),
});
