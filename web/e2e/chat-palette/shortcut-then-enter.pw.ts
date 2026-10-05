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
 * Chromium: the palette shortcut pressed while typing, followed by Enter,
 * stays in the conversation being typed in. On macOS, Ctrl+K in the composer
 * keeps its native meaning and never opens the palette. With an empty query,
 * Enter leaves the palette open until the user picks a row, so the
 * conversation with the newest activity is never opened by accident.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { setupApiMocks, AGENT_WITH_DM, AGENT_WITHOUT_DM, SELF_USER_ID } from './mock-api.js';

// A conversation other than the one the empty-query ranking puts first.
const CURRENT_DM_KEY = `dm:agent:${AGENT_WITHOUT_DM.id}:user:${SELF_USER_ID}`;
const CURRENT_ROUTE = `/chat/dm/${encodeURIComponent(CURRENT_DM_KEY)}`;
const CURRENT_URL = new RegExp(`/chat/dm/${encodeURIComponent(CURRENT_DM_KEY)}$`);

/** Makes the app's platform check report macOS. */
async function emulateMac(page: Page): Promise<void> {
  await page.addInitScript(() => {
    Object.defineProperty(Navigator.prototype, 'userAgentData', {
      configurable: true,
      get: () => ({ platform: 'macOS' }),
    });
  });
}

async function gotoCurrentConversation(page: Page): Promise<void> {
  await setupApiMocks(page);
  await page.goto(`/e2e/chat-palette/fixture.html?route=${encodeURIComponent(CURRENT_ROUTE)}`, {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForSelector('scion-chat-composer sl-textarea', {
    state: 'attached',
    timeout: 10_000,
  });
}

function composerTextarea(page: Page): Locator {
  return page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

function paletteInput(page: Page): Locator {
  return page.locator('scion-quick-palette #palette-query-input');
}

function paletteOptions(page: Page): Locator {
  return page.locator('scion-quick-palette .palette-option');
}

test('on macOS, Ctrl+K then Enter in the composer never opens the palette or leaves the conversation', async ({
  page,
}) => {
  await emulateMac(page);
  await gotoCurrentConversation(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft in progress');
  await page.evaluate(() => {
    const w = window as unknown as { __ctrlKPrevented?: boolean };
    document.addEventListener('keydown', (e) => {
      if (e.key === 'k' && e.ctrlKey) w.__ctrlKPrevented = e.defaultPrevented;
    });
  });

  await page.keyboard.press('Control+k');
  expect(
    await page.evaluate(
      () => (window as unknown as { __ctrlKPrevented?: boolean }).__ctrlKPrevented
    )
  ).toBe(false);
  // The key reached the field: it keeps focus and its text (the caret is at
  // the end, so deleting to the end of the line removes nothing).
  await expect(textarea).toBeFocused();
  await expect(textarea).toHaveValue('draft in progress');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(300);

  await expect(page.locator('scion-quick-palette')).toHaveCount(0);
  await expect(textarea).toBeFocused();
  await expect(page).toHaveURL(CURRENT_URL);
});

test('on macOS, Cmd+K in the composer still opens the palette', async ({ page }) => {
  await emulateMac(page);
  await gotoCurrentConversation(page);
  await composerTextarea(page).click();

  await page.keyboard.press('Meta+k');

  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();
});

test('the shortcut then Enter on an empty query keeps the palette open and the conversation', async ({
  page,
}) => {
  await gotoCurrentConversation(page);
  await composerTextarea(page).click();

  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  // The newest activity is highlighted, but not chosen.
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name })).toHaveClass(/active/);
  await page.keyboard.press('Enter');
  await page.waitForTimeout(300);

  await expect(page).toHaveURL(CURRENT_URL);
  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();

  // Picking the highlighted row with the arrow keys, then Enter, opens it.
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowUp');
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name })).toHaveClass(/active/);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
});
