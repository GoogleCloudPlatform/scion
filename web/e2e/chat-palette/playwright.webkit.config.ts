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

/**
 * The chat palette's touch keyboard checks on WebKit, the engine iOS Safari
 * uses: `npx playwright install webkit` (and its system dependencies) first.
 * The Linux WebKit build does not show an on-screen keyboard, so this
 * checks where focus is during and after the tap, not the keyboard itself.
 */

import { defineConfig } from '@playwright/test';
import base from './playwright.config.js';

export default defineConfig({
  ...base,
  testMatch: 'touch-keyboard.pw.ts',
  outputDir: '../../test-results/chat-palette-webkit',
  use: {
    baseURL: base.use?.baseURL,
    viewport: base.use?.viewport,
    browserName: 'webkit',
  },
});
