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
 * The @mention dropdown opens upwards from the composer, over the message
 * list. It must fit the room between the composer and the thread header,
 * so every candidate can be seen and tapped, even in a short landscape
 * frame, and its rows are touch-sized on touch screens and phones.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';

/**
 * Enough agents that the full list (8 rows) is taller than a landscape frame
 * allows. Each name is its own slug, so every row is a single line: the
 * shortest row there is.
 */
const MANY_AGENTS = Array.from({ length: 12 }, (_, i) => {
  const slug = `qa-agent-${String(i + 1).padStart(2, '0')}`;
  return { id: `agent-${slug}`, kind: 'agent', displayName: slug, slug };
});

async function withManyMembers(page: Page): Promise<void> {
  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/members/, (route) =>
    route.fulfill({ json: { humans: [], agents: MANY_AGENTS } })
  );
}

/** Type `@` into the composer and wait for the dropdown. */
async function openMentions(page: Page): Promise<void> {
  const field = page.locator('scion-chat-composer textarea');
  // Focus without a click, so no pointer rests over the dropdown to move its
  // highlight by hovering.
  await field.focus();
  await field.pressSequentially('@');
  await expect(page.locator('scion-mention-autocomplete .dropdown-item').first()).toBeVisible();
}

/** Where the dropdown and its rows sit, and what a tap at the first row would hit. */
async function measureDropdown(page: Page) {
  return page.evaluate(() => {
    const find = (root: ParentNode, selector: string): Element | null => {
      const hit = root.querySelector(selector);
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          const nested = find(el.shadowRoot, selector);
          if (nested) return nested;
        }
      }
      return null;
    };
    const dropdown = find(document, 'scion-mention-autocomplete')!.shadowRoot!.querySelector(
      '.dropdown'
    )!;
    const rows = [...dropdown.querySelectorAll('.dropdown-item')];
    const first = rows[0].getBoundingClientRect();
    // Follow the hit-test down through the shadow roots.
    let hit: Element | null = document.elementFromPoint(
      first.left + first.width / 2,
      first.top + first.height / 2
    );
    while (hit?.shadowRoot) {
      const inner = hit.shadowRoot.elementFromPoint(
        first.left + first.width / 2,
        first.top + first.height / 2
      );
      if (!inner || inner === hit) break;
      hit = inner;
    }
    const header = find(document, '.v2-thread-header')!.getBoundingClientRect();
    const box = dropdown.getBoundingClientRect();
    return {
      top: box.top,
      bottom: box.bottom,
      headerBottom: header.bottom,
      rowHeights: rows.map((r) => r.getBoundingClientRect().height),
      firstRowHit: !!hit?.closest('.dropdown-item') && rows[0].contains(hit),
    };
  });
}

test.describe('the mention dropdown', () => {
  test('rows are touch-sized on a phone and unchanged on desktop', async ({ page }, testInfo) => {
    await openChatRail(page, withManyMembers);
    await openGeneralThread(page);
    await openMentions(page);
    const box = await measureDropdown(page);
    if (testInfo.project.name === 'desktop-1440') {
      for (const h of box.rowHeights) expect(h, 'desktop row').toBeLessThan(44);
    } else {
      for (const h of box.rowHeights) expect(h, 'phone row').toBeGreaterThanOrEqual(44);
    }
    expect(box.top, 'below the thread header').toBeGreaterThanOrEqual(box.headerBottom);
    expect(box.firstRowHit, 'the first candidate takes the tap').toBe(true);
  });

  test.describe('touch landscape (844x390)', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    test('fits below the thread header, and the first candidate can be tapped', async ({
      page,
    }, testInfo) => {
      test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
      await openChatRail(page, withManyMembers);
      const cdp = await page.context().newCDPSession(page);
      await cdp.send('Emulation.setSafeAreaInsetsOverride', {
        insets: { top: 0, right: 47, bottom: 21, left: 47 },
      });
      await openGeneralThread(page);
      await openMentions(page);
      const box = await measureDropdown(page);
      expect(box.top, 'below the thread header').toBeGreaterThanOrEqual(box.headerBottom);
      expect(box.firstRowHit, 'the first candidate takes the tap').toBe(true);

      // The capped list scrolls: arrowing up wraps to the last candidate and
      // brings it into view, and arrowing back down returns to the first.
      const rows = page.locator('scion-mention-autocomplete .dropdown-item');
      const dropdown = page.locator('scion-mention-autocomplete .dropdown');
      await page.keyboard.press('ArrowUp');
      const view = (await dropdown.boundingBox())!;
      const last = (await rows.last().boundingBox())!;
      expect(last.y + last.height, 'last row in view').toBeLessThanOrEqual(
        view.y + view.height + 0.5
      );
      expect(last.y, 'last row in view').toBeGreaterThanOrEqual(view.y - 0.5);
      await page.keyboard.press('ArrowDown');
      expect((await measureDropdown(page)).firstRowHit, 'back at the first row').toBe(true);

      await rows.first().tap();
      await expect(page.locator('scion-chat-composer textarea')).toHaveValue(/^@\S+ ?/);
      await expect(page.locator('scion-mention-autocomplete .dropdown')).toHaveCount(0);
    });
  });
});
