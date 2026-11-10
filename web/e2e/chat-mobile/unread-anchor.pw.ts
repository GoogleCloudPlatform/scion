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
 * A conversation with unread messages opens with the "New messages" divider
 * near the top of the message list; one without opens at the bottom.
 * Checked at a phone width (390) and a desktop width (1280), for a direct
 * link and a rail click, and on desktop for the quick switcher too.
 */

import { test, expect, type Page, type TestInfo } from '@playwright/test';
import { buildMessages, GENERAL_THREAD_ID, PROJECT_A } from './mock-api.js';
import { openChatRail, expandSpace } from './fixture.js';

/** Threads the tests open, each with a full history and a read pointer. */
const RAIL_THREAD_ID = 'thread-03';
const SWITCHER_THREAD_ID = 'thread-04';
const UNREAD_THREADS = new Set([GENERAL_THREAD_ID, RAIL_THREAD_ID, SWITCHER_THREAD_ID]);
/** Messages 25..39 of 40 are unread. */
const LAST_READ_INDEX = 24;

/** Longer than the open-time anchor window (2 s), so a late scroll would show. */
const SETTLE_MS = 2_500;

function isDesktop(testInfo: TestInfo): boolean {
  return testInfo.project.name.startsWith('desktop');
}

/** Runs at 390 (phone) and, from the desktop project, at 1280. */
async function setUp(page: Page, testInfo: TestInfo): Promise<void> {
  test.skip(
    testInfo.project.name !== 'chromium-390' && testInfo.project.name !== 'desktop-1440',
    'checked at 390 and 1280 only'
  );
  if (isDesktop(testInfo)) await page.setViewportSize({ width: 1280, height: 800 });
}

/** Mocks a history and a read pointer for each unread thread (or none). */
async function mockUnread(page: Page, withUnread: boolean): Promise<void> {
  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const match = route
      .request()
      .url()
      .match(/\/conversations\/([^/?]+)\/messages/);
    const threadId = match ? decodeURIComponent(match[1]) : '';
    const items = UNREAD_THREADS.has(threadId) ? buildMessages(threadId, PROJECT_A.id) : [];
    void route.fulfill({ json: { items, messages: items } });
  });
  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/read$/, (route) => {
    if (route.request().method() !== 'GET') {
      void route.fulfill({ json: {} });
      return;
    }
    const match = route
      .request()
      .url()
      .match(/\/conversations\/([^/?]+)\/read$/);
    const threadId = match ? decodeURIComponent(match[1]) : '';
    void route.fulfill({
      json:
        withUnread && UNREAD_THREADS.has(threadId)
          ? { lastReadMessageId: `${threadId}-msg-${LAST_READ_INDEX}` }
          : {},
    });
  });
}

interface ScrollGeometry {
  conversationKey: string;
  /** Divider top minus the scroller's top, or null with no divider. */
  dividerOffset: number | null;
  clientHeight: number;
  distFromBottom: number;
}

function scrollGeometry(page: Page): Promise<ScrollGeometry | null> {
  return page.evaluate(() => {
    const chat = document.querySelector('scion-page-chat');
    const thread = chat?.shadowRoot?.querySelector('scion-chat-thread') as
      | (HTMLElement & { conversationKey: string })
      | null;
    const scroller = thread?.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!thread || !scroller || scroller.clientHeight === 0) return null;
    const divider = thread.shadowRoot?.querySelector('.unread-divider') as HTMLElement | null;
    return {
      conversationKey: thread.conversationKey,
      dividerOffset: divider
        ? divider.getBoundingClientRect().top - scroller.getBoundingClientRect().top
        : null,
      clientHeight: scroller.clientHeight,
      distFromBottom: scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight,
    };
  });
}

/** The divider is in view, in the top quarter of the message list. */
function dividerNearTop(g: ScrollGeometry | null, threadId: string): boolean {
  return (
    g !== null &&
    g.conversationKey === threadId &&
    g.dividerOffset !== null &&
    g.dividerOffset >= 0 &&
    g.dividerOffset <= g.clientHeight / 4 &&
    g.distFromBottom > 50
  );
}

/** Waits for the thread to open with its divider near the top, and stay there. */
async function expectOpenedAtDivider(page: Page, threadId: string): Promise<void> {
  await expect
    .poll(async () => dividerNearTop(await scrollGeometry(page), threadId), { timeout: 15_000 })
    .toBe(true);
  await page.waitForTimeout(SETTLE_MS);
  const settled = await scrollGeometry(page);
  expect(dividerNearTop(settled, threadId), JSON.stringify(settled)).toBe(true);
}

test.describe('open at the unread divider', () => {
  test('a direct link opens at the divider', async ({ page }, testInfo) => {
    await setUp(page, testInfo);
    await openChatRail(page, (p) => mockUnread(p, true));
    await page.goto(`/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}`, {
      waitUntil: 'domcontentloaded',
    });
    await expectOpenedAtDivider(page, GENERAL_THREAD_ID);
  });

  test('a rail click opens at the divider', async ({ page }, testInfo) => {
    await setUp(page, testInfo);
    await openChatRail(page, (p) => mockUnread(p, true));
    await expandSpace(page);
    await page.locator('.thread-item', { hasText: `${RAIL_THREAD_ID} discussion` }).click();
    await expectOpenedAtDivider(page, RAIL_THREAD_ID);
  });

  test('the quick switcher opens at the divider', async ({ page }, testInfo) => {
    await setUp(page, testInfo);
    test.skip(!isDesktop(testInfo), 'checked on desktop only');
    await openChatRail(page, (p) => mockUnread(p, true));
    await expandSpace(page);
    // The rail's Quick switcher button, as in palette-footer.pw.ts.
    await page.locator('.v2-rail > .palette-footer > .palette-footer-btn').click();
    await expect(
      page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')
    ).toBeVisible();
    const input = page.locator('scion-quick-palette #palette-query-input');
    await expect(input).toBeFocused();
    await input.fill(`${SWITCHER_THREAD_ID} discussion`);
    await page
      .locator('scion-quick-palette [role="option"]', {
        hasText: `${SWITCHER_THREAD_ID} discussion`,
      })
      .first()
      .click();
    await expectOpenedAtDivider(page, SWITCHER_THREAD_ID);
  });

  test('a conversation without unread messages opens at the bottom', async ({ page }, testInfo) => {
    await setUp(page, testInfo);
    await openChatRail(page, (p) => mockUnread(p, false));
    await expandSpace(page);
    await page.locator('.thread-item', { hasText: `${RAIL_THREAD_ID} discussion` }).click();
    const atBottom = async () => {
      const g = await scrollGeometry(page);
      return g !== null && g.conversationKey === RAIL_THREAD_ID && g.distFromBottom <= 2;
    };
    await expect.poll(atBottom, { timeout: 15_000 }).toBe(true);
    await page.waitForTimeout(SETTLE_MS);
    expect(await atBottom()).toBe(true);
    expect((await scrollGeometry(page))?.dividerOffset).toBeNull();
  });
});
