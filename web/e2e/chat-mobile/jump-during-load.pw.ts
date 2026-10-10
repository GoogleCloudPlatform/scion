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
 * A search result in the conversation already open closes search, which
 * mounts the thread again and starts the jump while the thread's initial
 * history is still loading. The jump must still land on its target once
 * that history arrives, rather than the view falling back to the bottom.
 *
 * Only the body of the re-mounted thread's latest-page request is held; the
 * jump's own request for the page around its target is answered at once.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { GENERAL_THREAD_ID, PROJECT_A } from './mock-api.js';

const TARGET_ID = 'jump-target';
const TARGET_TEXT = 'the message search jumped to';

function message(id: string, text: string, minute: number): Record<string, unknown> {
  return {
    id,
    projectId: PROJECT_A.id,
    sender: 'user',
    senderId: 'user-ada',
    recipient: 'thread',
    recipientId: GENERAL_THREAD_ID,
    msg: text,
    type: 'chat',
    threadId: GENERAL_THREAD_ID,
    createdAt: new Date(Date.parse('2026-08-01T00:00:00Z') + minute * 60_000).toISOString(),
  };
}

/** Ten messages either side of the target, all older than the latest page. */
function aroundTarget(): Record<string, unknown>[] {
  return Array.from({ length: 21 }, (_, i) =>
    i === 10 ? message(TARGET_ID, TARGET_TEXT, i) : message(`older-${i}`, `older message ${i}`, i)
  );
}

/** Once armed, hold `Response.json()` for the latest page until released. */
async function holdLatestPageBody(page: Page): Promise<void> {
  await page.addInitScript((threadId) => {
    const w = window as unknown as {
      __holdArmed?: boolean;
      __bodyHeld?: boolean;
      __releaseBody?: () => void;
    };
    const released = new Promise<void>((resolve) => (w.__releaseBody = resolve));
    const json = Response.prototype.json;
    Response.prototype.json = async function (this: Response): Promise<unknown> {
      const latest =
        this.url.includes(`/conversations/${threadId}/messages`) && !this.url.includes('around=');
      if (w.__holdArmed && latest) {
        w.__bodyHeld = true;
        await released;
      }
      return json.call(this);
    };
  }, GENERAL_THREAD_ID);
}

test('a search jump in the open thread during its initial load lands on the target', async ({
  page,
}) => {
  await openChatRail(page, async (p) => {
    await holdLatestPageBody(p);
    await p.route(
      new RegExp(`/api/v1/chat/conversations/${GENERAL_THREAD_ID}/messages\\?.*around=`),
      (route) => {
        const items = aroundTarget();
        // No older cursor: an older-page load would move the view itself.
        void route.fulfill({ json: { items, messages: items } });
      }
    );
    await p.route(/\/api\/v1\/chat\/search/, (route) => {
      void route.fulfill({
        json: {
          results: [
            {
              messageId: TARGET_ID,
              conversationKey: GENERAL_THREAD_ID,
              threadName: 'general',
              senderName: 'Ada',
              content: TARGET_TEXT,
              snippet: TARGET_TEXT,
              timestamp: '2026-08-01T00:10:00Z',
              projectId: PROJECT_A.id,
            },
          ],
        },
      });
    });
  });
  await openGeneralThread(page);

  await page.getByRole('button', { name: 'Search messages' }).click();
  await page.locator('.search-input').fill('jumped');
  const result = page.locator('.result-item', { hasText: TARGET_TEXT });
  await expect(result).toBeVisible({ timeout: 10_000 });

  await page.evaluate(() => {
    (window as unknown as { __holdArmed: boolean }).__holdArmed = true;
  });
  await result.click();
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __bodyHeld?: boolean }).__bodyHeld))
    .toBe(true);

  await page.evaluate(() => (window as unknown as { __releaseBody: () => void }).__releaseBody());
  // Give the released page time to land and move the view if it were going to.
  await page.waitForTimeout(800);

  const target = page.locator('scion-chat-message', { hasText: TARGET_TEXT });
  await expect(target).toBeInViewport();
  // The latest page is not spliced in under the window around the target.
  await expect(page.locator(`#msg-${GENERAL_THREAD_ID}-msg-39`)).toHaveCount(0);
});
