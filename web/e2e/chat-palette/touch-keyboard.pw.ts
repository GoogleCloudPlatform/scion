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
 * Touch emulation, real <scion-chat-shell>: a tap on the quick-switcher
 * button at the bottom of the spaces rail gives a text field focus before
 * the tap ends, so iOS shows the on-screen keyboard. Once the switcher has
 * preloaded (a touch-primary device mounts it when the page is idle), that
 * field is its own query input, focused inside the tap. A tap before the
 * preload finishes focuses a hidden field instead, and the query input takes
 * focus over from it once the palette shows. Runs on Chromium with the
 * suite, and on WebKit too with PW_WEBKIT=1 (see playwright.config.ts).
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';
import { focusAtClickEnd, paletteInputHasFocus, recordFocusAtClickEnd } from '../palette-focus.js';

/**
 * Holds every request for the palette module until the returned function is
 * called, so a tap made before that lands before the preload (or the first
 * open's import) can finish, whatever the timing.
 */
async function holdPaletteModule(page: Page): Promise<() => void> {
  let release!: () => void;
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(/\/quick-palette\.ts(\?|$)/, async (route) => {
    await released;
    await route.continue();
  });
  return release;
}

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

async function gotoShell(page: Page): Promise<void> {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html?shell=1', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

function paletteButton(page: Page): Locator {
  return page.locator('scion-page-chat .v2-rail > .palette-footer > .palette-footer-btn');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

/** Waits for the idle-time preload to mount the switcher, closed. */
async function waitForPreload(page: Page): Promise<void> {
  await expect(page.locator('scion-quick-palette')).toHaveCount(1, { timeout: 10_000 });
}

/** Taps the button and returns which field had focus as the tap's click finished. */
async function tapAndReadFocus(page: Page) {
  await recordFocusAtClickEnd(page);
  await paletteButton(page).tap();
  const focused = await focusAtClickEnd(page);
  expect(focused, 'a click reached the page').not.toBeNull();
  return focused!;
}

test('precondition: this device matches TOUCH_PRIMARY_QUERY', async ({ page }) => {
  await gotoShell(page);
  const matches = await page.evaluate(
    () => matchMedia('(hover: none) and (pointer: coarse)').matches
  );
  expect(matches).toBe(true);
});

test('once preloaded, a tap focuses the query input inside the tap', async ({ page }) => {
  await gotoShell(page);
  await waitForPreload(page);
  const focused = await tapAndReadFocus(page);
  expect(focused.paletteInput).toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('input[data-palette-keyboard-proxy]')).toHaveCount(0);
});

test('a tap before the preload finishes holds the keyboard with the hidden field', async ({
  page,
}) => {
  const releaseModule = await holdPaletteModule(page);
  await gotoShell(page);
  await expect(page.locator('scion-quick-palette')).toHaveCount(0);
  const focused = await tapAndReadFocus(page);
  expect(focused.textField).toBe(true);
  expect(focused.keyboardProxy, 'the hidden field had focus within the tap').toBe(true);
  releaseModule();
  await expect(paletteDialog(page)).toBeVisible();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('input[data-palette-keyboard-proxy]')).toHaveCount(0);
});

test('a later tap, with the palette already loaded, focuses the query input inside the tap', async ({
  page,
}) => {
  await gotoShell(page);
  await waitForPreload(page);
  await paletteButton(page).tap();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();

  const focused = await tapAndReadFocus(page);
  expect(focused.paletteInput).toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
});

test('typing straight after the tap becomes the query, while the palette module loads', async ({
  page,
}) => {
  const releaseModule = await holdPaletteModule(page);
  await gotoShell(page);
  await paletteButton(page).tap();
  await page.keyboard.type('ali');
  releaseModule();

  await expect(paletteDialog(page)).toBeVisible();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveValue('ali');
});

test('Escape after a tap-driven open returns focus to the button', async ({ page }) => {
  await gotoShell(page);
  await waitForPreload(page);
  await paletteButton(page).tap();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toBeFocused();
});
