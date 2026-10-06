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
 * Chromium: the chat-hosted palette's computed type scale. The terminal
 * suite asserts the same dense values for its own host, so the two palettes
 * match; chat's comfy density scales the palette with the rest of the page.
 */

import { test, expect } from '@playwright/test';
import { setupApiMocks, SPACE_ALPHA, THREAD_ALPHA, USER_WITH_DM } from './mock-api.js';
import { DENSE_PALETTE_FONT_SIZES, paletteFontSizes } from '../palette-typography.js';

test('the chat palette uses the dense type scale', async ({ page }) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA],
    threadsByProjectId: { [SPACE_ALPHA.projectId]: [THREAD_ALPHA] },
    users: [USER_WITH_DM],
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  await expect(page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')).toBeVisible();
  await expect(page.locator('scion-quick-palette .palette-option').first()).toBeVisible();

  expect(await paletteFontSizes(page)).toEqual(DENSE_PALETTE_FONT_SIZES);
});

test('the chat palette follows the comfy density', async ({ page }) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA],
    threadsByProjectId: { [SPACE_ALPHA.projectId]: [THREAD_ALPHA] },
    users: [USER_WITH_DM],
  });
  await page.addInitScript(() => localStorage.setItem('scion.chat.density', 'comfy'));
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  await expect(page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')).toBeVisible();
  await expect(page.locator('scion-quick-palette .palette-option').first()).toBeVisible();

  expect(await paletteFontSizes(page)).toEqual({
    input: '18px',
    groupHeading: '14px',
    secondary: '15px',
    help: '14px',
    kbd: '12px',
  });
});
