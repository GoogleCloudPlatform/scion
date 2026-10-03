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
 * Every text entry point in chat computes at 16px or more on a coarse
 * pointer, so focusing one never triggers iOS/Android focus-zoom: the
 * composer textarea, the rail new-thread input, the rail rename input, and
 * the thread search input. Each is a separate test so the ones a given
 * project can't reach (the rename input needs a `contextmenu` event, which
 * WebKit touch emulation doesn't synthesize the way Chromium's mobile
 * emulation does) can be skipped individually rather than failing the whole
 * file.
 */

import { test, expect, type TestInfo } from '@playwright/test';
import { openChatRail, openGeneralThread, expandSpace } from './fixture.js';
import { computedBoxDeepRetrying } from './helpers.js';

const MIN_TOUCH_FONT_PX = 16;

/** These checks only apply where the pointer is coarse — not desktop. */
function skipOnDesktop(testInfo: TestInfo): void {
  test.skip(testInfo.project.name === 'desktop-1440', '16px focus-zoom guard is touch-only');
}

test('@static the composer textarea computes at 16px or more', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);
  await page.locator('sl-textarea').first().waitFor({ state: 'visible' });

  const box = await computedBoxDeepRetrying(page, 'sl-textarea', '[part="textarea"]');
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_TOUCH_FONT_PX);
});

test('@static the rail new-thread input computes at 16px or more', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await expandSpace(page);

  await page.locator('sl-icon-button[label="Space actions"]').first().click();
  await page.locator('sl-menu-item', { hasText: 'New thread' }).first().click();
  await page.locator('.create-thread sl-input').waitFor({ state: 'visible' });

  const box = await computedBoxDeepRetrying(page, '.create-thread sl-input', '[part="input"]');
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_TOUCH_FONT_PX);
});

test('@static the rail rename input computes at 16px or more', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await expandSpace(page);

  const generalRow = page.locator('.thread-item', { hasText: 'general' }).first();
  await generalRow.click({ button: 'right' });
  // On a phone the row menu opens as an action sheet.
  await page.locator('scion-action-sheet[open]').locator('.item', { hasText: 'Rename' }).click();
  await page.locator('.rename-input').waitFor({ state: 'visible' });

  const box = await computedBoxDeepRetrying(page, '.rename-input', '[part="input"]');
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_TOUCH_FONT_PX);
});

test('@static the thread search input computes at 16px or more', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);

  await page.locator('sl-icon-button[label="Search messages"]').first().click();
  await page.locator('.search-input').waitFor({ state: 'visible' });

  const box = await computedBoxDeepRetrying(page, '.search-input');
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_TOUCH_FONT_PX);
});

test('@static the app-wide Shoelace input font-size variables are 16px or more', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  // This checks the critical-CSS rule itself (pkg/hub/web.go /
  // web/index.html, inside the mobile-frame markers), not any one
  // component's usage of it: the rule must win regardless of stylesheet
  // load order, which is exactly what broke it (Shoelace's own theme sets
  // the same custom properties at the same specificity, so whichever
  // stylesheet loads last used to decide).
  const vars = await page.evaluate(() => {
    const style = getComputedStyle(document.documentElement);
    return {
      small: style.getPropertyValue('--sl-input-font-size-small').trim(),
      medium: style.getPropertyValue('--sl-input-font-size-medium').trim(),
    };
  });
  expect(vars.small).toBe('16px');
  expect(vars.medium).toBe('16px');
});

test('@static a size="small" sl-input with no local override computes at 16px or more', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  // A fresh, otherwise-unstyled sl-input — deliberately not one of the
  // app's own inputs, every one of which happens to carry a local
  // ::part(base)/::part(input) override (see the other tests in this
  // file) — so this can only pass because of the app-wide critical-CSS
  // rule, not because of a component-local fix that would mask a
  // regression in the shared rule.
  const fontSizePx = await page.evaluate(async () => {
    const el = document.createElement('sl-input');
    el.setAttribute('size', 'small');
    document.body.appendChild(el);
    try {
      await customElements.whenDefined('sl-input');
      // Two rAFs: one for Shoelace's first render, one for layout/style to
      // settle after that render.
      await new Promise((resolve) => requestAnimationFrame(resolve));
      await new Promise((resolve) => requestAnimationFrame(resolve));
      const inner = el.shadowRoot?.querySelector('input');
      return inner ? Number.parseFloat(getComputedStyle(inner).fontSize) : null;
    } finally {
      el.remove();
    }
  });
  expect(fontSizePx).not.toBeNull();
  expect(fontSizePx!).toBeGreaterThanOrEqual(MIN_TOUCH_FONT_PX);
});
