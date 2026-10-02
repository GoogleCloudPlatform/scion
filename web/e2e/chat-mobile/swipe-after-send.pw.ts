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
