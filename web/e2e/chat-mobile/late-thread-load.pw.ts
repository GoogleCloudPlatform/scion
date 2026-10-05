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
 * A slow answer for a thread the user has already left must not bring that
 * thread back: its URL, its header and its messages all stay on the thread
 * the user moved to.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { PROJECT_A } from './mock-api.js';

const THREAD_A = { id: 'thread-05', name: 'thread-05 discussion' };
const THREAD_B = { id: 'thread-06', name: 'thread-06 discussion' };

function messagesFor(threadId: string): Record<string, unknown>[] {
  const base = Date.parse('2026-09-01T00:00:00Z');
  return Array.from({ length: 3 }, (_, i) => ({
    id: `${threadId}-msg-${i}`,
    projectId: PROJECT_A.id,
    sender: 'user',
    senderId: 'user-ada',
    recipient: 'thread',
    recipientId: threadId,
    msg: `${threadId} message ${i}`,
    type: 'chat',
    threadId,
    createdAt: new Date(base + i * 60_000).toISOString(),
  }));
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

/** Back to the rail on phones, where the open thread covers it. */
async function backToRail(page: Page): Promise<void> {
  const back = page.locator('.v2-thread-header .mobile-back:visible');
  if ((await back.count()) > 0) await back.first().click();
}

test('a late history for a thread the user left does not bring it back', async ({ page }) => {
  let releaseA: () => void = () => {};
  const heldA = new Promise<void>((resolve) => (releaseA = resolve));
  let requestedA = false;

  await openChatRail(page, async (p) => {
    await p.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, async (route) => {
      const url = route.request().url();
      if (url.includes(`/conversations/${THREAD_A.id}/`)) {
        requestedA = true;
        await heldA;
        const items = messagesFor(THREAD_A.id);
        await route.fulfill({ json: { items, messages: items } });
        return;
      }
      const items = url.includes(`/conversations/${THREAD_B.id}/`) ? messagesFor(THREAD_B.id) : [];
      await route.fulfill({ json: { items, messages: items } });
    });
  });
  await openGeneralThread(page);

  await backToRail(page);
  await page.locator('.thread-item', { hasText: THREAD_A.name }).first().click();
  await expect(page).toHaveURL(`/chat/${PROJECT_A.slug}/${THREAD_A.id}`);
  await expect.poll(() => requestedA).toBe(true);

  await backToRail(page);
  await page.locator('.thread-item', { hasText: THREAD_B.name }).first().click();
  const pathB = `/chat/${PROJECT_A.slug}/${THREAD_B.id}`;
  await expect(page).toHaveURL(pathB);
  await expect(page.getByText(`${THREAD_B.id} message 2`)).toBeVisible({ timeout: 10_000 });

  releaseA();
  // Give the released answer time to land and render if it were going to.
  await page.waitForTimeout(500);

  await expect(page).toHaveURL(pathB);
  expect(await openKey(page)).toBe(THREAD_B.id);
  await expect(page.locator('.v2-thread-header')).toContainText(THREAD_B.name);
  await expect(page.getByText(`${THREAD_B.id} message 2`)).toBeVisible();
  await expect(page.getByText(`${THREAD_A.id} message`)).toHaveCount(0);
});
