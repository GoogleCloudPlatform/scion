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
 * Browser Back and Forward step through the mobile chat panels.
 *
 * On a phone the chat page shows one panel at a time. Each panel change
 * records a history entry on the same URL, so Back goes from the members
 * list to the conversation, from the conversation to the rail, and from the
 * rail on to the previous page. The page is never rebuilt for a panel
 * entry: the tests tag the page element and type a draft to show it.
 * Desktop, with every panel on screen, adds no entries.
 */

import { test, expect, type Page } from '@playwright/test';
import { currentPanel } from './fixture.js';
import { touchSwipe } from './helpers.js';
import { setupChatMobileMocks, PROJECT_A, GENERAL_THREAD_ID } from './mock-api.js';

const DESKTOP = 'desktop-1440';
/** Settle time for the panel-track transform transition. */
const SETTLE_MS = 400;
const SPACE_URL = `/chat/${PROJECT_A.slug}`;
const THREAD_URL = `/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}`;

function skipOnDesktop(testInfo: { project: { name: string } }): void {
  test.skip(testInfo.project.name === DESKTOP, 'panels are separate screens only on a phone');
  test.skip(testInfo.project.name.startsWith('webkit'), 'CDP touch is Chromium-only');
}

/** Land on the space's rail by its readable URL: one history entry, no redirect. */
async function openSpaceRail(page: Page): Promise<void> {
  await setupChatMobileMocks(page);
  await page.goto(SPACE_URL, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('.thread-item', { hasText: 'general' }).first()).toBeVisible({
    timeout: 15_000,
  });
}

/** Navigate in the app (as a nav link does): pushes an entry and renders the route. */
async function navigateInApp(page: Page, path: string): Promise<void> {
  await page.evaluate((p) => {
    document.dispatchEvent(
      new CustomEvent('nav-click', { detail: { path: p }, bubbles: true, composed: true })
    );
  }, path);
  await expect.poll(() => new URL(page.url()).pathname).toBe(path);
}

/** Land on /chat first, then go to the space's rail in the app: Back has an in-app page to reach. */
async function openSpaceRailInApp(page: Page): Promise<void> {
  await setupChatMobileMocks(page);
  await page.goto('/chat', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('.space-header').first()).toBeVisible({ timeout: 15_000 });
  await navigateInApp(page, SPACE_URL);
  await expect(page.locator('.thread-item', { hasText: 'general' }).first()).toBeVisible({
    timeout: 15_000,
  });
}

/** Tap #general in the rail and wait for the conversation panel. */
async function tapGeneral(page: Page): Promise<void> {
  await page.locator('.thread-item', { hasText: 'general' }).first().click();
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 10_000 });
  await expectPanel(page, 'center');
}

async function expectPanel(page: Page, panel: string): Promise<void> {
  await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', panel);
  await page.waitForTimeout(SETTLE_MS);
}

/** Tag the chat page element; `expectSamePage` checks it was never rebuilt. */
async function tagPage(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.querySelector('scion-page-chat')?.setAttribute('data-panel-back-probe', '');
  });
}

async function expectSamePage(page: Page, context: string): Promise<void> {
  const counts = await page.evaluate(() => ({
    tagged: document.querySelectorAll('scion-page-chat[data-panel-back-probe]').length,
    pages: document.querySelectorAll('scion-page-chat').length,
  }));
  expect(counts, `${context}: the chat page element survived`).toEqual({ tagged: 1, pages: 1 });
}

const historyLength = (page: Page): Promise<number> => page.evaluate(() => history.length);

/**
 * Run a navigation that makes the router render a new chat page, and wait
 * for that page to be in the document: a page not there yet cannot handle
 * the next Back or Forward.
 */
async function toNewPage(page: Page, navigate: () => Promise<unknown>): Promise<void> {
  await page.evaluate(() => {
    document.querySelector('scion-page-chat')?.setAttribute('data-old-page', '');
  });
  await navigate();
  await expect(page.locator('scion-page-chat:not([data-old-page])')).toHaveCount(1, {
    timeout: 15_000,
  });
}

/** The panel the current history entry records. */
const entryPanel = (page: Page): Promise<string | undefined> =>
  page.evaluate(
    () => (history.state as { scionInPage?: { panel: string } } | null)?.scionInPage?.panel
  );

/** The composer's textarea value, through its shadow roots. */
async function draft(page: Page): Promise<string> {
  return page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as { value: string }).value);
}

async function typeDraft(page: Page, text: string): Promise<void> {
  await page
    .locator('sl-textarea')
    .first()
    .evaluate((el, value) => {
      const field = el as unknown as HTMLElement & { value: string };
      field.value = value;
      field.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    }, text);
}

async function swipeAcross(page: Page, dx: number): Promise<void> {
  const vp = page.viewportSize();
  const w = vp?.width ?? 375;
  const h = vp?.height ?? 812;
  const x = dx < 0 ? w * 0.7 : w * 0.3;
  await touchSwipe(page, x, h / 2, x + dx, h / 2, 8, 300);
  await page.waitForTimeout(SETTLE_MS);
}

test.describe('Back and Forward step through the mobile chat panels', () => {
  test.beforeEach(({ page: _page }, testInfo) => skipOnDesktop(testInfo));

  test('Back goes members -> conversation -> rail -> previous page, and Forward redoes it', async ({
    page,
  }) => {
    await openSpaceRail(page);
    await tapGeneral(page);
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
    await tagPage(page);
    await typeDraft(page, 'half-written reply');

    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');

    await page.goBack();
    await expectPanel(page, 'center');
    expect(await draft(page), 'the draft survives').toBe('half-written reply');

    await page.goBack();
    await expectPanel(page, 'left');
    // The rail entry moved to the thread's URL when the thread opened, so
    // Back slid the panels without rebuilding the page.
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
    await expectSamePage(page, 'after Back twice');

    await page.goForward();
    await expectPanel(page, 'center');
    await page.goForward();
    await expectPanel(page, 'right');
    await expectSamePage(page, 'after Forward twice');
    expect(await draft(page), 'the draft survives Forward').toBe('half-written reply');

    // From the rail, Back is ordinary browser history: the page before chat.
    await page.goBack();
    await page.goBack();
    await expectPanel(page, 'left');
    await page.goBack();
    await expect.poll(() => page.url()).not.toContain('/chat');
  });

  test("the app's own back control and swipes go back through history, adding no entries", async ({
    page,
  }) => {
    await openSpaceRail(page);
    await swipeAcross(page, -150); // rail -> conversation (empty: nothing open yet)
    await expectPanel(page, 'center');
    await swipeAcross(page, 150); // and back
    await expectPanel(page, 'left');
    await tapGeneral(page);
    await tagPage(page);
    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');
    const length = await historyLength(page);

    // The members panel's back button, then a swipe back to the rail.
    await page.locator('.v2-members .mobile-back').click();
    await expectPanel(page, 'center');
    await swipeAcross(page, 150);
    await expectPanel(page, 'left');
    expect(await historyLength(page), 'going back added no entries').toBe(length);

    // Forward redoes both moves; a swipe forward again truncates nothing extra.
    await page.goForward();
    await expectPanel(page, 'center');
    await swipeAcross(page, -150);
    await expectPanel(page, 'right');
    expect(await historyLength(page)).toBe(length);
    await expectSamePage(page, 'after the back controls');
  });

  test('a panel swipe adds one entry, and Back undoes it', async ({ page }) => {
    await openSpaceRail(page);
    await tapGeneral(page);
    const length = await historyLength(page);

    await swipeAcross(page, -150); // conversation -> members
    await expectPanel(page, 'right');
    expect(await historyLength(page)).toBe(length + 1);

    await page.goBack();
    await expectPanel(page, 'center');
    await page.goBack();
    await expectPanel(page, 'left');
  });

  test('opening another thread from the rail replaces the rail entry, so Back never revisits the old thread', async ({
    page,
  }) => {
    await openSpaceRail(page);
    await tapGeneral(page);
    await page.goBack();
    await expectPanel(page, 'left');
    const length = await historyLength(page);

    const other = page.locator('.thread-item').filter({ hasNotText: 'general' }).first();
    await other.click();
    await expectPanel(page, 'center');
    const otherUrl = new URL(page.url()).pathname;
    expect(otherUrl).not.toBe(THREAD_URL);
    // The forward entry of #general was replaced: same length as before.
    expect(await historyLength(page)).toBe(length);

    await page.goBack();
    await expectPanel(page, 'left');
    expect(new URL(page.url()).pathname, 'Back stays on the rail of the open thread').toBe(
      otherUrl
    );
  });

  test('a deep link opens on the conversation; the back control leads to the rail, and Back returns to it', async ({
    page,
  }) => {
    await setupChatMobileMocks(page);
    await page.goto(THREAD_URL, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
    await expectPanel(page, 'center');
    await tagPage(page);
    const length = await historyLength(page);

    await page.locator('.v2-content .mobile-back').click();
    await expectPanel(page, 'left');
    expect(await historyLength(page), 'no entry beneath to go back to: replaced').toBe(length);

    await swipeAcross(page, -150);
    await expectPanel(page, 'center');
    await page.goBack();
    await expectPanel(page, 'left');
    await expectSamePage(page, 'deep link');
  });

  test('a reload lands on the panel it was on, and Back still steps through the panels', async ({
    page,
  }) => {
    await openSpaceRail(page);
    await tapGeneral(page);
    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
    await expectPanel(page, 'right');
    // The rail's reload re-parses the route; the panel must hold.
    await page.waitForTimeout(1500);
    expect(await currentPanel(page)).toBe('right');
    await tagPage(page);

    await page.goBack();
    await expectPanel(page, 'center');
    await page.goBack();
    await expectPanel(page, 'left');
    await expectSamePage(page, 'after reload');
  });

  test('Back with an action sheet open closes the sheet', async ({ page }) => {
    await openSpaceRail(page);
    await tapGeneral(page);
    const more = page.locator('.v2-thread-header .header-more');
    await expect(more).toBeVisible();
    await tagPage(page);
    await more.click();
    const sheet = page.locator('scion-action-sheet[open]');
    await expect(sheet).toHaveCount(1);

    await page.goBack();
    await expect(sheet).toHaveCount(0);
    await expectPanel(page, 'left');
    // Closed by the sheet itself, not by a rebuilt page dropping it.
    await expectSamePage(page, 'Back with a sheet open');
  });

  test('widened while on the page, one Back leaves the thread', async ({ page }) => {
    await openSpaceRailInApp(page);
    await tapGeneral(page);
    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.waitForTimeout(SETTLE_MS);
    await page.goBack();
    // Past the conversation and rail entries, to the page before, in one press.
    await expect.poll(() => new URL(page.url()).pathname).toBe('/chat');
  });

  test('panel entries left from a phone are stepped over on desktop, and hold again on a phone', async ({
    page,
  }) => {
    const vp = page.viewportSize()!;
    await openSpaceRailInApp(page);
    await tapGeneral(page);
    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');
    // Leave chat for another page, and come back wide (a rotated tablet, a
    // window snapped to half width and back).
    await navigateInApp(page, '/chat');
    const length = await historyLength(page);
    await page.setViewportSize({ width: 1440, height: 900 });
    await toNewPage(page, () => page.goBack());
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
    // It lands on the members entry; the page, created wide on it, drops
    // back to the first entry of the URL.
    await expect.poll(() => entryPanel(page)).toBe('left');
    expect(await historyLength(page)).toBe(length);

    // One Back leaves the thread.
    await toNewPage(page, () => page.goBack());
    expect(new URL(page.url()).pathname).toBe('/chat');
    // Forward returns to it. Forward through the panel entries made on the
    // phone is left alone (one press each, nothing to see), then reaches
    // the page after them.
    await toNewPage(page, () => page.goForward());
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
    await expect.poll(() => entryPanel(page)).toBe('left');
    await page.goForward();
    await expect.poll(() => entryPanel(page)).toBe('center');
    await page.goForward();
    await expect.poll(() => entryPanel(page)).toBe('right');
    await toNewPage(page, () => page.goForward());
    expect(new URL(page.url()).pathname).toBe('/chat');
    expect(await historyLength(page), 'nothing was written').toBe(length);

    // Back lands on the members entry again, and drops back to the first.
    await toNewPage(page, () => page.goBack());
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
    await expect.poll(() => entryPanel(page)).toBe('left');
    expect(await historyLength(page)).toBe(length);

    // Narrow again: the page shows the panel the entry records, and the
    // entries after it still lead to the conversation and the members.
    await page.setViewportSize(vp);
    await expectPanel(page, 'left');
    await page.goForward();
    await expectPanel(page, 'center');
    await page.goForward();
    await expectPanel(page, 'right');
    await page.goBack();
    await expectPanel(page, 'center');
    await swipeAcross(page, 150);
    await expectPanel(page, 'left');
    expect(new URL(page.url()).pathname).toBe(THREAD_URL);
  });
});

test.describe('Back and Forward around panel runs on the same URL, and around other routes', () => {
  test.beforeEach(({ page: _page }, testInfo) => skipOnDesktop(testInfo));

  /** Count popstates from now on. */
  async function countPops(page: Page): Promise<() => Promise<number>> {
    await page.evaluate(() => {
      const w = window as unknown as { __pops: number };
      w.__pops = 0;
      window.addEventListener('popstate', () => w.__pops++);
    });
    return () => page.evaluate(() => (window as unknown as { __pops: number }).__pops);
  }

  test('picking the open thread again adds no entry; wide, one Back leaves, without looping', async ({
    page,
  }) => {
    await openSpaceRailInApp(page);
    await tapGeneral(page);
    const length = await historyLength(page);
    // Pick the open thread again from the conversation (as the palette's
    // Threads group does).
    await page.evaluate(
      ({ projectId, slug, id }) => {
        document
          .querySelector('scion-page-chat')
          ?.shadowRoot?.querySelector('scion-chat-space-rail')
          ?.dispatchEvent(
            new CustomEvent('thread-select', {
              detail: { conversationKey: id, projectId, projectSlug: slug, threadName: 'general' },
              bubbles: true,
              composed: true,
            })
          );
      },
      { projectId: PROJECT_A.id, slug: PROJECT_A.slug, id: GENERAL_THREAD_ID }
    );
    await page.waitForTimeout(SETTLE_MS);
    expect(await historyLength(page), 'no entry for the same thread').toBe(length);
    await expectPanel(page, 'center');

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.waitForTimeout(SETTLE_MS);
    const pops = await countPops(page);
    await toNewPage(page, () => page.goBack());
    expect(new URL(page.url()).pathname).toBe('/chat');
    await page.waitForTimeout(1500);
    // The user's Back and one step over the phone entries; nothing after.
    expect(await pops()).toBe(2);
    expect(new URL(page.url()).pathname).toBe('/chat');
  });

  test('wide after a reload on top of phone entries: Back leaves, and keeps leaving', async ({
    page,
  }) => {
    await openSpaceRailInApp(page);
    await tapGeneral(page);
    await page.locator('.mobile-members').click();
    await expectPanel(page, 'right');
    await page.setViewportSize({ width: 1440, height: 900 });
    // The open thread clicked in the (now visible) rail adds no entry.
    const length = await historyLength(page);
    await page.locator('.thread-item', { hasText: 'general' }).first().click();
    await page.waitForTimeout(SETTLE_MS);
    expect(await historyLength(page)).toBe(length);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
    // Created wide on the members entry: back to the first entry of the URL.
    await expect.poll(() => entryPanel(page)).toBe('left');
    const pops = await countPops(page);

    await toNewPage(page, () => page.goBack());
    expect(new URL(page.url()).pathname).toBe('/chat');
    await page.waitForTimeout(1000);
    expect(await pops(), 'one Back, no further steps').toBe(1);
    expect(new URL(page.url()).pathname).toBe('/chat');
  });

  test('a quick Back while the next route is still loading shows chat again, not that route', async ({
    page,
  }) => {
    await openSpaceRailInApp(page);
    await tapGeneral(page);
    await tagPage(page);
    // Hold the Agents page's module back, so its render is still in flight
    // when Back arrives.
    await page.route('**/src/components/pages/agents.ts*', async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 2000));
      await route.continue();
    });

    await navigateInApp(page, '/agents');
    await page.evaluate(() => history.back());
    await expect.poll(() => new URL(page.url()).pathname).toBe(THREAD_URL);
    // Past the time the held render would have finished.
    await page.waitForTimeout(3000);
    expect(await page.locator('scion-page-agents').count(), 'the Agents page never mounted').toBe(
      0
    );
    await expectSamePage(page, 'Back during the Agents render');
    await expectPanel(page, 'center');
  });
});

test.describe('desktop: panel changes add no history entries', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name !== DESKTOP, 'desktop-only');
  });

  test('opening a thread pushes one entry, toggling members none, and Back leaves the thread', async ({
    page,
  }) => {
    await openSpaceRail(page);
    // Desktop opens #general for the space, and redirects to its URL.
    await page.waitForURL(new RegExp(`${THREAD_URL}$`), { timeout: 15_000 });
    await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
    const length = await historyLength(page);

    const toggle = page.locator('.desktop-members sl-icon-button');
    await toggle.click();
    await toggle.click();
    expect(await historyLength(page)).toBe(length);
    expect(
      await page.evaluate(() => (history.state as Record<string, unknown> | null)?.scionInPage)
    ).toBeUndefined();

    const other = page.locator('.thread-item').filter({ hasNotText: 'general' }).first();
    await other.click();
    await expect.poll(() => new URL(page.url()).pathname).not.toBe(THREAD_URL);
    expect(await historyLength(page)).toBe(length + 1);

    await page.goBack();
    await expect.poll(() => new URL(page.url()).pathname).toBe(THREAD_URL);
  });
});
