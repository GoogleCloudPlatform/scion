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

import { chromiumLaunchOptions } from '../../playwright-chromium.js';

const CI = !!process.env.CI;

// Real-hub E2E suite for interactive chat choices (ptone/scion#7776).
// Starts the real Go hub binary with `web.chat_choices` enabled and a
// co-located runtime broker so containerless test agents (`../harness/agent.ts`)
// can send real choice prompts over SSE without requiring Docker.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 90_000,
  workers: 1,
  retries: CI ? 1 : 0,
  globalTimeout: CI ? 15 * 60_000 : 0,
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/chat-choices', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/chat-choices',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  use: {
    trace: CI ? 'on-first-retry' : 'off',
    viewport: { width: 1280, height: 800 },
    launchOptions: chromiumLaunchOptions(),
  },
});
