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
  spaceChevron: Box | null;
  spaceName: Box | null;
  spaceActions: Box | null;
  spaceBadge: Box | null;
  spaceMenuBase: Box | null;
  dmBadgeText: string | undefined;
  spaceBadgeText: string | undefined;
  dmBadgeRight: number | null;
  spaceBadgeRight: number | null;
}

/** Layout boxes of the DM heading and the space heading. */
async function measure(page: Page): Promise<Measurement> {
  const space = '.space-header:not(.dm-header)';
  const right = (b: Box | null): number | null => (b ? b.x + b.width : null);
  const dmBadge = await box(page, '.dm-header .unread-badge');
  const spaceBadge = await box(page, `${space} .unread-badge`);
  return {
    dmHeader: await box(page, '.dm-header'),
    dmBadge,
    spaceHeader: await box(page, space),
    spaceChevron: await box(page, `${space} .chevron`),
    spaceName: await box(page, `${space} .space-name`),
    spaceActions: await box(page, `${space} .space-actions`),
    spaceBadge,
    spaceMenuBase: await box(page, `${space} sl-icon-button [part~="base"]`),
    dmBadgeText: (await visible(page, '.dm-header .unread-badge').textContent())?.trim(),
    spaceBadgeText: (await visible(page, `${space} .unread-badge`).textContent())?.trim(),
    dmBadgeRight: right(dmBadge),
    spaceBadgeRight: right(spaceBadge),
  };
}

test('@static the DM heading badge lines up with the space heading badge', async ({
  page,
}, testInfo) => {
  const desktop = testInfo.project.name.startsWith('desktop');
  if (desktop) await page.setViewportSize({ width: 1280, height: 900 });
  await openChatRail(page, unreadMocks);
  const dmBadge = page.locator('.dm-header .unread-badge');
  await expect(dmBadge).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('.space-header:not(.dm-header) .unread-badge')).toBeVisible();

  // On desktop the space then opens #general, which re-renders the page:
  // measure once both headings have settled on screen.
  await expect
    .poll(async () => {
      const probe = await measure(page);
      return Boolean(probe.dmBadge && probe.spaceBadge && probe.dmHeader && probe.spaceHeader);
    })
    .toBe(true);
  await page.waitForTimeout(300);
  const m = await measure(page);
  const label = desktop ? 'desktop-1280' : testInfo.project.name;
  console.info(`DM-BADGE-MEASURE ${label} ${JSON.stringify(m)}`);

  const shotDir = process.env.DM_BADGE_SHOT_DIR;
  if (shotDir && m.dmHeader && m.spaceHeader) {
    const top = Math.max(0, Math.floor(m.dmHeader.y) - 8);
    await page.screenshot({
      path: `${shotDir}/${process.env.DM_BADGE_SHOT_PREFIX ?? 'rail'}-${label}.png`,
      clip: {
        x: 0,
        y: top,
        width: Math.ceil(m.spaceHeader.x + m.spaceHeader.width),
        height: Math.ceil(m.spaceHeader.y + m.spaceHeader.height) - top + 8,
      },
    });
  }

  expect(m.dmBadgeText).toBe('2');
  expect(m.spaceBadgeText).toBe(String(SPACE_UNREAD));
  expect(Math.abs(m.dmBadgeRight! - m.spaceBadgeRight!)).toBeLessThanOrEqual(1);
});
