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

import base from './playwright.config.js';
import { VIEW_ENV } from './state.js';

// Page view journeys (ptone/scion#5569): the port proxy rig of
// playwright.config.ts, with hub.port_view on from hub start, the conduit
// agent only, and the extra identities journey 4 uses (see
// global-setup.ts). Global setup and teardown run in this process, so the
// variable below reaches them.
process.env[VIEW_ENV] = '1';

const CI = !!process.env.CI;

export default defineConfig({
  ...base,
  testMatch: '**/port-view.pw.ts',
  testIgnore: [],
  reporter: CI
    ? [
        ['github'],
        ['list'],
        ['html', { outputFolder: '../../playwright-report/port-proxy-view', open: 'never' }],
      ]
    : 'list',
  forbidOnly: CI,
  outputDir: '../../test-results/port-proxy-view',
});
