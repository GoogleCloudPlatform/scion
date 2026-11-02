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
 * The starred-only center (`?messages=starred`, web.chat_starred_messages),
 * with mock data and a mocked hub that serves the starred=true history
 * variant the way the hub does:
 *
 * - only starred messages, each standalone, with the banner in place of the
 *   composer; the legacy route's slug canonicalization and a reload keep
 *   the filter;
 * - the view never posts a read watermark, and an unstarred live message is
 *   not appended;
 * - Jump to message in conversation lands on a message outside the latest
 *   page of the full history, Back restores the filtered center at its
 *   position and Forward returns;
 * - Show all opens the full conversation; unstarring the last star leaves
 *   the empty center with Show all; rapid Back/Forward ends on the URL's view.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { setupChatMobileMocks, PROJECT_A } from './mock-api.js';
import { emitEventSourceUpdate } from '../event-source-stub.js';

const USER_ID = 'fixture-user';
const TOPIC_ID = '5c3e0c4e-0000-4000-8000-0000000000bb';
const TOPIC_NAME = 'Mock starred center';
const TOTAL = 120;
const PAGE = 50;

function id(n: number): string {
  return `5c3e0c4e-0000-4000-8000-${n.toString(16).padStart(12, '0')}`;
}

/** Starred: three outside the latest page of the full history, one inside. */
const STARRED_INDEXES = [3, 10, 60, 110];

function text(n: number): string {
  return STARRED_INDEXES.includes(n)
    ? `Mock starred message ${n}: the rollout starts after the migration.`
    : `Mock ordinary message ${n}`;
}

function message(n: number): Record<string, unknown> {
  const base = Date.parse('2026-10-20T08:00:00Z');
  return {
    id: id(n),
    projectId: PROJECT_A.id,
    // One sender throughout: the full history groups them, the filtered center must not.
    sender: 'user:mock.person@example.test',
    senderId: 'user-mock-person',
    recipient: 'chat',
    msg: text(n),
    type: 'chat',
    channel: 'web',
    threadId: TOPIC_ID,
    createdAt: new Date(base + n * 120_000).toISOString(),
  };
}

interface HubState {
  starred: Set<number>;
  readPosts: number;
  filteredCalls: number;
  aroundCalls: string[];
}

/** Newest first, as the hub answers; `range` is inclusive of both ends. */
function page(indexes: number[]): Record<string, unknown>[] {
  return [...indexes].sort((a, b) => b - a).map(message);
}

async function setupMocks(pw: Page): Promise<HubState> {
  const hub: HubState = {
    starred: new Set(STARRED_INDEXES),
    readPosts: 0,
    filteredCalls: 0,
    aroundCalls: [],
  };
  await setupChatMobileMocks(pw);
  await pw.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      ...(window.__SCION_FEATURES__ ?? {}),
      'web.chat_starred_messages': true,
    };
  });
  await pw.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: USER_ID, email: 'fixture@example.test' } })
  );
  await pw.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/threads/, (route) =>
    route.fulfill({
      json: {
        threads: [
          {
            id: TOPIC_ID,
            name: TOPIC_NAME,
            isGeneral: false,
            pinned: false,
            hasUnread: false,
            hasUnreadMention: false,
          },
        ],
      },
    })
  );
  await pw.route(/\/api\/v1\/chat\/topics\//, (route) =>
    route.fulfill({ json: { id: TOPIC_ID, projectId: PROJECT_A.id, name: TOPIC_NAME } })
  );
  await pw.route(/\/api\/v1\/chat\/conversations\/[^/]+\/read/, (route) => {
    if (route.request().method() === 'POST') hub.readPosts++;
    void route.fulfill({ json: {} });
  });
  await pw.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const url = new URL(route.request().url());
    const all = Array.from({ length: TOTAL }, (_, i) => i);
    const around = url.searchParams.get('around');
    const cursor = url.searchParams.get('cursor');
    if (url.searchParams.get('starred') === 'true') {
      hub.filteredCalls++;
      const starred = all.filter((n) => hub.starred.has(n));
      void route.fulfill({ json: { messages: page(starred), totalCount: starred.length } });
      return;
    }
    if (around) {
      hub.aroundCalls.push(around);
      const at = all.findIndex((n) => id(n) === around);
      const window = all.slice(Math.max(0, at - 25), at + 25);
      void route.fulfill({
        json: {
          messages: page(window),
          ...(window[0] > 0 ? { nextCursor: `c-${window[0]}` } : {}),
        },
      });
      return;
    }
    const end = cursor ? Number(cursor.slice(2)) : TOTAL;
    const slice = all.slice(Math.max(0, end - PAGE), end);
    void route.fulfill({
      json: { messages: page(slice), ...(slice[0] > 0 ? { nextCursor: `c-${slice[0]}` } : {}) },
    });
  });
  await pw.route(/\/api\/v1\/chat\/stars(\/|\?|$)/, (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    if (path === '/api/v1/chat/stars/state') {
      const ids = url.searchParams.getAll('messageId');
      const starredIds = new Set([...hub.starred].map(id));
      void route.fulfill({ json: { starredMessageIds: ids.filter((m) => starredIds.has(m)) } });
      return;
    }
    if (path === '/api/v1/chat/stars') {
      void route.fulfill({ json: { items: [] } });
      return;
    }
    const n = Array.from({ length: TOTAL }, (_, i) => i).find(
      (i) => path === `/api/v1/chat/stars/${id(i)}`
    );
    if (n === undefined) {
      void route.fulfill({ status: 404, json: { error: { code: 'not_found', message: 'x' } } });
      return;
    }
    if (method === 'DELETE') {
      hub.starred.delete(n);
      void route.fulfill({ status: 204, body: '' });
      return;
    }
    if (!hub.starred.has(n)) {
      void route.fulfill({ status: 404, json: { error: { code: 'not_found', message: 'x' } } });
      return;
    }
    void route.fulfill({
      json: {
        messageId: id(n),
        conversationKey: TOPIC_ID,
        conversationKind: 'thread',
        projectId: PROJECT_A.id,
        projectName: PROJECT_A.name,
        conversationName: TOPIC_NAME,
        senderName: 'Mock Person',
        messageCreatedAt: message(n).createdAt,
        starredAt: '2026-10-22T10:00:00Z',
        excerpt: text(n),
        edited: false,
        permalink: `/chat/space/${PROJECT_A.id}/thread/${TOPIC_ID}#msg-${id(n)}`,
      },
    });
  });
  return hub;
}

const filteredUrl = `/chat/space/${PROJECT_A.id}/thread/${TOPIC_ID}?messages=starred`;
const canonicalPath = `/chat/${PROJECT_A.slug}/${TOPIC_ID}`;

const row = (pw: Page, n: number): Locator => pw.locator(`#msg-${id(n)}`);
const banner = (pw: Page): Locator => pw.locator('scion-chat-thread .star-filter-banner');

/** The rendered message IDs of the open thread. */
function renderedIds(pw: Page): Promise<string[]> {
  return pw
    .locator('scion-chat-thread scion-chat-message')
    .evaluateAll((els) => els.map((el) => el.getAttribute('messageId') ?? ''));
}

function menuItem(pw: Page, label: string): Locator {
  const width = pw.viewportSize()?.width ?? Number.MAX_SAFE_INTEGER;
  return width <= 768
    ? pw.locator('scion-chat-thread scion-action-sheet[open] .item', { hasText: label })
    : pw.locator('.context-menu-item', { hasText: label });
}

/** Waits until the thread's list has kept its scroll position for 300 ms. */
async function scrollIdle(pw: Page): Promise<void> {
  const list = pw.locator('scion-chat-thread .messages-scroll');
  let last = Number.NaN;
  let since = Date.now();
  const deadline = Date.now() + 15_000;
  for (;;) {
    const top = await list.evaluate((el) => el.scrollTop);
    if (top !== last) {
      last = top;
      since = Date.now();
    } else if (Date.now() - since >= 300) {
      return;
    }
    if (Date.now() > deadline) throw new Error('the thread kept scrolling');
    await new Promise((r) => setTimeout(r, 50));
  }
}

/** Open a message's menu (popup or phone sheet) and choose `label`, retrying a dismissed popup. */
async function chooseFromMenu(pw: Page, n: number, label: string): Promise<void> {
  await scrollIdle(pw);
  const item = menuItem(pw, label);
  await expect(async () => {
    if (await item.isVisible()) return;
    await pw.keyboard.press('Escape');
    await row(pw, n).click({ button: 'right', timeout: 2_000 });
    await expect(item).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 15_000 });
  await item.click();
}

async function openFiltered(pw: Page): Promise<void> {
  await pw.goto(filteredUrl, { waitUntil: 'domcontentloaded' });
  await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
  await expect(row(pw, 110)).toBeVisible({ timeout: 15_000 });
}

test.describe('starred-only center', () => {
  test('shows only starred messages, standalone; canonicalization and reload keep the filter', async ({
    page: pw,
  }) => {
    await setupMocks(pw);
    await openFiltered(pw);
    await expect(pw).toHaveURL(new RegExp(`${canonicalPath}\\?messages=starred$`));
    await expect.poll(() => renderedIds(pw)).toEqual(STARRED_INDEXES.map(id));
    await expect(pw.getByText('Mock ordinary message', { exact: false })).toHaveCount(0);
    await expect(banner(pw)).toContainText('Starred messages only');
    await expect(banner(pw)).toContainText('omitted');
    await expect(pw.locator('scion-chat-thread scion-chat-composer')).toHaveCount(0);
    // Every row has its own author header despite the shared sender.
    const headers = await pw
      .locator('scion-chat-thread scion-chat-message')
      .evaluateAll((els) =>
        els.map((el) => (el as HTMLElement & { showHeader: boolean }).showHeader)
      );
    expect(headers).toEqual([true, true, true, true]);

    await pw.reload({ waitUntil: 'domcontentloaded' });
    await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
    await expect(pw).toHaveURL(new RegExp(`\\?messages=starred$`));
    await expect.poll(() => renderedIds(pw)).toEqual(STARRED_INDEXES.map(id));
  });

  test('never advances the read watermark and never appends an unstarred live message', async ({
    page: pw,
  }) => {
    const hub = await setupMocks(pw);
    await openFiltered(pw);
    await emitEventSourceUpdate(pw, `project.${PROJECT_A.id}.chat.message`, {
      ...message(119),
      id: id(500),
      msg: 'Mock unstarred live message',
      createdAt: '2026-10-21T00:00:00Z',
    });
    await pw.waitForTimeout(2_500);
    await expect(pw.getByText('Mock unstarred live message')).toHaveCount(0);
    expect(hub.readPosts).toBe(0);
    await expect(pw.locator('scion-chat-thread .unread-divider')).toHaveCount(0);
  });

  test('jump lands outside the loaded window; Back restores the filtered center; Forward returns', async ({
    page: pw,
  }) => {
    const hub = await setupMocks(pw);
    await openFiltered(pw);
    await expect(pw).toHaveURL(new RegExp(`\\?messages=starred$`));
    await row(pw, 10).scrollIntoViewIfNeeded();
    await chooseFromMenu(pw, 10, 'Jump to message in conversation');

    await expect(pw).toHaveURL(new RegExp(`${canonicalPath}#msg-${id(10)}$`), { timeout: 15_000 });
    await expect(banner(pw)).toHaveCount(0);
    await expect(row(pw, 10)).toBeInViewport({ timeout: 15_000 });
    // Full context: the unstarred neighbours are there, fetched around the target.
    await expect(row(pw, 9)).toBeAttached();
    await expect(row(pw, 11)).toBeAttached();
    expect(hub.aroundCalls).toContain(id(10));
    await expect(pw.locator('scion-chat-thread scion-chat-composer')).toHaveCount(1);

    await pw.goBack();
    await expect(pw).toHaveURL(new RegExp(`\\?messages=starred$`), { timeout: 15_000 });
    await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
    await expect.poll(() => renderedIds(pw)).toEqual(STARRED_INDEXES.map(id));
    await expect(row(pw, 10)).toBeInViewport({ timeout: 15_000 });

    await pw.goForward();
    await expect(pw).toHaveURL(new RegExp(`#msg-${id(10)}$`), { timeout: 15_000 });
    await expect(banner(pw)).toHaveCount(0);
    await expect(row(pw, 10)).toBeInViewport({ timeout: 15_000 });
    await expect(row(pw, 9)).toBeAttached();
  });

  test('Show all opens the full conversation; Back returns to the filter', async ({ page: pw }) => {
    await setupMocks(pw);
    await openFiltered(pw);
    await banner(pw).getByRole('button', { name: 'Show all messages' }).click();
    await expect(pw).toHaveURL(new RegExp(`${canonicalPath}$`), { timeout: 15_000 });
    await expect(banner(pw)).toHaveCount(0);
    await expect(row(pw, 109)).toBeAttached({ timeout: 15_000 });
    await pw.goBack();
    await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
    await expect.poll(() => renderedIds(pw)).toEqual(STARRED_INDEXES.map(id));
  });

  test('unstarring the last star leaves the empty center with Show all', async ({ page: pw }) => {
    const hub = await setupMocks(pw);
    hub.starred = new Set([110]);
    await pw.goto(filteredUrl, { waitUntil: 'domcontentloaded' });
    await expect(row(pw, 110)).toBeVisible({ timeout: 15_000 });
    await chooseFromMenu(pw, 110, 'Unstar message');
    const empty = pw.locator('scion-chat-thread .star-filter-empty');
    await expect(empty).toContainText('No starred messages in this conversation', {
      timeout: 15_000,
    });
    await expect(empty.getByRole('button', { name: 'Show all messages' })).toBeVisible();
    // The center stays on this conversation.
    await expect(pw).toHaveURL(new RegExp(`${TOPIC_ID}\\?messages=starred$`));
  });

  test('rapid Back and Forward end on the view the URL names', async ({ page: pw }) => {
    const hub = await setupMocks(pw);
    await openFiltered(pw);
    await banner(pw).getByRole('button', { name: 'Show all messages' }).click();
    await expect(pw).toHaveURL(new RegExp(`${canonicalPath}$`), { timeout: 15_000 });
    const before = hub.filteredCalls;
    await pw.goBack();
    await pw.goForward();
    await pw.goBack();
    await expect(pw).toHaveURL(new RegExp(`\\?messages=starred$`), { timeout: 15_000 });
    await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
    await expect.poll(() => renderedIds(pw)).toEqual(STARRED_INDEXES.map(id));
    expect(hub.filteredCalls).toBeGreaterThan(before);
  });
});
