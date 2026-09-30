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

// Deliberately separate from the Hub E2E harness: no project agents or
// credentials — everything the chat page and space rail fetch is mocked
// via page.route in the specs themselves.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 30_000,
  workers: 1,
  forbidOnly: !!process.env.CI,
  outputDir: '../../test-results/chat',
  use: {
    baseURL: 'http://127.0.0.1:4535',
    viewport: { width: 1100, height: 700 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4535',
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    url: 'http://127.0.0.1:4535/',
    reuseExistingServer: false,
  },
});
