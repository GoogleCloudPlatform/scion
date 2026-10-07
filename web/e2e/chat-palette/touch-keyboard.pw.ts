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
 * Touch emulation, real <scion-chat-shell>: a tap on the header's
 * quick-switcher button gives a text field focus before the tap ends, so
 * iOS Safari shows the on-screen keyboard, and the query input takes focus
 * over from it once the palette shows. Runs on Chromium with the suite and
 * on WebKit with playwright.webkit.config.ts.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';
import {
  expectTapHoldsKeyboard,
  paletteInputHasFocus,
  slowPaletteModule,
} from '../palette-focus.js';

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

async function gotoShell(page: Page): Promise<void> {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html?shell=1', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

function paletteButton(page: Page) {
  return page.locator('scion-header .palette-button');
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

test('precondition: this device matches TOUCH_PRIMARY_QUERY', async ({ page }) => {
  await gotoShell(page);
  const matches = await page.evaluate(
    () => matchMedia('(hover: none) and (pointer: coarse)').matches
  );
  expect(matches).toBe(true);
});

test('the first tap holds the keyboard from the tap until the query input has focus', async ({
  page,
}) => {
  await slowPaletteModule(page);
  await gotoShell(page);
  await expectTapHoldsKeyboard(page, () => paletteButton(page).tap());
  await expect(paletteDialog(page)).toBeVisible();
});

test('a later tap, with the palette already loaded, holds the keyboard too', async ({ page }) => {
  await gotoShell(page);
  await paletteButton(page).tap();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();

  await expectTapHoldsKeyboard(page, () => paletteButton(page).tap());
});

test('typing straight after the tap becomes the query, while the palette module loads', async ({
  page,
}) => {
  await slowPaletteModule(page);
  await gotoShell(page);
  await paletteButton(page).tap();
  await page.keyboard.type('ali');

  await expect(paletteDialog(page)).toBeVisible();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveValue('ali');
});

test('Escape after a tap-driven open returns focus to the button', async ({ page }) => {
  await gotoShell(page);
  await paletteButton(page).tap();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toBeFocused();
});
