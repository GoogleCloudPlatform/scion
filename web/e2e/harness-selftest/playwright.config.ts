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

// Self-test of the shared e2e harness (../harness/hub.ts): startHub's
// options checked against a stand-in hub binary (a Node script written by
// the spec), so it needs neither Go nor a browser. No global setup.
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.pw.ts',
  timeout: 60_000,
  workers: 1,
  retries: 0,
  reporter: CI ? [['github'], ['list']] : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/harness-selftest',
});
