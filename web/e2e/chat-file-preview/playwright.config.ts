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

// Isolated fixture for the extracted <scion-chat-file-preview>.
// Mounts the real scion-chat-thread/scion-chat-message components with
// endpoint-shaped request interception — no live Hub. See e2e/chat-palette
// for the sibling pattern this follows.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 20_000,
  workers: 1,
  forbidOnly: !!process.env.CI,
  outputDir: '../../test-results/chat-file-preview',
  use: {
    baseURL: 'http://127.0.0.1:4535',
    viewport: { width: 1200, height: 800 },
    // The markdown-copy test depends on the browser's default
    // clipboard-write permission, which varies by environment ("prompt" in
    // some containers, where writeText is denied, and "granted" in others);
    // the grant pins it to granted so the result does not depend on the
    // environment.
    permissions: ['clipboard-read', 'clipboard-write'],
    launchOptions: {
      ...(process.env.CHROMIUM_EXECUTABLE
        ? { executablePath: process.env.CHROMIUM_EXECUTABLE }
        : {}),
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  webServer: {
    command: 'node e2e/chat-file-preview/serve.mjs',
    cwd: new URL('../../', import.meta.url).pathname,
    url: 'http://127.0.0.1:4535/e2e/chat-file-preview/fixture.html',
    reuseExistingServer: false,
  },
});
