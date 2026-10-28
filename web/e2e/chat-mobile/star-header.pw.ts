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
 * The conversation header's starred-message list and the keyboard path to
 * the message menu (web.chat_starred_messages), with mock data and a
 * mocked hub:
 *
 * - the header star stays visible and inside the viewport on narrow
 *   screens;
 * - the list's loading, error (with Retry), empty and paginated states;
 *   on a phone it is a bottom sheet that fits the screen, and touch
 *   scrolling inside it stays inside it;
 * - keyboard: open the list, reach an entry with Tab, open it (focus lands
 *   on the message), Escape returns focus to the star; star and unstar a
 *   message from its More actions button with Shift+F10;
 * - a reduced-motion jump lands at once; rapid A then B ends on B.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { touchScroll } from './helpers.js';
import { PROJECT_A } from './mock-api.js';
import { isPhone, mockThread, msgId, setupStarRail, type StarRailHub } from './star-rail-mock.js';

const KEY = 'topic-mock-planning';
const URL_PATH = `/chat/space/${PROJECT_A.id}/thread/${KEY}`;
const MESSAGES = Array.from({ length: 60 }, (_, i) => i + 1);
const STARRED = MESSAGES.slice(0, 45);

interface ListControl {
  /** Requests of the list endpoint, in order. */
  calls: URLSearchParams[];
  /** Answer the next N list requests with 500. */
  failNext: number;
  /** Hold list answers until released (null: answer at once). */
  hold: Promise<void> | null;
}

/** The mock hub with one long conversation and a paging star list. */
async function setup(
  pw: Page,
  starred: number[] = STARRED
): Promise<{ hub: StarRailHub; list: ListControl }> {
  const conv = mockThread(KEY, 'Mock planning', PROJECT_A, '2026-10-22T10:00:00Z', starred);
  conv.messages = MESSAGES;
  const hub = await setupStarRail(pw, [conv]);
  const list: ListControl = { calls: [], failNext: 0, hold: null };
  // Registered last, so it answers the list before the shared star route.
  await pw.route(/\/api\/v1\/chat\/stars\?/, async (route) => {
    const q = new URL(route.request().url()).searchParams;
    list.calls.push(q);
    if (list.hold) await list.hold;
    if (list.failNext > 0) {
      list.failNext--;
      await route.fulfill({
        status: 500,
        json: { error: { code: 'internal', message: 'Could not load starred messages' } },
      });
      return;
    }
    const limit = Number(q.get('limit') ?? '20');
    const offset = Number((q.get('cursor') ?? 'o-0').slice(2));
    // Most recently starred first: the highest message number was starred last.
    const rows = [...conv.starred].sort((a, b) => b - a);
    const slice = rows.slice(offset, offset + limit);
    await route.fulfill({
      json: {
        items: slice.map((n) => ({
          messageId: msgId(KEY, n),
          conversationKey: KEY,
          conversationKind: 'thread',
          projectId: PROJECT_A.id,
          projectName: PROJECT_A.name,
          conversationName: 'Mock planning',
          senderName: 'Mock Person',
          messageCreatedAt: new Date(Date.parse('2026-10-20T08:00:00Z') + n * 60_000).toISOString(),
          starredAt: new Date(Date.parse('2026-10-22T08:00:00Z') + n * 60_000).toISOString(),
          excerpt: `Mock starred note ${n} in Mock planning: the rollout starts after the migration.`,
          edited: false,
          permalink: `${URL_PATH}#msg-${msgId(KEY, n)}`,
        })),
        ...(offset + limit < rows.length ? { nextCursor: `o-${offset + limit}` } : {}),
      },
    });
  });
  return { hub, list };
}

const trigger = (pw: Page): Locator => pw.locator('scion-chat-starred-list button.trigger');
const entries = (pw: Page): Locator => pw.locator('scion-chat-starred-list a.entry');
const listSurface = (pw: Page): Locator =>
  pw.locator(
    isPhone(pw) ? 'scion-chat-starred-list dialog.sheet' : 'scion-chat-starred-list .panel'
  );
const loadMore = (pw: Page): Locator =>
  pw.locator('scion-chat-starred-list .more sl-button', { hasText: 'Load more' });

async function openConversation(pw: Page): Promise<void> {
  await pw.goto(URL_PATH, { waitUntil: 'domcontentloaded' });
  await expect(pw.locator(`#msg-${msgId(KEY, 60)}`)).toBeVisible({ timeout: 15_000 });
  await expect(trigger(pw)).toBeVisible();
}

/** The deepest focused element's id/class summary, through shadow roots. */
function deepFocus(
  pw: Page
): Promise<{ tag: string; id: string; cls: string; text: string } | null> {
  return pw.evaluate(() => {
    let el: Element | null = document.activeElement;
    for (;;) {
      const next = (el as Element & { shadowRoot?: ShadowRoot })?.shadowRoot?.activeElement ?? null;
      if (!next || next === el) break;
      el = next;
    }
    if (!el || el === document.body) return null;
    return {
      tag: el.tagName.toLowerCase(),
      id: el.id,
      cls: el.getAttribute('class') ?? '',
      text: (el.textContent ?? '').trim(),
    };
  });
}

/** The message element the deepest focus is in (its own id, or its host's). */
function focusedMessageId(pw: Page): Promise<string> {
  return pw.evaluate(() => {
    let el: Element | null = document.activeElement;
    let msg = '';
    for (;;) {
      if (el?.tagName.toLowerCase() === 'scion-chat-message') msg = el.id;
      const next = (el as Element & { shadowRoot?: ShadowRoot })?.shadowRoot?.activeElement ?? null;
      if (!next || next === el) break;
      el = next;
    }
    return msg;
  });
}

/** Whether a message's box is inside the thread's scroller. */
function inView(pw: Page, n: number): Promise<boolean> {
  return pw.evaluate(
    (id) => {
      const thread = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-chat-thread');
      const root = thread?.shadowRoot;
      const msg = root?.getElementById(`msg-${id}`);
      const scroller = root?.querySelector('.messages-scroll');
      if (!msg || !scroller) return false;
      const a = msg.getBoundingClientRect();
      const b = scroller.getBoundingClientRect();
      return a.top >= b.top - 1 && a.bottom <= b.bottom + 1;
    },
    msgId(KEY, n)
  );
}

test.describe('starred messages: header list', () => {
  test('the header star stays visible inside a narrow viewport', async ({ page }) => {
    await setup(page);
    await openConversation(page);
    const box = await trigger(page).boundingBox();
    const width = page.viewportSize()!.width;
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(width);
    // Not folded behind More: it is a direct, labelled control.
    await expect(trigger(page)).toHaveAttribute(
      'aria-label',
      'Starred messages in this conversation'
    );
  });

  test('loading, error with Retry, and pagination to the end', async ({ page }) => {
    const { list } = await setup(page);
    await openConversation(page);
    let release: () => void = () => {};
    list.hold = new Promise<void>((r) => (release = r));
    list.failNext = 1;
    await trigger(page).click();
    await expect(listSurface(page)).toBeVisible();
    await expect(listSurface(page).getByText('Loading starred messages…')).toBeVisible();
    list.hold = null;
    release();
    const alert = listSurface(page).locator('[role="alert"]');
    await expect(alert).toBeVisible();
    await alert.locator('sl-button', { hasText: 'Retry' }).click();
    await expect(entries(page)).toHaveCount(20);
    await expect(entries(page).first()).toContainText('Mock starred note 45');
    await loadMore(page).click();
    await expect(entries(page)).toHaveCount(40);
    await loadMore(page).click();
    await expect(entries(page)).toHaveCount(45);
    await expect(loadMore(page)).toHaveCount(0);
    expect(list.calls.at(-1)?.get('cursor')).toBe('o-40');
    // The panel or sheet fits the viewport: nothing clipped.
    const box = (await listSurface(page).boundingBox())!;
    const vp = page.viewportSize()!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(vp.width + 0.5);
    expect(box.y + box.height).toBeLessThanOrEqual(vp.height + 0.5);
  });

  test('the empty state explains how to add a star', async ({ page }) => {
    await setup(page, []);
    await openConversation(page);
    await trigger(page).click();
    await expect(listSurface(page)).toContainText('No starred messages in this conversation');
    await expect(listSurface(page)).toContainText('Star message');
  });

  test('a reduced-motion jump from the list lands at once and focuses the message', async ({
    page,
  }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await setup(page);
    await openConversation(page);
    await trigger(page).click();
    await expect(entries(page)).toHaveCount(20);
    // Load to the oldest star, far above the newest messages.
    await loadMore(page).click();
    await loadMore(page).click();
    await expect(entries(page)).toHaveCount(45);
    await entries(page).last().click();
    await expect(listSurface(page)).toHaveCount(0);
    // No smooth scroll: in view on the next frames, and it stays there.
    await expect.poll(() => inView(page, 1), { timeout: 1_000 }).toBe(true);
    const top = await page
      .locator('scion-chat-thread .messages-scroll')
      .evaluate((el) => el.scrollTop);
    await page.waitForTimeout(400);
    expect(
      await page.locator('scion-chat-thread .messages-scroll').evaluate((el) => el.scrollTop)
    ).toBe(top);
    expect(await focusedMessageId(page)).toBe(`msg-${msgId(KEY, 1)}`);
  });

  test('rapid A then B ends on B', async ({ page }) => {
    await setup(page);
    await openConversation(page);
    await trigger(page).click();
    await expect(entries(page)).toHaveCount(20);
    // A: the 20th most recent star (message 26).
    await entries(page).nth(19).click();
    await trigger(page).click();
    await expect(entries(page).first()).toBeVisible();
    // B: the 10th most recent star (message 36), before A has settled.
    await entries(page).nth(9).click();
    await expect
      .poll(() => focusedMessageId(page), { timeout: 5_000 })
      .toBe(`msg-${msgId(KEY, 36)}`);
    await expect.poll(() => inView(page, 36), { timeout: 5_000 }).toBe(true);
    await page.waitForTimeout(800);
    expect(await inView(page, 36)).toBe(true);
    expect(await focusedMessageId(page)).toBe(`msg-${msgId(KEY, 36)}`);
  });
});

test.describe('starred messages: header list on a phone', () => {
  test.beforeEach(({ page }) => {
    test.skip(!isPhone(page), 'phone layout only');
  });

  test('a bottom sheet with a heading and a 44px Close; touch scrolling stays inside it', async ({
    page,
  }) => {
    await setup(page);
    await openConversation(page);
    await trigger(page).tap();
    const sheet = listSurface(page);
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('h2')).toHaveText('Starred messages');
    const close = sheet.locator('button.close');
    const cbox = (await close.boundingBox())!;
    expect(cbox.width).toBeGreaterThanOrEqual(44);
    expect(cbox.height).toBeGreaterThanOrEqual(44);
    await expect(entries(page)).toHaveCount(20);
    const ebox = (await entries(page).first().boundingBox())!;
    expect(ebox.height).toBeGreaterThanOrEqual(44);

    const scroller = sheet.locator('.scroll');
    const room = await scroller.evaluate((el) => el.scrollHeight - el.clientHeight);
    expect(room, 'starred sheet list must overflow before the touch drag').toBeGreaterThan(100);
    // Measure the list only after the sheet's slide-in has finished, or the drag starts below it.
    await sheet.evaluate((el) =>
      Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)).then(() => undefined)
    );
    const sbox = (await scroller.boundingBox())!;
    const frameBefore = await page.evaluate(() => document.scrollingElement?.scrollTop ?? 0);
    await touchScroll(page, sbox.x + sbox.width / 2, sbox.y + sbox.height - 30, 0, -200);
    await expect.poll(() => scroller.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
    // Still open, the page behind did not move and no panel swipe happened.
    await expect(sheet).toBeVisible();
    expect(await page.evaluate(() => document.scrollingElement?.scrollTop ?? 0)).toBe(frameBefore);
    await expect(page.locator('scion-chat-thread')).toBeVisible();

    await close.tap();
    await expect(sheet).toHaveCount(0);
  });

  test('tapping an entry opens the message in the full conversation', async ({ page }) => {
    await setup(page);
    await openConversation(page);
    await trigger(page).tap();
    await expect(entries(page)).toHaveCount(20);
    await entries(page).nth(5).tap();
    await expect(listSurface(page)).toHaveCount(0);
    await expect.poll(() => inView(page, 40), { timeout: 5_000 }).toBe(true);
  });
});

test.describe('starred messages: keyboard', () => {
  test.beforeEach(({ page }) => {
    test.skip(isPhone(page), 'desktop keyboard');
  });

  test('open the list, reach an entry with Tab, open it; Escape returns focus to the star', async ({
    page,
  }) => {
    await setup(page);
    await openConversation(page);
    await trigger(page).focus();
    await page.keyboard.press('Enter');
    await expect(listSurface(page)).toBeVisible();
    await expect(trigger(page)).toHaveAttribute('aria-expanded', 'true');
    await expect(entries(page)).toHaveCount(20);
    await page.keyboard.press('Escape');
    await expect(listSurface(page)).toHaveCount(0);
    expect((await deepFocus(page))?.cls).toContain('trigger');

    await page.keyboard.press('Enter');
    await expect(entries(page)).toHaveCount(20);
    await page.keyboard.press('Tab');
    expect((await deepFocus(page))?.cls).toContain('entry');
    await page.keyboard.press('Tab');
    await page.keyboard.press('Enter');
    await expect(listSurface(page)).toHaveCount(0);
    // The second most recent star: message 44.
    await expect.poll(() => focusedMessageId(page)).toBe(`msg-${msgId(KEY, 44)}`);
  });

  test('star and unstar a message from its More actions button', async ({ page }) => {
    const { hub } = await setup(page, []);
    await openConversation(page);
    const target = page.locator(`#msg-${msgId(KEY, 58)}`);
    const button = target.locator('button.msg-actions');
    await button.focus();
    await page.keyboard.press('Shift+F10');
    // By name: the composer's agent menu is also a role="menu" whenever the thread has agents.
    const menu = page.getByRole('menu', { name: /^Message actions/ });
    await expect(menu).toBeVisible();
    await expect(button).toHaveAttribute('aria-expanded', 'true');
    const star = menu.locator('[role="menuitemcheckbox"]');
    await expect(star).toHaveAttribute('aria-checked', 'false');
    for (let i = 0; i < 12; i++) {
      if (((await deepFocus(page))?.text ?? '').includes('Star message')) break;
      await page.keyboard.press('ArrowDown');
    }
    await page.keyboard.press('Enter');
    await expect(target.locator('.star-indicator')).toBeVisible();
    // The indicator is optimistic: wait for the mocked hub to record the request.
    await expect.poll(() => hub.conversations[0].starred.has(58)).toBe(true);
    // Focus came back to the message's button.
    await expect.poll(async () => (await deepFocus(page))?.cls ?? '').toContain('msg-actions');

    await page.keyboard.press('Enter');
    await expect(star).toHaveAttribute('aria-checked', 'true');
    for (let i = 0; i < 12; i++) {
      if (((await deepFocus(page))?.text ?? '').includes('Unstar message')) break;
      await page.keyboard.press('ArrowDown');
    }
    await page.keyboard.press('Enter');
    await expect(target.locator('.star-indicator')).toHaveCount(0);
    // The indicator is optimistic: wait for the mocked hub to record the request.
    await expect.poll(() => hub.conversations[0].starred.has(58)).toBe(false);
  });

  test('Up/Down move between messages from the action button; Tab leaves the list', async ({
    page,
  }) => {
    await setup(page);
    await openConversation(page);
    const last = page.locator(`#msg-${msgId(KEY, 60)} button.msg-actions`);
    await last.focus();
    await page.keyboard.press('ArrowUp');
    await expect.poll(() => focusedMessageId(page)).toBe(`msg-${msgId(KEY, 59)}`);
    await expect(page.locator(`#msg-${msgId(KEY, 59)} button.msg-actions`)).toHaveAttribute(
      'tabindex',
      '0'
    );
    await expect(last).toHaveAttribute('tabindex', '-1');
    await page.keyboard.press('Tab');
    expect(await focusedMessageId(page)).toBe('');
  });
});
