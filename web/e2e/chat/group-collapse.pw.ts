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
 * Real-browser verification that thread-group collapse survives a reload
 * (nc-group-collapse brief). Everything the chat page and space rail fetch
 * is mocked via page.route — this exercises the actual Lit component,
 * actual localStorage, and an actual full-page reload in Chromium, which is
 * the one thing the Vitest unit tests (jsdom-ish happy-dom) can't prove: that
 * there is no expand-then-collapse flash and that a real `location.reload()`
 * really does restore the collapsed state.
 */

import { test, expect, type Page } from '@playwright/test';

const projectId = 'fixture-project';
const generalThreadId = '11111111-1111-4111-8111-111111111111';
const groupedThreadId = '22222222-2222-4222-8222-222222222222';
const groupId = 'g-fixture-group';
const fixtureUserId = 'fixture-user';

async function setupChat(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      'web.native_chat': true,
    };
    // Suppress EventSource (SSE) — no real server behind these mocks.
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: fixtureUserId, email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => route.fulfill({ json: { agents: [] } }));
  await page.route('**/api/v1/users**', (route) => route.fulfill({ json: { users: [] } }));

  // Chat spaces (space rail) — one space, matched only for the bare list
  // endpoint so the per-space routes below can still fall through to it.
  await page.route('**/api/v1/chat/spaces', (route) => {
    const url = route.request().url();
    if (/\/spaces\/[^/]+/.test(url.split('/api/v1/chat/')[1] || '')) {
      void route.fallback();
      return;
    }
    void route.fulfill({
      json: {
        spaces: [
          {
            projectId,
            projectSlug: 'fixture-proj',
            projectName: 'Fixture Project',
            unreadCount: 0,
            hasUnreadMention: false,
          },
        ],
      },
    });
  });

  // Two threads: #general, and one thread that belongs to "My Group".
  await page.route(/\/api\/v1\/chat\/spaces\/[^/]+\/threads/, (route) =>
    route.fulfill({
      json: {
        threads: [
          {
            id: generalThreadId,
            name: 'general',
            isGeneral: true,
            pinned: false,
            hasUnread: false,
            hasUnreadMention: false,
          },
          {
            id: groupedThreadId,
            name: 'grouped-thread',
            isGeneral: false,
            pinned: false,
            hasUnread: false,
            hasUnreadMention: false,
          },
        ],
      },
    })
  );

  await page.route(/\/api\/v1\/chat\/spaces\/[^/]+\/members/, (route) =>
    route.fulfill({ json: { agents: [], humans: [] } })
  );

  // User prefs: one server-side thread group ("My Group") containing the
  // grouped thread. Collapse state itself is NOT part of this payload — it
  // is the client-only localStorage state under test.
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
          [projectId]: [{ id: groupId, name: 'My Group', threadIds: [groupedThreadId] }],
        }),
      },
    });
  });

  await page.route('**/api/v1/chat/dms**', (route) => {
    if (route.request().url().includes('/unread')) {
      void route.fulfill({ json: { peerIds: [] } });
      return;
    }
    void route.fulfill({ json: { dms: [] } });
  });

  await page.route(/\/api\/v1\/chat\/topics\//, (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/chat/presence', (route) => route.fulfill({ json: {} }));
  await page.route(/\/api\/v1\/chat\/conversations\//, (route) => route.fulfill({ json: {} }));
  await page.route(/\/api\/v1\/chat\/messages/, (route) =>
    route.fulfill({ json: { messages: [] } })
  );

  await page.goto(`/chat/space/${projectId}`, { waitUntil: 'domcontentloaded' });
}

/**
 * Wait for the fixture space to be expanded and its "My Group" header shown.
 *
 * The page is opened on the space's deep link, so once the page has resolved
 * the route it opens a conversation in that space and the rail expands the
 * routed space by itself. That can land after the space header is already on
 * screen (still collapsed, the first-load default). The test does not click
 * the header to expand it: the click would race that route-driven expansion,
 * and a click landing just after it would collapse the space again, leaving
 * the group header permanently absent. Instead, wait for the expansion with
 * auto-retrying assertions.
 */
async function waitForGroupHeader(page: Page) {
  const spaceHeader = page.locator('.space-header', { hasText: 'Fixture Project' });
  await expect(spaceHeader).toBeVisible({ timeout: 15_000 });
  await expect(spaceHeader.locator('.chevron')).not.toHaveClass(/collapsed/, {
    timeout: 15_000,
  });

  const groupHeader = page.locator('.thread-group-header', { hasText: 'My Group' });
  await expect(groupHeader).toBeVisible({ timeout: 10_000 });
  return groupHeader;
}

/** The group IDs the rail has persisted as collapsed for the fixture user. */
async function persistedCollapsedGroupIds(page: Page): Promise<unknown> {
  return page.evaluate((key) => {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as unknown) : null;
  }, `scion-chat-group-collapse:${fixtureUserId}`);
}

test.describe('Thread-group collapse persists across reload (nc-group-collapse)', () => {
  test('collapsing a group survives a full page reload', async ({ page }) => {
    await setupChat(page);

    const groupHeader = await waitForGroupHeader(page);
    const groupedThreadItem = page.locator('.thread-item', { hasText: 'grouped-thread' });
    await expect(groupedThreadItem).toBeVisible();
    await expect(groupHeader.locator('.chevron')).not.toHaveClass(/collapsed/);

    // Collapse the group.
    await groupHeader.click();
    await expect(groupHeader.locator('.chevron')).toHaveClass(/collapsed/);
    await expect(groupedThreadItem).toBeHidden();
    // Reload only once the collapse is in storage, so a failure after the
    // reload can only mean the state was not restored.
    await expect.poll(() => persistedCollapsedGroupIds(page)).toEqual([groupId]);

    // A real full-page reload — this is the case that used to always come
    // back fully expanded.
    await page.reload({ waitUntil: 'domcontentloaded' });

    const groupHeaderAfterReload = await waitForGroupHeader(page);
    await expect(groupHeaderAfterReload.locator('.chevron')).toHaveClass(/collapsed/);
    await expect(page.locator('.thread-item', { hasText: 'grouped-thread' })).toBeHidden();
  });
});
