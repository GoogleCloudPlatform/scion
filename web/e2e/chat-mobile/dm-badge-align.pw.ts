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
 * The Direct messages heading's unread badge lines up with the space
 * heading badges: under the Unread filter, the DM badge's right edge sits
 * where a space badge's right edge does, left of that space's three-dot
 * menu, at desktop and phone widths.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { openChatRail } from './fixture.js';
import { PROJECT_A } from './mock-api.js';

const SPACE_UNREAD = 3;

async function unreadMocks(page: Page): Promise<void> {
  await page.addInitScript(() => localStorage.setItem('scion-chat-space-filter', 'unread'));
  await page.route(/\/api\/v1\/chat\/spaces$/, (route) =>
    route.fulfill({
      json: {
        spaces: [
          {
            projectId: PROJECT_A.id,
            projectSlug: PROJECT_A.slug,
            projectName: PROJECT_A.name,
            unreadCount: SPACE_UNREAD,
            hasUnreadMention: false,
          },
        ],
      },
    })
  );
  await page.route('**/api/v1/chat/dms**', (route) => {
    if (route.request().url().includes('/unread')) {
      void route.fulfill({ json: { peerIds: ['user-alice', 'agent-helper'] } });
      return;
    }
    void route.fulfill({
      json: {
        dms: [
          {
            conversationKey: 'dm:alice',
            peerId: 'user-alice',
            peerKind: 'user',
            peerName: 'Alice',
            hasUnread: true,
          },
          {
            conversationKey: 'dm:helper',
            peerId: 'agent-helper',
            peerKind: 'agent',
            peerName: 'helper',
            hasUnread: true,
          },
        ],
      },
    });
  });
}

type Box = { x: number; y: number; width: number; height: number };

/** The first visible match of `selector`. */
function visible(page: Page, selector: string): Locator {
  return page.locator(selector).filter({ visible: true }).first();
}

/** The layout box of the first visible match, or null when nothing matches. */
async function box(page: Page, selector: string): Promise<Box | null> {
  const loc = visible(page, selector);
  return (await loc.count()) ? loc.boundingBox() : null;
}

interface Measurement {
  dmHeader: Box | null;
  dmBadge: Box | null;
  spaceHeader: Box | null;
  spaceBadge: Box | null;
}

const SPACE_HEADER = '.space-header:not(.dm-header)';

/** Layout boxes of the DM heading, the space heading and their badges. */
async function measure(page: Page): Promise<Measurement> {
  return {
    dmHeader: await box(page, '.dm-header'),
    dmBadge: await box(page, '.dm-header .unread-badge'),
    spaceHeader: await box(page, SPACE_HEADER),
    spaceBadge: await box(page, `${SPACE_HEADER} .unread-badge`),
  };
}

/** How far apart the two badges' right edges are; Infinity until both render. */
async function badgeRightGap(page: Page): Promise<number> {
  const { dmBadge, spaceBadge } = await measure(page);
  if (!dmBadge || !spaceBadge) return Number.POSITIVE_INFINITY;
  return Math.abs(dmBadge.x + dmBadge.width - (spaceBadge.x + spaceBadge.width));
}

test('@static the DM heading badge lines up with the space heading badge', async ({
  page,
}, testInfo) => {
  if (testInfo.project.name.startsWith('desktop')) {
    await page.setViewportSize({ width: 1280, height: 900 });
  }
  await openChatRail(page, unreadMocks);
  const dmBadge = visible(page, '.dm-header .unread-badge');
  const spaceBadge = visible(page, `${SPACE_HEADER} .unread-badge`);
  await expect(dmBadge).toBeVisible({ timeout: 15_000 });
  await expect(spaceBadge).toBeVisible();
  await expect(dmBadge).toHaveText('2');
  await expect(spaceBadge).toHaveText(String(SPACE_UNREAD));

  // On desktop the space then opens #general, which re-renders the page:
  // poll until the right edges agree rather than sleeping.
  await expect.poll(() => badgeRightGap(page)).toBeLessThanOrEqual(1);

  // The spacer adds no height: the DM heading is no taller than a space
  // heading.
  await expect
    .poll(async () => {
      const { dmHeader, spaceHeader } = await measure(page);
      return dmHeader && spaceHeader
        ? dmHeader.height - spaceHeader.height
        : Number.POSITIVE_INFINITY;
    })
    .toBeLessThanOrEqual(1);
});
