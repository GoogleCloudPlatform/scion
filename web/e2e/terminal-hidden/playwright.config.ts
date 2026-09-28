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

// Isolated fixture for hidden terminal interaction tests.
// No project agents, credentials, or Hub connections.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 15_000,
  workers: 1,
  forbidOnly: !!process.env.CI,
  outputDir: '../../test-results/terminal-hidden',
  use: {
    baseURL: 'http://127.0.0.1:4529',
    viewport: { width: 1100, height: 700 },
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  webServer: {
    command: 'node e2e/terminal-hidden/serve.mjs',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4529/e2e/terminal-hidden/fixture.html',
    reuseExistingServer: false,
  },
});
