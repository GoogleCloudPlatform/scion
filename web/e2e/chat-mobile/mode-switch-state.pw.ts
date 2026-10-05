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
 * Switching from chat to the dashboard or the terminal view and back keeps
 * the open thread and where the user was scrolled to in it. The dashboard
 * swaps the whole chat shell out (a fresh chat page is built on the way
 * back); the terminal view only hides it. The one deliberate exception is
 * the terminal pane's chat button, which jumps to that agent's DM.
 *
 * The thread is opened from the rail (an in-place URL change, not a full
 * route render) — the path where the remembered chat URL used to go stale.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { PROJECT_A } from './mock-api.js';

/** A UUID user, so DM keys built for it pass the page's DM-key check. */
const USER_ID = '99999999-9999-4999-8999-999999999999';
const AGENT_ID = '11111111-1111-4111-8111-111111111111';
const AGENT_NAME = 'alpha-agent';
const THREAD_ID = 'thread-05';
const THREAD_NAME = 'thread-05 discussion';
const THREAD_PATH = `/chat/${PROJECT_A.slug}/${THREAD_ID}`;

/** Pixels the anchor message may drift across a round trip. */
const TOLERANCE_PX = 4;

function threadMessages(): Record<string, unknown>[] {
  const base = Date.parse('2026-09-01T00:00:00Z');
  return Array.from({ length: 40 }, (_, i) => ({
    id: `${THREAD_ID}-msg-${i}`,
    projectId: PROJECT_A.id,
    sender: 'user',
    senderId: 'user-ada',
    recipient: 'thread',
    recipientId: THREAD_ID,
    msg: `Message ${i}. ` + 'Some words to give the row a little height. '.repeat(3),
    type: 'chat',
    threadId: THREAD_ID,
    createdAt: new Date(base + i * 60_000).toISOString(),
  }));
}

/** Extra mocks: terminal workspace on, a UUID user, one attachable agent. */
async function modeSwitchMocks(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      ...(window.__SCION_FEATURES__ ?? {}),
      'web.terminal_workspace': true,
    };
  });
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: '99999999-9999-4999-8999-999999999999', email: 'u@example.test' } })
  );
  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const url = route.request().url();
    const items = url.includes(`/conversations/${THREAD_ID}/`) ? threadMessages() : [];
    void route.fulfill({ json: { items, messages: items } });
  });
  const agent = {
    id: AGENT_ID,
    name: AGENT_NAME,
    slug: AGENT_NAME,
    phase: 'running',
    projectId: PROJECT_A.id,
    canAttach: true,
    _capabilities: { actions: ['attach'] },
  };
  await page.route('**/api/v1/agents/**', (route) => {
    const url = route.request().url();
    if (/\/agents\/[^/]+\/[a-z]+/.test(url)) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({ json: agent });
  });
  await page.routeWebSocket('**/pty?*', () => {});
}

const scrollerHandle = `
  document.querySelector('scion-page-chat')?.shadowRoot
    ?.querySelector('scion-chat-thread')?.shadowRoot
    ?.querySelector('.messages-scroll')
`;

/** The open thread's conversation key and its topmost visible message. */
async function viewState(page: Page): Promise<{
  key: string;
  messageId: string;
  offset: number;
} | null> {
  return page.evaluate((expr) => {
    const thread = document
      .querySelector('scion-page-chat')
      ?.shadowRoot?.querySelector('scion-chat-thread') as
      | (HTMLElement & { conversationKey: string })
      | null;
    const scroller = (0, eval)(expr) as HTMLElement | null;
    if (!thread || !scroller) return null;
    const top = scroller.getBoundingClientRect().top;
    const rows = Array.from(scroller.querySelectorAll('scion-chat-message[id^="msg-"]'));
    const row = rows.find((r) => r.getBoundingClientRect().bottom > top);
    if (!row) return null;
    return {
      key: thread.conversationKey,
      messageId: row.id,
      offset: row.getBoundingClientRect().top - top,
    };
  }, scrollerHandle);
}

/** Use the header's mode switch: segmented buttons, or the narrow dropdown. */
async function switchMode(page: Page, mode: 'Dashboard' | 'Chat' | 'Terminal'): Promise<void> {
  const header = page.locator('scion-header:visible').first();
  const button = header.locator('.mode-switch button:visible', { has: page.getByText(mode) });
  if ((await button.count()) > 0) {
    await button.first().click();
    return;
  }
  await header.locator('.mode-trigger').click();
  await header.locator('sl-menu-item', { hasText: mode }).click();
}

async function openScrolledThread(page: Page): Promise<{ messageId: string; offset: number }> {
  await openChatRail(page, modeSwitchMocks);
  await openGeneralThread(page);
  // Back to the rail on phones, where the thread covers it.
  const back = page.locator('.v2-thread-header .mobile-back:visible');
  if ((await back.count()) > 0) await back.first().click();
  await page.locator('.thread-item', { hasText: THREAD_NAME }).first().click();
  await expect(page).toHaveURL(THREAD_PATH);
  await expect(page.locator('scion-chat-message').first()).toBeVisible({ timeout: 10_000 });

  // Park the view part-way up the history, off the bottom.
  await page.evaluate((expr) => {
    const scroller = (0, eval)(expr) as HTMLElement;
    scroller.scrollTop = Math.round(scroller.scrollHeight * 0.4);
  }, scrollerHandle);
  await expect(page.locator('.jump-to-latest')).toBeVisible();
  // Let the rAF-throttled position capture run.
  await page.waitForTimeout(200);
  const before = await viewState(page);
  expect(before?.key).toBe(THREAD_ID);
  return { messageId: before!.messageId, offset: before!.offset };
}

async function expectSamePosition(
  page: Page,
  before: { messageId: string; offset: number }
): Promise<void> {
  await expect(page).toHaveURL(THREAD_PATH);
  await expect
    .poll(async () => {
      const now = await viewState(page);
      if (!now || now.key !== THREAD_ID || now.messageId !== before.messageId) return Infinity;
      return Math.abs(now.offset - before.offset);
    })
    .toBeLessThanOrEqual(TOLERANCE_PX);
}

test('thread and scroll position survive dashboard and terminal round trips', async ({ page }) => {
  const before = await openScrolledThread(page);

  await switchMode(page, 'Dashboard');
  await expect(page).toHaveURL('/');
  await expect(page.locator('scion-page-chat')).toHaveCount(0);
  await switchMode(page, 'Chat');
  await expectSamePosition(page, before);

  await switchMode(page, 'Terminal');
  await expect(page).toHaveURL(/\/terminals/);
  await switchMode(page, 'Chat');
  await expectSamePosition(page, before);
});

test('the terminal pane chat button opens the agent DM, not the old thread', async ({ page }) => {
  await openScrolledThread(page);

  await page.evaluate(
    (id) =>
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` } })
      ),
    AGENT_ID
  );
  await expect(page).toHaveURL(`/terminals/${AGENT_ID}`);
  await page.locator(`button[aria-label="Chat with ${AGENT_NAME}"]:visible`).first().click();

  const dmKey = `dm:agent:${AGENT_ID}:user:${USER_ID}`;
  await expect(page).toHaveURL(`/chat/dm/${encodeURIComponent(dmKey)}`);
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            document
              .querySelector('scion-page-chat')
              ?.shadowRoot?.querySelector('scion-chat-thread') as
              | (HTMLElement & { conversationKey: string })
              | null
          )?.conversationKey ?? ''
      )
    )
    .toBe(dmKey);
});
