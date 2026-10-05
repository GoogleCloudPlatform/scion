/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Every row and message menu stays reachable and on-screen.
 *
 * On a phone the rail thread, rail group, member and message menus open as
 * a bottom action sheet — by long-press on rows, by tap on messages — with
 * full-width touch rows, and neither the press nor a swipe across the open
 * sheet navigates. On desktop the same menus open as popups that are
 * clamped so they never overflow the viewport.
 */

import { test, expect, type Locator, type Page, type TestInfo } from '@playwright/test';
import { openChatRail, expandSpace, openGeneralThread, currentPanel } from './fixture.js';
import { deepActiveElementTagName, touchHold, touchSwipe } from './helpers.js';
import { PROJECT_A } from './mock-api.js';

const DESKTOP = 'desktop-1440';
const SHEET_ROW_MIN_PX = 48;
const SHEET_LABEL_MIN_PX = 16;

const THREAD_ROW_NAME = 'thread-03 discussion';
const GROUP_NAME = 'Design group';
const DM_PEER_NAME = 'Grace Hopper';

/** The actions a plain, ungrouped thread row offers — popup and sheet alike. */
const THREAD_ACTIONS = [
  'Mark as read',
  'Mark space read',
  'Pin to top',
  'Mute',
  'Move up',
  'Move down',
  'Move to Design group',
  'Copy as Markdown',
  'Download as Markdown',
  'Rename',
  'Delete',
];
const GROUP_ACTIONS = ['New thread', 'Rename group', 'Delete group'];
const MEMBER_ACTIONS = ['Mark unread'];

/** One thread group, and a non-empty, read DM with Grace, so both menus exist. */
async function groupAndDMMocks(page: Page): Promise<void> {
  await page.route('**/api/v1/chat/user-prefs', (route) => {
    if (route.request().method() === 'PUT') {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      json: {
        spaceSortMode: 'activity',
        threadSortMode: 'activity',
        spaceOrder: '[]',
        threadOrder: '{}',
        threadGroups: JSON.stringify({
          [PROJECT_A.id]: [{ id: 'grp-design', name: GROUP_NAME, threadIds: ['thread-04'] }],
        }),
      },
    });
  });
  await page.route('**/api/v1/chat/dms**', (route) => {
    if (route.request().url().includes('/unread')) {
      void route.fulfill({ json: { peerIds: [] } });
      return;
    }
    void route.fulfill({
      json: {
        dms: [
          {
            conversationKey: 'dm:fixture-user:user-grace',
            peerId: 'user-grace',
            hasUnread: false,
            muted: false,
            lastMessageId: 'dm-msg-1',
          },
        ],
      },
    });
  });
}

function onlyMobile(testInfo: TestInfo): void {
  test.skip(testInfo.project.name === DESKTOP, 'the action sheet is the narrow-viewport path');
}

function onlyDesktop(testInfo: TestInfo): void {
  test.skip(testInfo.project.name !== DESKTOP, 'popup clamping is the wide-viewport path');
}

function viewport(page: Page): { width: number; height: number } {
  const vp = page.viewportSize();
  if (!vp) throw new Error('the project must set a viewport');
  return vp;
}

/** Long-press the middle of `target` with a real touch stroke. */
async function longPress(page: Page, target: Locator): Promise<void> {
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  if (!box) throw new Error('long-press target has no box');
  await touchHold(page, box.x + box.width / 2, box.y + box.height / 2);
}

/**
 * Long-press `target`, then slide the finger a little before lifting, as a
 * hand often does once the sheet appears. The slide means the lift sends no
 * click of its own, so the long-press is still waiting to swallow one when
 * the next tap lands; that tap must go through regardless.
 */
async function longPressThenSlide(page: Page, target: Locator): Promise<void> {
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  if (!box) throw new Error('long-press target has no box');
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
    await page.waitForTimeout(600);
    for (let step = 1; step <= 4; step++) {
      await cdp.send('Input.dispatchTouchEvent', {
        type: 'touchMove',
        touchPoints: [{ x, y: y + step * 8 }],
      });
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}

const openSheet = (page: Page): Locator => page.locator('scion-action-sheet[open]');

/**
 * The open sheet is fully on-screen, every row is a full touch target, and
 * it offers exactly `labels` (plus Cancel).
 */
async function expectSheet(page: Page, labels: string[]): Promise<void> {
  const sheet = openSheet(page);
  await expect(sheet).toHaveCount(1);
  const dialog = sheet.locator('dialog');
  await expect(dialog).toBeVisible();
  // Let the slide-in animation finish before measuring.
  await page.waitForTimeout(300);

  const vp = viewport(page);
  const box = await dialog.boundingBox();
  expect(box).not.toBeNull();
  if (!box) return;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.y).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(vp.width + 0.5);
  expect(box.y + box.height).toBeLessThanOrEqual(vp.height + 0.5);

  const items = sheet.locator('.item');
  expect((await items.allTextContents()).map((t) => t.trim())).toEqual(labels);
  // Sheet rows are taller than the general touch-target floor, and their
  // labels are never below 16px. Measured inside the borders, so Cancel's
  // separator stripe cannot make up for a short row.
  for (const row of [...(await items.all()), sheet.locator('.cancel')]) {
    const height = await row.evaluate((el) => el.clientHeight);
    expect(height).toBeGreaterThanOrEqual(SHEET_ROW_MIN_PX);
  }
  for (const label of [...(await sheet.locator('.item .label').all()), sheet.locator('.cancel')]) {
    const fontSize = await label.evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
    expect(fontSize).toBeGreaterThanOrEqual(SHEET_LABEL_MIN_PX);
  }
  const cancelBox = await sheet.locator('.cancel').boundingBox();
  expect(cancelBox).not.toBeNull();
  if (cancelBox) expect(cancelBox.y + cancelBox.height).toBeLessThanOrEqual(vp.height + 0.5);
}

/** A horizontal drag across the open sheet must not move the panel track. */
async function swipeAcrossSheet(page: Page): Promise<void> {
  const box = await openSheet(page).locator('.items').boundingBox();
  if (!box) throw new Error('sheet list has no box');
  const y = box.y + Math.min(box.height / 2, 40);
  const vp = viewport(page);
  await touchSwipe(page, vp.width * 0.85, y, vp.width * 0.1, y);
  await touchSwipe(page, vp.width * 0.1, y, vp.width * 0.85, y);
  // Give a misrouted swipe the time the panel transition would take.
  await page.waitForTimeout(400);
}

test.describe('on a phone, menus open as an on-screen action sheet', () => {
  test('a long-pressed thread row opens its sheet without opening the thread', async ({
    page,
  }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page, groupAndDMMocks);
    await expandSpace(page);
    const url = page.url();
    const panel = await currentPanel(page);

    await longPress(page, page.locator('.thread-item', { hasText: THREAD_ROW_NAME }));
    await expectSheet(page, THREAD_ACTIONS);
    expect(page.url()).toBe(url);
    expect(await currentPanel(page)).toBe(panel);

    await swipeAcrossSheet(page);
    expect(await currentPanel(page)).toBe(panel);
    await expect(openSheet(page)).toHaveCount(1);

    await openSheet(page).locator('.cancel').click();
    await expect(openSheet(page)).toHaveCount(0);
    expect(page.url()).toBe(url);

    // Escape closes it too.
    await longPress(page, page.locator('.thread-item', { hasText: THREAD_ROW_NAME }));
    await expect(openSheet(page)).toHaveCount(1);
    await page.keyboard.press('Escape');
    await expect(openSheet(page)).toHaveCount(0);
    expect(page.url()).toBe(url);
  });

  test('a short tap on a thread row still opens the thread', async ({ page }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await expandSpace(page);

    await page.locator('.thread-item', { hasText: THREAD_ROW_NAME }).tap();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'center');
    await expect(openSheet(page)).toHaveCount(0);
  });

  test('a sheet action runs: Rename from the thread sheet starts the rename', async ({
    page,
  }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await expandSpace(page);

    await longPressThenSlide(page, page.locator('.thread-item', { hasText: THREAD_ROW_NAME }));
    await openSheet(page).locator('.item', { hasText: 'Rename' }).tap();
    await expect(openSheet(page)).toHaveCount(0);
    await expect(page.locator('.rename-input')).toBeVisible();
  });

  test('a long-pressed group header opens the group sheet', async ({ page }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page, groupAndDMMocks);
    await expandSpace(page);
    const header = page.locator('.thread-group-header', { hasText: GROUP_NAME });
    await expect(header).toBeVisible();
    const collapsedBefore = await header.locator('.chevron.collapsed').count();

    await longPress(page, header);
    await expectSheet(page, GROUP_ACTIONS);
    // The press did not also toggle the group.
    expect(await header.locator('.chevron.collapsed').count()).toBe(collapsedBefore);

    await openSheet(page).locator('.cancel').click();
    await expect(openSheet(page)).toHaveCount(0);
  });

  test('a long-pressed member opens the member sheet', async ({ page }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page, groupAndDMMocks);
    await openGeneralThread(page);
    await page.locator('.mobile-members').click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
    await page.waitForTimeout(400);
    const url = page.url();

    await longPress(page, page.locator('.member-item', { hasText: DM_PEER_NAME }));
    await expectSheet(page, MEMBER_ACTIONS);
    expect(page.url()).toBe(url);

    await swipeAcrossSheet(page);
    expect(await currentPanel(page)).toBe('right');

    await page.keyboard.press('Escape');
    await expect(openSheet(page)).toHaveCount(0);
    expect(page.url()).toBe(url);
  });

  test('a tapped message opens the message sheet, and Reply from it works', async ({
    page,
  }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await openGeneralThread(page);

    const message = page.locator('scion-chat-message').last();
    await message.tap();
    const sheet = openSheet(page);
    await expect(sheet).toHaveCount(1);
    const labels = (await sheet.locator('.item').allTextContents()).map((t) => t.trim());
    expect(labels).toContain('Reply');
    expect(labels).toContain('Copy text');
    await expectSheet(page, labels);

    await swipeAcrossSheet(page);
    expect(await currentPanel(page)).toBe('center');

    await sheet.locator('.item', { hasText: 'Reply' }).click();
    await expect(openSheet(page)).toHaveCount(0);
    await expect(page.locator('.reply-bar')).toBeVisible();
  });
});

/**
 * Count the thread's message POSTs (answering each like the server would),
 * and record every `chat-send` the composer emits.
 */
async function watchSends(page: Page): Promise<{ posts: () => number }> {
  let posts = 0;
  await page.route(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages$/, (route) => {
    if (route.request().method() !== 'POST') {
      void route.fallback();
      return;
    }
    posts++;
    void route.fulfill({ json: { id: `sent-msg-${posts}` } });
  });
  await page.evaluate(() => {
    const w = window as unknown as { __chatSends: boolean[] };
    w.__chatSends = [];
    window.addEventListener(
      'chat-send',
      (e) => w.__chatSends.push((e as CustomEvent<{ interrupt: boolean }>).detail.interrupt),
      true
    );
  });
  return { posts: () => posts };
}

async function chatSends(page: Page): Promise<boolean[]> {
  return page.evaluate(() => (window as unknown as { __chatSends: boolean[] }).__chatSends);
}

async function typeDraft(page: Page, text: string): Promise<void> {
  await page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type(text);
}

async function draftValue(page: Page): Promise<string> {
  return page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as { value: string }).value);
}

test.describe('on a phone, a long-press on Send offers Send with interruption', () => {
  test('the press opens the sheet without sending, and the item sends once with interrupt', async ({
    page,
  }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await openGeneralThread(page);
    const sends = await watchSends(page);
    await typeDraft(page, 'urgent: stop that');

    await longPress(page, page.locator('.send-btn').first());
    await expectSheet(page, ['Send with interruption']);
    expect(sends.posts()).toBe(0);
    expect(await chatSends(page)).toEqual([]);
    expect(await draftValue(page)).toBe('urgent: stop that');

    await openSheet(page).locator('.item', { hasText: 'Send with interruption' }).tap();
    await expect(openSheet(page)).toHaveCount(0);
    await expect(page.locator('.messages-scroll', { hasText: 'urgent: stop that' })).toBeVisible();
    await expect.poll(() => sends.posts()).toBe(1);
    expect(await chatSends(page)).toEqual([true]);
    expect(await draftValue(page)).toBe('');
    // Same touch blur as a normal send: the keyboard stays down.
    expect(await deepActiveElementTagName(page)).not.toBe('textarea');
    // Nothing else goes out afterwards.
    await page.waitForTimeout(500);
    expect(sends.posts()).toBe(1);
  });

  test('Cancel sends nothing and keeps the draft', async ({ page }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await openGeneralThread(page);
    const sends = await watchSends(page);
    await typeDraft(page, 'not yet');

    await longPress(page, page.locator('.send-btn').first());
    await expect(openSheet(page)).toHaveCount(1);
    await openSheet(page).locator('.cancel').tap();
    await expect(openSheet(page)).toHaveCount(0);

    await page.waitForTimeout(500);
    expect(sends.posts()).toBe(0);
    expect(await chatSends(page)).toEqual([]);
    expect(await draftValue(page)).toBe('not yet');
  });

  test('a short tap on Send sends normally', async ({ page }, testInfo) => {
    onlyMobile(testInfo);
    await openChatRail(page);
    await openGeneralThread(page);
    const sends = await watchSends(page);
    await typeDraft(page, 'plain send');

    await page.locator('.send-btn').first().tap();
    await expect(page.locator('.messages-scroll', { hasText: 'plain send' })).toBeVisible();
    await expect.poll(() => sends.posts()).toBe(1);
    expect(await chatSends(page)).toEqual([false]);
    await expect(openSheet(page)).toHaveCount(0);
  });
});

/** Dispatch a contextmenu on `target` as if right-clicked at (x, y). */
async function contextMenuAt(target: Locator, x: number, y: number): Promise<void> {
  await target.evaluate(
    (el, at) =>
      el.dispatchEvent(
        new MouseEvent('contextmenu', {
          bubbles: true,
          composed: true,
          cancelable: true,
          button: 2,
          clientX: at.x,
          clientY: at.y,
        })
      ),
    { x, y }
  );
}

/** The one open popup menu is visible and fully inside the viewport. */
async function expectPopupInViewport(page: Page, labels?: string[]): Promise<void> {
  const menu = page.locator('.context-menu').filter({ visible: true });
  await expect(menu).toHaveCount(1);
  const box = await menu.boundingBox();
  expect(box).not.toBeNull();
  if (!box) return;
  const vp = viewport(page);
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.y).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(vp.width);
  expect(box.y + box.height).toBeLessThanOrEqual(vp.height);
  if (labels) {
    const items = menu.locator('.context-menu-item');
    expect((await items.allTextContents()).map((t) => t.trim())).toEqual(labels);
  }
  await expect(page.locator('scion-action-sheet[open]')).toHaveCount(0);
}

async function closePopup(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await page.mouse.click(viewport(page).width / 2, 10);
  await expect(page.locator('.context-menu').filter({ visible: true })).toHaveCount(0);
}

test.describe('on desktop, menus are clamped inside the viewport', () => {
  test('menus opened at the bottom-right corner stay on-screen', async ({ page }, testInfo) => {
    onlyDesktop(testInfo);
    await openChatRail(page, groupAndDMMocks);
    await openGeneralThread(page);
    const vp = viewport(page);
    const corner = { x: vp.width - 5, y: vp.height - 5 };

    await contextMenuAt(
      page.locator('.thread-item', { hasText: THREAD_ROW_NAME }),
      corner.x,
      corner.y
    );
    await expectPopupInViewport(page, THREAD_ACTIONS);
    await closePopup(page);

    await contextMenuAt(
      page.locator('.thread-group-header', { hasText: GROUP_NAME }),
      corner.x,
      corner.y
    );
    await expectPopupInViewport(page, GROUP_ACTIONS);
    await closePopup(page);

    await contextMenuAt(
      page.locator('.member-item', { hasText: DM_PEER_NAME }),
      corner.x,
      corner.y
    );
    await expectPopupInViewport(page, MEMBER_ACTIONS);
    await closePopup(page);

    await contextMenuAt(page.locator('scion-chat-message').last(), corner.x, corner.y);
    await expectPopupInViewport(page);
  });

  test('a real right-click at the bottom-right of the last message stays on-screen', async ({
    page,
  }, testInfo) => {
    onlyDesktop(testInfo);
    await openChatRail(page);
    await openGeneralThread(page);

    const box = await page.locator('scion-chat-message').last().boundingBox();
    if (!box) throw new Error('last message has no box');
    await page.mouse.click(box.x + box.width - 4, box.y + box.height - 4, { button: 'right' });
    await expectPopupInViewport(page);
  });

  test('a right-click with room to spare opens exactly at the pointer', async ({
    page,
  }, testInfo) => {
    onlyDesktop(testInfo);
    await openChatRail(page);
    // Settle on an open thread first, so the rail is not re-rendered by the
    // landing redirect between measuring the row and clicking it.
    await openGeneralThread(page);

    const row = page.locator('.thread-item', { hasText: THREAD_ROW_NAME });
    await expect(row).toBeVisible();
    const box = await row.boundingBox();
    if (!box) throw new Error('thread row has no box');
    // Whole pixels: the pointer event reports integer client coordinates.
    const at = { x: Math.round(box.x + box.width / 2), y: Math.round(box.y + box.height / 2) };
    await page.mouse.click(at.x, at.y, { button: 'right' });
    await expectPopupInViewport(
      page,
      THREAD_ACTIONS.filter((l) => !l.startsWith('Move to'))
    );
    const menuBox = await page.locator('.context-menu').filter({ visible: true }).boundingBox();
    expect(Math.abs((menuBox?.x ?? -99) - at.x)).toBeLessThanOrEqual(1);
    expect(Math.abs((menuBox?.y ?? -99) - at.y)).toBeLessThanOrEqual(1);
  });
});
