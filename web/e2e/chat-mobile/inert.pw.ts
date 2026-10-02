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
 * Off-screen mobile panels are `inert`, so Tab from the visible rail must
 * never reach the centre or members panels.
 */

import { test, expect } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';

/** Is the deep-active element currently inside `.v2-content` or `.v2-members`? */
async function focusInOffScreenPanel(page: import('@playwright/test').Page): Promise<boolean> {
  return page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const content = pageEl?.shadowRoot?.querySelector('.v2-content');
    const members = pageEl?.shadowRoot?.querySelector('.v2-members');

    let el: Element | null = document.activeElement;
    const chain: Element[] = [];
    while (el) {
      chain.push(el);
      const shadowActive = (el as HTMLElement).shadowRoot?.activeElement;
      if (!shadowActive) break;
      el = shadowActive;
    }
    return chain.some(
      (node) => (content && content.contains(node)) || (members && members.contains(node))
    );
  });
}

test('@static Tab from the rail never focuses into an off-screen panel', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'desktop-1440',
    'off-screen panels are only inert on the mobile layout (isMobileLayout)'
  );
  await openChatRail(page);
  // Opening the thread mounts the centre and members panels' content, so
  // there is something to wrongly focus. Then drive `mobilePanel` straight
  // back to 'left' (no swipe), leaving the other two panels mounted but
  // inert behind it.
  await openGeneralThread(page);
  await page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as unknown as { mobilePanel: string };
    pageEl.mobilePanel = 'left';
  });
  await page.waitForTimeout(400);
  expect(await currentPanel(page)).toBe('left');

  // Start focus inside the visible rail, on a real focusable control. Scoped
  // to scion-chat-space-rail: chat-members renders an identical "All" filter
  // button in its own (currently inert) members panel.
  await page.locator('scion-chat-space-rail .filter-toggle button', { hasText: 'All' }).focus();

  for (let i = 0; i < 40; i++) {
    await page.keyboard.press('Tab');
    expect(await focusInOffScreenPanel(page), `tab ${i + 1} landed outside the visible panel`).toBe(
      false
    );
  }
});
