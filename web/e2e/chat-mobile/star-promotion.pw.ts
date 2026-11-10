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
 * Starred messages across a DM promotion (web.chat_starred_messages), with
 * mock data. The hub's promotion path (both dialects, modern and legacy
 * rows, rollback, access re-check) is covered by the Go store and handler
 * tests; this case covers the browser side: a DM's star list is open when
 * the DM is promoted, and
 *
 * - with the promotion event (the same subject and payload the hub
 *   publishes, user.<id>.chat.dm.promoted, plus the stars invalidation),
 *   the page moves to the new thread, whose star list holds the moved star
 *   and jumps to it in the new thread;
 * - with the event missed, clicking the stale DM row re-resolves the star
 *   through the detail endpoint and lands in the new thread, never the old
 *   (now empty) DM.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { setupChatMobileMocks, PROJECT_A, AGENT_MEMBER_ID } from './mock-api.js';
import { emitEventSourceUpdate } from '../event-source-stub.js';

const USER_ID = 'fixture-user';
const DM_KEY = `dm:agent:${AGENT_MEMBER_ID}:user:${USER_ID}`;
const TOPIC_ID = '7a3e0c4e-0000-4000-8000-0000000000aa';
const TOPIC_NAME = 'Promoted stars';
const TARGET = '7a3e0c4e-0000-4000-8000-000000000001';
const OTHERS = [2, 3, 4].map((n) => `7a3e0c4e-0000-4000-8000-00000000000${n}`);
const TARGET_TEXT = 'Mock DM star: keep the retry budget small.';

function messages(threadId: string): Record<string, unknown>[] {
  const base = Date.parse('2026-10-20T10:00:00Z');
  return [TARGET, ...OTHERS].map((id, i) => ({
    id,
    projectId: PROJECT_A.id,
    sender: i % 2 === 0 ? 'user:fixture@example.test' : 'agent:coder-one',
    senderId: i % 2 === 0 ? USER_ID : AGENT_MEMBER_ID,
    recipient: 'chat',
    msg: i === 0 ? TARGET_TEXT : `Mock DM message ${i}`,
    type: 'chat',
    channel: 'web',
    threadId,
    createdAt: new Date(base + i * 60_000).toISOString(),
  }));
}

function starItem(promoted: boolean): Record<string, unknown> {
  const common = {
    messageId: TARGET,
    senderName: 'Fixture User',
    messageCreatedAt: '2026-10-20T10:00:00Z',
    starredAt: '2026-10-22T10:00:00Z',
    excerpt: TARGET_TEXT,
    edited: false,
    projectId: PROJECT_A.id,
    projectName: PROJECT_A.name,
  };
  return promoted
    ? {
        ...common,
        conversationKey: TOPIC_ID,
        conversationKind: 'thread',
        conversationName: TOPIC_NAME,
        permalink: `/chat/space/${PROJECT_A.id}/thread/${TOPIC_ID}#msg-${TARGET}`,
      }
    : {
        ...common,
        conversationKey: DM_KEY,
        conversationKind: 'dm',
        conversationName: 'Coder One',
        permalink: `/chat/dm/${encodeURIComponent(DM_KEY)}#msg-${TARGET}`,
      };
}

interface HubState {
  promoted: boolean;
  detailCalls: number;
  listKeys: string[];
}

/** The mocked hub for the DM, its promotion and the star routes. */
async function setupMocks(page: Page): Promise<HubState> {
  const hub: HubState = { promoted: false, detailCalls: 0, listKeys: [] };
  await setupChatMobileMocks(page);
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      ...(window.__SCION_FEATURES__ ?? {}),
      'web.chat_starred_messages': true,
    };
  });
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: USER_ID, email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/chat/dms**', (route) => {
    if (route.request().url().includes('/unread')) {
      void route.fulfill({ json: { peerIds: [] } });
      return;
    }
    const dms = hub.promoted
      ? []
      : [
          {
            conversationKey: DM_KEY,
            peerId: AGENT_MEMBER_ID,
            peerKind: 'agent',
            peerName: 'Coder One',
            peerSlug: 'coder-one',
            lastActivityAt: '2026-10-20T10:03:00Z',
          },
        ];
    void route.fulfill({ json: { dms } });
  });
  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/threads/, (route) => {
    const threads = [
      {
        id: 'thread-01',
        name: 'general',
        isGeneral: true,
        pinned: false,
        hasUnread: false,
        hasUnreadMention: false,
      },
    ];
    if (hub.promoted && route.request().url().includes(PROJECT_A.id)) {
      threads.push({
        id: TOPIC_ID,
        name: TOPIC_NAME,
        isGeneral: false,
        pinned: false,
        hasUnread: false,
        hasUnreadMention: false,
      });
    }
    void route.fulfill({ json: { threads } });
  });
  await page.route(/\/api\/v1\/chat\/topics\//, (route) =>
    route.fulfill({
      json: hub.promoted ? { id: TOPIC_ID, projectId: PROJECT_A.id, name: TOPIC_NAME } : {},
    })
  );
  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const match = route
      .request()
      .url()
      .match(/\/conversations\/([^/?]+)\/messages/);
    const key = match ? decodeURIComponent(match[1]) : '';
    // Promotion moves the DM's messages into the thread: the DM is empty.
    const items =
      (key === DM_KEY && !hub.promoted) || (key === TOPIC_ID && hub.promoted) ? messages(key) : [];
    void route.fulfill({ json: { items, messages: items } });
  });
  await page.route(/\/api\/v1\/chat\/stars(\/|\?|$)/, (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (path === '/api/v1/chat/stars/state') {
      const ids = url.searchParams.getAll('messageId');
      void route.fulfill({ json: { starredMessageIds: ids.filter((id) => id === TARGET) } });
      return;
    }
    if (path === '/api/v1/chat/stars') {
      const key = url.searchParams.get('conversationKey') ?? '';
      hub.listKeys.push(key);
      const current = hub.promoted ? TOPIC_ID : DM_KEY;
      void route.fulfill({ json: { items: key === current ? [starItem(hub.promoted)] : [] } });
      return;
    }
    if (path === `/api/v1/chat/stars/${TARGET}`) {
      hub.detailCalls++;
      void route.fulfill({ json: starItem(hub.promoted) });
      return;
    }
    void route.fulfill({ status: 404, json: { error: { code: 'not_found', message: 'x' } } });
  });
  return hub;
}

/** The thread component's conversation key. */
function openKey(page: Page): Promise<string> {
  return page.evaluate(
    () =>
      (
        document
          .querySelector('scion-page-chat')
          ?.shadowRoot?.querySelector('scion-chat-thread') as
          | (HTMLElement & { conversationKey: string })
          | null
      )?.conversationKey ?? ''
  );
}

const indicator = (page: Page): Locator =>
  page.locator(`#msg-${TARGET} [aria-label="Starred by you"]`);
const listButton = (page: Page): Locator =>
  page.getByRole('button', { name: 'Starred messages in this conversation' });
const entry = (page: Page): Locator =>
  page.locator('scion-chat-starred-list a.entry', { hasText: 'Mock DM star' });

/** Open the DM and its star list, with the star shown in both places. */
async function openDMWithList(page: Page): Promise<void> {
  await page.goto(`/chat/dm/${encodeURIComponent(DM_KEY)}`, { waitUntil: 'domcontentloaded' });
  await expect.poll(() => openKey(page), { timeout: 15_000 }).toBe(DM_KEY);
  await expect(page.getByText(TARGET_TEXT)).toBeVisible({ timeout: 15_000 });
  await expect(indicator(page)).toBeVisible({ timeout: 10_000 });
  await listButton(page).click();
  await expect(entry(page)).toBeVisible();
}

test.describe('starred messages across a DM promotion', () => {
  test.beforeEach(({ page }) => {
    test.skip(
      (page.viewportSize()?.width ?? 0) <= 768,
      'desktop header list; the phone sheet is covered by the real-hub spec'
    );
  });

  test('the promotion event moves the open collection to the new thread', async ({ page }) => {
    const hub = await setupMocks(page);
    await openDMWithList(page);

    hub.promoted = true;
    await emitEventSourceUpdate(page, `user.${USER_ID}.chat.dm.promoted`, {
      oldConversationKey: DM_KEY,
      newTopic: { id: TOPIC_ID, projectId: PROJECT_A.id, name: TOPIC_NAME },
    });
    await emitEventSourceUpdate(page, `user.${USER_ID}.chat.stars`, { action: 'changed' });

    // The page follows the promotion; the old DM is not left open.
    await expect.poll(() => openKey(page), { timeout: 15_000 }).toBe(TOPIC_ID);
    await expect(page).toHaveURL(new RegExp(`${TOPIC_ID}`), { timeout: 15_000 });
    await expect(page.getByText(TARGET_TEXT)).toBeVisible({ timeout: 15_000 });
    await expect(indicator(page)).toBeVisible({ timeout: 10_000 });

    // The page announces the promotion; dismiss the toast, which sits over
    // the header actions.
    const toast = page.locator('.sl-toast-stack sl-alert', { hasText: `#${TOPIC_NAME}` });
    await expect(toast).toBeVisible();
    await toast.evaluate((el) => (el as HTMLElement & { hide(): Promise<void> }).hide());
    await expect(toast).toHaveCount(0);

    // The thread's own star list holds the moved star; its entry jumps
    // within the new thread.
    await listButton(page).click();
    await expect(entry(page)).toBeVisible();
    expect(hub.listKeys).toContain(TOPIC_ID);
    const detailsBefore = hub.detailCalls;
    await entry(page).click();
    await expect.poll(() => hub.detailCalls).toBe(detailsBefore + 1);
    await expect(page.locator(`#msg-${TARGET}`)).toBeInViewport({ timeout: 15_000 });
    expect(await openKey(page)).toBe(TOPIC_ID);
  });

  test('a stale DM row re-resolves through the detail endpoint after a missed promotion event', async ({
    page,
  }) => {
    const hub = await setupMocks(page);
    await openDMWithList(page);

    // The DM is promoted elsewhere and this tab misses the event: its list
    // still shows the DM row with the DM permalink.
    hub.promoted = true;
    await expect(entry(page)).toHaveAttribute('href', new RegExp('^/chat/dm/'));
    const detailsBefore = hub.detailCalls;
    await entry(page).click();

    // The click asks the hub for the star's current target and opens the new
    // thread, not the old (now empty) DM.
    await expect.poll(() => hub.detailCalls).toBe(detailsBefore + 1);
    await expect.poll(() => openKey(page), { timeout: 15_000 }).toBe(TOPIC_ID);
    await expect(page).toHaveURL(new RegExp(`${TOPIC_ID}(?:[?#].*)?$`), { timeout: 15_000 });
    await expect(page.locator(`#msg-${TARGET}`)).toBeInViewport({ timeout: 15_000 });
  });
});
