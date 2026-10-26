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
 * A tap on a quick-switcher button focuses a text field before the tap ends,
 * so a phone opens its on-screen keyboard, and the switcher's query input
 * then takes focus over from it, on a phone that does not match the
 * touch-primary query (several Android phones report hover support or a
 * fine pointer), emulated by making that one query report no match. A
 * phone that matches it is covered in e2e/chat-palette/touch-keyboard.pw.ts.
 * Playwright cannot show an on-screen keyboard: these tests check the focus
 * the keyboard depends on, not the keyboard itself.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { focusAtClickEnd, paletteInputHasFocus, recordFocusAtClickEnd } from '../palette-focus.js';

const PANEL_SETTLE_MS = 400;
const TOUCH_PRIMARY_QUERY = '(hover: none) and (pointer: coarse)';

const isDesktop = (projectName: string): boolean => projectName === 'desktop-1440';

function railButton(page: Page): Locator {
  return page.locator('.v2-rail > .palette-footer > .palette-footer-btn');
}

function membersButton(page: Page): Locator {
  return page.locator('.v2-members > .palette-footer > .palette-footer-btn');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

/** Makes `matchMedia(TOUCH_PRIMARY_QUERY)` report no match, as some Android phones do. */
async function reportNotTouchPrimary(page: Page): Promise<void> {
  await page.addInitScript((query) => {
    const real = window.matchMedia.bind(window);
    window.matchMedia = (q: string) => (q === query ? real('not all') : real(q));
  }, TOUCH_PRIMARY_QUERY);
}

/**
 * Taps `button` and checks that a text field had focus as the tap's click
 * finished dispatching, then that the switcher's query input has focus,
 * with no hidden field left behind.
 */
async function expectTapFocusesQueryInput(page: Page, button: Locator): Promise<void> {
  await recordFocusAtClickEnd(page);
  await button.tap();
  const focused = await focusAtClickEnd(page);
  expect(focused, 'a click reached the page').not.toBeNull();
  expect(focused!.textField, 'a text field had focus within the tap').toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('input[data-palette-keyboard-proxy]')).toHaveCount(0);
}

test('@static phone not matching the touch-primary query: a tap still focuses the query input', async ({
  page,
}, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'touch only');
  await reportNotTouchPrimary(page);
  await openChatRail(page);
  expect(await page.evaluate((q) => matchMedia(q).matches, TOUCH_PRIMARY_QUERY)).toBe(false);
  await expectTapFocusesQueryInput(page, railButton(page));
});

test('@static phone not matching the touch-primary query: the members column button too', async ({
  page,
}, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'panel concept is mobile-only');
  await reportNotTouchPrimary(page);
  await openChatRail(page);
  await openGeneralThread(page);
  await page.locator('.mobile-members').click();
  await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  await page.waitForTimeout(PANEL_SETTLE_MS);
  await expectTapFocusesQueryInput(page, membersButton(page));
});

test('@static desktop: a mouse click focuses the query input, with no hidden field', async ({
  page,
}, testInfo) => {
  test.skip(!isDesktop(testInfo.project.name), 'desktop only');
  await openChatRail(page);
  await recordFocusAtClickEnd(page);
  await railButton(page).click();
  const focused = await focusAtClickEnd(page);
  expect(focused!.keyboardProxy, 'no hidden field on a mouse click').toBe(false);
  await expect(paletteDialog(page)).toBeVisible();
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
});

test('@static phone: once the switcher has preloaded, a tap focuses its query input inside the tap', async ({
  page,
}, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'touch only');
  await openChatRail(page);
  // A touch-primary device mounts the switcher, closed, once the page is idle.
  await expect(page.locator('scion-quick-palette')).toHaveCount(1, { timeout: 10_000 });
  await recordFocusAtClickEnd(page);
  await railButton(page).tap();
  const focused = await focusAtClickEnd(page);
  expect(focused, 'a click reached the page').not.toBeNull();
  expect(focused!.paletteInput, 'the query input had focus within the tap').toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('input[data-palette-keyboard-proxy]')).toHaveCount(0);
});

test('@static desktop: the switcher does not preload', async ({ page }, testInfo) => {
  test.skip(!isDesktop(testInfo.project.name), 'desktop only');
  await openChatRail(page);
  // Longer than the preload's fallback delay; a desktop never schedules it.
  await page.waitForTimeout(3_000);
  await expect(page.locator('scion-quick-palette')).toHaveCount(0);
});

test('@static phone: Escape after an in-tap open closes the switcher and releases focus', async ({
  page,
}, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'touch only');
  await openChatRail(page);
  await expect(page.locator('scion-quick-palette')).toHaveCount(1, { timeout: 10_000 });
  await recordFocusAtClickEnd(page);
  await railButton(page).tap();
  expect((await focusAtClickEnd(page))!.paletteInput).toBe(true);

  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  // Focus goes back to the button that opened it.
  await expect(railButton(page)).toBeFocused();
  // The dialog's focus trap is released: Tab moves on from the button rather
  // than back into the closed switcher.
  await page.keyboard.press('Tab');
  await expect(railButton(page)).not.toBeFocused();
  const insideSwitcher = await page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el) {
      if (el.tagName === 'SCION-QUICK-PALETTE') return true;
      el = el.shadowRoot?.activeElement ?? null;
    }
    return false;
  });
  expect(insideSwitcher, 'focus is not inside the switcher or its shadow root').toBe(false);
});
