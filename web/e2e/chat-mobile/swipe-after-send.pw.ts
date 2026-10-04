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
 * A real-device report: after sending a message, swiping right back to the
 * rail bounces back to the conversation a couple of seconds later. This is
 * not scroll drift (the panels are `overflow: clip` and inert off-screen)
 * — something re-selects the center panel as a side effect of the send.
 *
 * Root cause: `chat.ts`'s `handleChatMessage` (bound to the state manager's
 * `chat-message-received` event — the same event an SSE echo of the user's
 * own just-sent message arrives on) debounces a `rail.reload()` 2s after
 * any chat message. That reload re-dispatches `rail-loaded`, whose handler
 * calls `parseV2Route()` to "re-resolve the route now that slug data is
 * available". `parseV2Route()`'s readable-thread-match branch unconditionally
 * sets `mobilePanel = 'center'` and rebuilds `v2Conversation` every time it
 * runs, with no guard for "already viewing this exact thread" — unlike the
 * DM branch a few lines below it, which has exactly that guard. Swiping
 * away from the conversation never changes the URL (by design — it is a
 * lightweight panel switch, not a navigation), so the thread route still
 * matches on the next parse, and the panel gets stomped back to 'center'.
 *
 * There is no real SSE transport in this mocked harness (`EventSource` is
 * stubbed to a no-op in mock-api.ts), so the echo is simulated by invoking
 * the page element's own `handleChatMessage` directly — the same method
 * `stateManager`'s real `chat-message-received` listener calls, just
 * without reimplementing the SSE transport in between.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';
import { touchSwipe, deepActiveElementTagName } from './helpers.js';
import { setupChatMobileMocks, PROJECT_A, GENERAL_THREAD_ID, HUMAN_MEMBER_ID } from './mock-api.js';

/**
 * Upper bound for `simulateChatMessageEcho`'s wait on the rail's debounced
 * reload. Generous relative to chat.ts's own 2000ms debounce so a slow CI
 * run doesn't turn into a flake — the test still only waits as long as the
 * reload actually takes, not a fixed guess.
 */
const RAIL_RELOAD_WAIT_TIMEOUT_MS = 6_000;

async function mockSuccessfulSend(page: Page): Promise<void> {
  await page.route(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages$/, (route) => {
    if (route.request().method() !== 'POST') {
      void route.fallback();
      return;
    }
    void route.fulfill({ json: { id: 'sent-msg-1', content: 'hello from the test' } });
  });
}

/**
 * Simulate the SSE echo of a chat message arriving, without a real
 * transport, and wait for the debounced rail reload it triggers to actually
 * complete — rather than sleeping for a guessed duration.
 *
 * `chat-space-rail.ts`'s `reload()` dispatches a `bubbles: true, composed:
 * true` `rail-loaded` CustomEvent once `loadData()` finishes (see its
 * `loadData` finally-block). `composed: true` means that event crosses every
 * shadow-root boundary between the rail and `document`, so listening on
 * `document` observes it regardless of how deeply the rail is nested. That
 * makes it a reliable completion signal for the 2s-debounced reload this
 * test is exercising, without reaching into the rail's internals.
 */
async function simulateChatMessageEcho(page: Page): Promise<void> {
  await page.evaluate((timeoutMs) => {
    const pageEl = document.querySelector('scion-page-chat') as unknown as {
      handleChatMessage: (e: Event) => void;
    } | null;
    if (!pageEl) {
      throw new Error('scion-page-chat not found: cannot simulate the chat-message-received echo');
    }
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        document.removeEventListener('rail-loaded', onRailLoaded);
        reject(
          new Error(
            `rail-loaded did not fire within ${timeoutMs}ms of the simulated echo ` +
              '(the debounced reload this test depends on did not complete)'
          )
        );
      }, timeoutMs);
      const onRailLoaded = (): void => {
        clearTimeout(timer);
        resolve();
      };
      document.addEventListener('rail-loaded', onRailLoaded, { once: true });
      pageEl.handleChatMessage(
        new CustomEvent('chat-message-received', { detail: { senderId: 'self' } })
      );
    });
  }, RAIL_RELOAD_WAIT_TIMEOUT_MS);
}

/** Type into the composer and tap Send, the way a user ends a message. */
async function sendFromComposer(page: Page): Promise<void> {
  await page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type('hello from the test');
  await page.locator('.send-btn').first().click();
}

async function swipeRightToRail(page: Page): Promise<void> {
  const vp = page.viewportSize();
  const w = vp?.width ?? 375;
  const h = vp?.height ?? 812;
  await touchSwipe(page, w / 2, h / 2, w, h / 2, 8, 150);
  await page.waitForTimeout(400); // panel-track transform transition
}

test('swipe-back to the rail shortly after Send is not reverted when the rail reloads', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSuccessfulSend(page);

  await page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type('hello from the test');
  await page.locator('.send-btn').first().click();

  // Swipe back to the rail within ~300ms of hitting Send.
  await page.waitForTimeout(300);
  await swipeRightToRail(page);
  expect(await currentPanel(page)).toBe('left');

  // The echo of the just-sent message arrives; this resolves once the
  // rail's debounced reload has actually completed (see
  // simulateChatMessageEcho's doc comment), not after a guessed delay.
  await simulateChatMessageEcho(page);

  expect(
    await currentPanel(page),
    'the rail reload must not revert a manual swipe back to the rail'
  ).toBe('left');
  expect(
    await deepActiveElementTagName(page),
    'focus must not land in the (inert, off-screen) conversation panel'
  ).not.toBe('textarea');
});

test('swipe-back to the rail after the send resolves is not reverted when the rail reloads', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSuccessfulSend(page);

  await page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type('hello from the test');
  const [response] = await Promise.all([
    page.waitForResponse(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages$/),
    page.locator('.send-btn').first().click(),
  ]);
  expect(response.ok()).toBe(true);

  await swipeRightToRail(page);
  expect(await currentPanel(page)).toBe('left');

  await simulateChatMessageEcho(page);

  expect(
    await currentPanel(page),
    'the rail reload must not revert a manual swipe back to the rail'
  ).toBe('left');
  expect(
    await deepActiveElementTagName(page),
    'focus must not land in the (inert, off-screen) conversation panel'
  ).not.toBe('textarea');
});

test('a DM opened from a peer-ID link is not reverted when the rail reloads after Send', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');
  await setupChatMobileMocks(page);
  await mockSuccessfulSend(page);
  // /chat/dm/<peerId> is the URL form a reload, a shared link or Back lands
  // on. The open conversation is keyed by the full DM key, so the URL
  // segment never equals it.
  await page.goto(`/chat/dm/${HUMAN_MEMBER_ID}`, { waitUntil: 'domcontentloaded' });
  await expect.poll(() => currentPanel(page), { timeout: 15_000 }).toBe('center');
  // The rail's first load has to be over, so the reload below is a re-parse
  // of a route that is already open rather than the first parse.
  await expect(page.locator('.space-header', { hasText: PROJECT_A.name })).toBeAttached({
    timeout: 15_000,
  });
  await page.waitForTimeout(400);

  await sendFromComposer(page);
  await page.waitForTimeout(300);
  await swipeRightToRail(page);
  expect(await currentPanel(page)).toBe('left');

  await simulateChatMessageEcho(page);

  expect(
    await currentPanel(page),
    'the rail reload must not revert a manual swipe back to the rail'
  ).toBe('left');
});

test('a thread opened from a legacy link is not reverted or rebuilt when the rail first loads', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');
  await setupChatMobileMocks(page);
  await mockSuccessfulSend(page);
  // Hold the space list so the slug for the legacy URL stays unknown until
  // after the user has sent and swiped away, as on a slow network.
  let releaseSpaces: () => void = () => {};
  const spacesHeld = new Promise<void>((resolve) => {
    releaseSpaces = resolve;
  });
  await page.route(/\/api\/v1\/chat\/spaces$/, async (route) => {
    await spacesHeld;
    await route.fallback();
  });

  // Notification clicks and the terminal pane link to threads in this form.
  await page.goto(`/chat/space/${PROJECT_A.id}/thread/${GENERAL_THREAD_ID}`, {
    waitUntil: 'domcontentloaded',
  });
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
  expect(await currentPanel(page)).toBe('center');
  await page.waitForTimeout(400);
  await page.evaluate(() => {
    (window as unknown as { __chatPageBefore: Element | null }).__chatPageBefore =
      document.querySelector('scion-page-chat');
  });
  const historyBefore = await page.evaluate(() => history.length);

  await sendFromComposer(page);
  await page.waitForTimeout(300);
  await swipeRightToRail(page);
  expect(await currentPanel(page)).toBe('left');

  releaseSpaces();
  await page.waitForURL(new RegExp(`/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}$`), {
    timeout: 15_000,
  });
  // Give a rebuilt page, if there were one, time to load its own rail and
  // re-open the thread.
  await page.waitForTimeout(1_500);

  expect(
    await page.evaluate(
      () =>
        document.querySelector('scion-page-chat') ===
        (window as unknown as { __chatPageBefore: Element | null }).__chatPageBefore
    ),
    'rewriting the legacy URL must not rebuild the page'
  ).toBe(true);
  expect(
    await page.evaluate(() => history.length),
    'rewriting the legacy URL must not add a history entry Back would land on'
  ).toBe(historyBefore);
  expect(
    await currentPanel(page),
    'rewriting the legacy URL must not revert a manual swipe back to the rail'
  ).toBe('left');
});

test('a reloaded thread is not reverted when its slow slug lookup resolves after Send', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');
  await setupChatMobileMocks(page);
  await mockSuccessfulSend(page);
  // A cold load of a readable thread URL (a reload, or a shared link) looks
  // the slug up while the rail loads. Hold that lookup until after the user
  // has sent and swiped away; the rail opens the thread in the meantime.
  let releaseLookup: () => void = () => {};
  const lookupHeld = new Promise<void>((resolve) => {
    releaseLookup = resolve;
  });
  await page.route(/\/api\/v1\/projects\?slug=/, async (route) => {
    await lookupHeld;
    await route.fulfill({
      json: { items: [{ id: PROJECT_A.id, slug: PROJECT_A.slug, name: PROJECT_A.name }] },
    });
  });

  await page.goto(`/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}`, {
    waitUntil: 'domcontentloaded',
  });
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
  expect(await currentPanel(page)).toBe('center');
  await page.waitForTimeout(400);

  await sendFromComposer(page);
  await page.waitForTimeout(300);
  await swipeRightToRail(page);
  expect(await currentPanel(page)).toBe('left');

  const lookupDone = page.waitForResponse(/\/api\/v1\/projects\?slug=/);
  releaseLookup();
  await lookupDone;
  await page.waitForTimeout(500);

  expect(
    await currentPanel(page),
    'a late slug lookup must not revert a manual swipe back to the rail'
  ).toBe('left');
});

test('rewriting a legacy thread link in place keeps the router and the open thread in step', async ({
  page,
}) => {
  await setupChatMobileMocks(page);
  let releaseSpaces: () => void = () => {};
  const spacesHeld = new Promise<void>((resolve) => {
    releaseSpaces = resolve;
  });
  await page.route(/\/api\/v1\/chat\/spaces$/, async (route) => {
    await spacesHeld;
    await route.fallback();
  });

  // A query and a hash must survive the rewrite, and the router records the
  // query with the path the way a render does.
  const query = '?x=1';
  const hash = '#msg-abc';
  await page.goto(`/chat/space/${PROJECT_A.id}/thread/${GENERAL_THREAD_ID}${query}${hash}`, {
    waitUntil: 'domcontentloaded',
  });
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 15_000 });
  await expect.poll(() => page.title(), { timeout: 5_000 }).toMatch(/^Thread\b/);
  const titleBefore = await page.title();

  releaseSpaces();
  const readable = `/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}`;
  await page.waitForURL((url) => url.pathname === readable, { timeout: 15_000 });
  expect(new URL(page.url()).search, 'the rewrite must keep the query').toBe(query);
  expect(new URL(page.url()).hash, 'the rewrite must keep the hash').toBe(hash);
  await page.waitForTimeout(500);

  const state = await page.evaluate(() => {
    const shell = document.querySelector('scion-chat-shell') as unknown as {
      currentPath: string;
    } | null;
    const pageEl = document.querySelector('scion-page-chat') as unknown as {
      v2Conversation: { projectSlug: string } | null;
    } | null;
    return {
      currentPath: shell?.currentPath ?? null,
      projectSlug: pageEl?.v2Conversation?.projectSlug ?? null,
    };
  });
  expect(state.currentPath, "the router's rendered path must follow the rewrite").toBe(
    `${readable}${query}`
  );
  expect(state.projectSlug, 'the open thread must carry the slug the URL now names').toBe(
    PROJECT_A.slug
  );
  expect(await page.title(), 'the rewrite must not replace the thread title').toBe(titleBefore);
});

test('a thread opened from a reloaded space link is not left when the slow slug lookup resolves', async ({
  page,
}, testInfo) => {
  const mobile = testInfo.project.name !== 'desktop-1440';
  await setupChatMobileMocks(page);
  // A cold load of a space URL (a reload while on the rail, or a shared
  // link) looks the slug up while the rail loads. Hold that lookup until the
  // user has opened a thread from the rail.
  let releaseLookup: () => void = () => {};
  const lookupHeld = new Promise<void>((resolve) => {
    releaseLookup = resolve;
  });
  await page.route(/\/api\/v1\/projects\?slug=/, async (route) => {
    await lookupHeld;
    await route.fulfill({
      json: { items: [{ id: PROJECT_A.id, slug: PROJECT_A.slug, name: PROJECT_A.name }] },
    });
  });

  await page.goto(`/chat/${PROJECT_A.slug}`, { waitUntil: 'domcontentloaded' });
  if (!mobile) {
    // Desktop opens the space's #general on its own once the rail loads.
    await page.waitForURL(new RegExp(`/chat/${PROJECT_A.slug}/${GENERAL_THREAD_ID}$`), {
      timeout: 15_000,
    });
  }
  const chosen = 'thread-04';
  const row = page.locator('.thread-item', { hasText: `${chosen} discussion` }).first();
  await expect(row).toBeVisible({ timeout: 15_000 });
  await row.click();
  await page.waitForURL(new RegExp(`/chat/${PROJECT_A.slug}/${chosen}$`), { timeout: 10_000 });
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 10_000 });
  await page.waitForTimeout(400);
  if (mobile) expect(await currentPanel(page)).toBe('center');

  const lookupDone = page.waitForResponse(/\/api\/v1\/projects\?slug=/);
  releaseLookup();
  await lookupDone;
  await page.waitForTimeout(1_000);

  expect(
    new URL(page.url()).pathname,
    'a late slug lookup must not navigate away from the thread the user opened'
  ).toBe(`/chat/${PROJECT_A.slug}/${chosen}`);
  if (mobile) {
    expect(
      await currentPanel(page),
      'a late slug lookup must not send the user back to the rail'
    ).toBe('center');
  }
});
