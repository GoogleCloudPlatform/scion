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
 * Shared by the chat, terminal and graph palette e2e suites: whether the
 * palette's query input holds keyboard focus, a slow palette module to
 * widen the window between an open and that focus, and what had focus as
 * the tap that opened a palette finished.
 */

import { expect, type Page } from '@playwright/test';

/**
 * Whether the deepest focused element, through open shadow roots, is the
 * query input of a `<scion-quick-palette>`.
 */
export function paletteInputHasFocus(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    const root = el?.getRootNode();
    return (
      el?.id === 'palette-query-input' &&
      root instanceof ShadowRoot &&
      root.host.tagName === 'SCION-QUICK-PALETTE'
    );
  });
}

/**
 * Delays the palette component module by `ms`, as a first open over a slow
 * network would see it. Keys typed meanwhile must still become the query.
 */
export async function slowPaletteModule(page: Page, ms = 400): Promise<void> {
  await page.route(/\/quick-palette\.ts(\?|$)/, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, ms));
    await route.continue();
  });
}

/** What had focus, through open shadow roots, as a click finished dispatching. */
export interface FocusAtClickEnd {
  /** A text `<input>`: focusing one inside a tap is what shows the iOS keyboard. */
  textField: boolean;
  /** The hidden field a palette focuses until its query input can take focus. */
  keyboardProxy: boolean;
  /** The palette's own query input. */
  paletteInput: boolean;
}

/**
 * Records what has focus as each later click on the page finishes
 * dispatching: from a window listener in the bubble phase, which runs after
 * the opener's own click handler, in the same task. iOS Safari shows the
 * on-screen keyboard only for a text field focused by then.
 */
export async function recordFocusAtClickEnd(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __focusAtClickEnd?: FocusAtClickEnd | null };
    w.__focusAtClickEnd = null;
    window.addEventListener('click', () => {
      let el: Element | null = document.activeElement;
      while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
      const root = el?.getRootNode();
      w.__focusAtClickEnd = {
        textField: el instanceof HTMLInputElement && el.type === 'text' && !el.readOnly,
        keyboardProxy:
          el instanceof HTMLElement && el.matches('input[data-palette-keyboard-proxy]'),
        paletteInput:
          el?.id === 'palette-query-input' &&
          root instanceof ShadowRoot &&
          root.host.tagName === 'SCION-QUICK-PALETTE',
      };
    });
  });
}

/** What {@link recordFocusAtClickEnd} saw at the latest click, if any. */
export function focusAtClickEnd(page: Page): Promise<FocusAtClickEnd | null> {
  return page.evaluate(
    () =>
      (window as unknown as { __focusAtClickEnd?: FocusAtClickEnd | null }).__focusAtClickEnd ??
      null
  );
}

/**
 * Taps with `tap` and checks the palette holds the on-screen keyboard
 * throughout: a text field has focus as the tap ends, before the palette
 * shows, and the query input then takes focus over from it, leaving no
 * hidden field behind.
 */
export async function expectTapHoldsKeyboard(page: Page, tap: () => Promise<void>): Promise<void> {
  await recordFocusAtClickEnd(page);
  await tap();
  const focused = await focusAtClickEnd(page);
  expect(focused, 'a click reached the page').not.toBeNull();
  expect(focused!.textField).toBe(true);
  expect(focused!.keyboardProxy || focused!.paletteInput).toBe(true);
  await expect.poll(() => paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('input[data-palette-keyboard-proxy]')).toHaveCount(0);
}
