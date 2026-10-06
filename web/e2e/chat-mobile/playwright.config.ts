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

import { defineConfig, devices, type PlaywrightTestConfig } from '@playwright/test';

/**
 * Mobile-emulation guard suite for the chat layout.
 *
 * Deliberately separate from the Hub E2E harness, following the `e2e/chat/`
 * pattern: no project agents or credentials — everything the chat page and
 * its children fetch is mocked via `page.route` in `mock-api.ts`. CDP touch
 * gestures (`helpers.ts`) are Chromium-only, so the optional WebKit project
 * runs only the `@static`-tagged tests (no gestures) when `PW_WEBKIT=1` —
 * WebKit may not be installed in every sandbox.
 */

const CHROMIUM_LAUNCH_OPTIONS = {
  ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}),
  args: ['--no-sandbox', '--disable-setuid-sandbox'],
};

type ProjectConfig = NonNullable<PlaywrightTestConfig['projects']>[number];

/** A Chromium mobile-emulation project at a given CSS viewport size. */
function mobileProject(name: string, width: number, height: number): ProjectConfig {
  return {
    name,
    use: {
      browserName: 'chromium' as const,
      viewport: { width, height },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 3,
      launchOptions: CHROMIUM_LAUNCH_OPTIONS,
    },
  };
}

const projects: PlaywrightTestConfig['projects'] = [
  mobileProject('chromium-320', 320, 568),
  mobileProject('chromium-375', 375, 812),
  mobileProject('chromium-390', 390, 844),
  {
    name: 'desktop-1440',
    use: {
      browserName: 'chromium',
      viewport: { width: 1440, height: 900 },
      launchOptions: CHROMIUM_LAUNCH_OPTIONS,
    },
  },
];

// WebKit may not be installed in every sandbox, and the CDP touch helpers
// are Chromium-only — so this project only runs the tests tagged @static
// (no gestures — every check that only clicks, focuses, or reads computed
// styles) and only when explicitly requested.
if (process.env.PW_WEBKIT === '1') {
  projects.push({
    name: 'webkit-iphone',
    grep: /@static/,
    use: { ...devices['iPhone 13'] },
  });
}

export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  // Generous: the CDP touch-gesture helpers (the touch-scroll tests in
  // particular, which drive several full-list scroll-and-overscroll
  // sequences) make many real round trips and are slower than a typical
  // click-driven test.
  timeout: 60_000,
  workers: 1,
  forbidOnly: !!process.env.CI,
  outputDir: '../../test-results/chat-mobile',
  use: {
    baseURL: 'http://127.0.0.1:4536',
  },
  projects,
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4536',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4536/',
    reuseExistingServer: false,
  },
});
