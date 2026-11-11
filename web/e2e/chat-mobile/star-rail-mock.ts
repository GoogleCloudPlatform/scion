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
 * A mocked hub for the Starred rail (web.chat_starred_messages): it serves
 * GET /api/v1/chat/starred-conversations from its own star state (sorted
 * and paged as the hub does), the starred-only history and the star routes,
 * with mock data only. Used by star-rail.pw.ts.
 */

import { expect, type Locator, type Page } from '@playwright/test';
import { setupChatMobileMocks, PROJECT_A, PROJECT_B } from './mock-api.js';

export const STAR_RAIL_USER = 'fixture-user';

export interface MockConversation {
  key: string;
  kind: 'thread' | 'dm';
  name: string;
  projectId?: string;
  projectName?: string;
  activity: string;
  /** Message numbers; their IDs are msgId(key, n). */
  messages: number[];
  starred: Set<number>;
}

export interface StarRailHub {
  conversations: MockConversation[];
  sortMode: 'activity' | 'alpha';
  starredCalls: URLSearchParams[];
}

function hex(s: string): string {
  let h = 0;
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return h.toString(16).padStart(8, '0');
}

/** A UUID-shaped message ID per conversation and number. */
export function msgId(key: string, n: number): string {
  return `${hex(key)}-0000-4000-8000-${n.toString(16).padStart(12, '0')}`;
}

export function mockThread(
  key: string,
  name: string,
  project: { id: string; name: string },
  activity: string,
  starred: number[]
): MockConversation {
  return {
    key,
    kind: 'thread',
    name,
    projectId: project.id,
    projectName: project.name,
    activity,
    messages: [1, 2, 3, 4],
    starred: new Set(starred),
  };
}

export function mockDM(
  key: string,
  name: string,
  activity: string,
  starred: number[],
  project?: { id: string; name: string }
): MockConversation {
  return {
    key,
    kind: 'dm',
    name,
    ...(project ? { projectId: project.id, projectName: project.name } : {}),
    activity,
    messages: [1, 2, 3, 4],
    starred: new Set(starred),
  };
}

export const HUMAN_DM = `dm:user:${STAR_RAIL_USER}:user:user-mock-person`;
export const AGENT_DM = `dm:agent:agent-mock-helper:user:${STAR_RAIL_USER}`;

/** The default mock collection: two project A threads, one project B thread, two DMs. */
export function defaultConversations(): MockConversation[] {
  return [
    mockThread('topic-mock-planning', 'Mock planning', PROJECT_A, '2026-10-22T10:00:00Z', [2, 3]),
    mockThread('topic-mock-review', 'mock review', PROJECT_A, '2026-10-22T07:00:00Z', [1]),
    mockThread('topic-mock-rollout', 'Mock rollout', PROJECT_B, '2026-10-22T09:00:00Z', [4]),
    mockThread('topic-mock-unstarred', 'Mock no stars', PROJECT_B, '2026-10-22T11:00:00Z', []),
    mockDM(HUMAN_DM, 'Mock Person', '2026-10-22T08:00:00Z', [2]),
    mockDM(AGENT_DM, 'Mock helper agent', '2026-10-22T06:00:00Z', [3], PROJECT_A),
  ];
}

function messageBody(conv: MockConversation, n: number): Record<string, unknown> {
  return {
    id: msgId(conv.key, n),
    projectId: conv.projectId ?? PROJECT_A.id,
    sender: 'user:mock.person@example.test',
    senderId: 'user-mock-person',
    recipient: 'chat',
    msg: conv.starred.has(n)
      ? `Mock starred note ${n} in ${conv.name}: the rollout starts after the migration.`
      : `Mock ordinary note ${n} in ${conv.name}`,
    type: 'chat',
    channel: 'web',
    threadId: conv.key,
    createdAt: new Date(Date.parse('2026-10-20T08:00:00Z') + n * 60_000).toISOString(),
  };
}

function railRow(conv: MockConversation): Record<string, unknown> {
  return {
    conversationKey: conv.key,
    conversationKind: conv.kind,
    conversationName: conv.name,
    lastActivityAt: conv.activity,
    ...(conv.projectId ? { projectId: conv.projectId, projectName: conv.projectName } : {}),
  };
}

/** Install the base chat mocks plus a starred hub over `conversations`. */
export async function setupStarRail(
  pw: Page,
  conversations: MockConversation[] = defaultConversations(),
  sortMode: 'activity' | 'alpha' = 'activity'
): Promise<StarRailHub> {
  const hub: StarRailHub = { conversations, sortMode, starredCalls: [] };
  await setupChatMobileMocks(pw);
  await pw.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      ...(window.__SCION_FEATURES__ ?? {}),
      'web.chat_starred_messages': true,
    };
  });
  await pw.route('**/api/v1/chat/user-prefs', (route) => {
    if (route.request().method() === 'PUT') {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      json: {
        spaceSortMode: hub.sortMode,
        threadSortMode: hub.sortMode,
        spaceOrder: '[]',
        threadOrder: '{}',
        threadGroups: '{}',
      },
    });
  });
  const byKey = (key: string): MockConversation | undefined =>
    hub.conversations.find((c) => c.key === key);
  await pw.route(/\/api\/v1\/chat\/topics\/([^/?]+)/, (route) => {
    const key = decodeURIComponent(new URL(route.request().url()).pathname.split('/').pop() ?? '');
    const conv = byKey(key);
    void route.fulfill({
      json: { id: key, projectId: conv?.projectId ?? PROJECT_A.id, name: conv?.name ?? key },
    });
  });
  await pw.route(/\/api\/v1\/chat\/starred-conversations/, (route) => {
    const q = new URL(route.request().url()).searchParams;
    hub.starredCalls.push(q);
    const sort = q.get('sort') ?? 'activity';
    const limit = Number(q.get('limit') ?? '30');
    const offset = Number((q.get('cursor') ?? 'o-0').slice(2));
    const rows = hub.conversations
      .filter((c) => c.starred.size > 0)
      .sort((a, b) =>
        sort === 'alpha'
          ? a.name.toLowerCase().localeCompare(b.name.toLowerCase()) || a.key.localeCompare(b.key)
          : b.activity.localeCompare(a.activity) || a.key.localeCompare(b.key)
      );
    const slice = rows.slice(offset, offset + limit);
    void route.fulfill({
      json: {
        items: slice.map(railRow),
        ...(offset + limit < rows.length ? { nextCursor: `o-${offset + limit}` } : {}),
      },
    });
  });
  await pw.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const url = new URL(route.request().url());
    const key = decodeURIComponent(url.pathname.split('/')[5] ?? '');
    const conv = byKey(key);
    if (!conv) {
      void route.fulfill({ json: { messages: [] } });
      return;
    }
    const nums =
      url.searchParams.get('starred') === 'true'
        ? conv.messages.filter((n) => conv.starred.has(n))
        : conv.messages;
    void route.fulfill({
      json: {
        messages: [...nums].reverse().map((n) => messageBody(conv, n)),
        totalCount: nums.length,
      },
    });
  });
  await pw.route(/\/api\/v1\/chat\/stars(\/|\?|$)/, (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    const find = (id: string): { conv: MockConversation; n: number } | undefined => {
      for (const conv of hub.conversations) {
        const n = conv.messages.find((m) => msgId(conv.key, m) === id);
        if (n !== undefined) return { conv, n };
      }
      return undefined;
    };
    if (path === '/api/v1/chat/stars/state') {
      const ids = url.searchParams.getAll('messageId');
      void route.fulfill({
        json: {
          starredMessageIds: ids.filter((id) => {
            const f = find(id);
            return !!f && f.conv.starred.has(f.n);
          }),
        },
      });
      return;
    }
    if (path === '/api/v1/chat/stars') {
      void route.fulfill({ json: { items: [] } });
      return;
    }
    const target = find(decodeURIComponent(path.split('/').pop() ?? ''));
    if (!target) {
      void route.fulfill({
        status: 404,
        json: { error: { code: 'not_found', message: 'Message not found' } },
      });
      return;
    }
    if (method === 'DELETE') {
      target.conv.starred.delete(target.n);
      void route.fulfill({ status: 204, body: '' });
      return;
    }
    if (method === 'PUT') {
      target.conv.starred.add(target.n);
      void route.fulfill({ json: { starred: true, starredAt: '2026-10-22T12:00:00Z' } });
      return;
    }
    if (!target.conv.starred.has(target.n)) {
      void route.fulfill({
        status: 404,
        json: { error: { code: 'not_found', message: 'Message not found' } },
      });
      return;
    }
    const conv = target.conv;
    const permalink =
      conv.kind === 'dm'
        ? `/chat/dm/${encodeURIComponent(conv.key)}#msg-${msgId(conv.key, target.n)}`
        : `/chat/space/${conv.projectId}/thread/${conv.key}#msg-${msgId(conv.key, target.n)}`;
    void route.fulfill({
      json: {
        messageId: msgId(conv.key, target.n),
        conversationKey: conv.key,
        conversationKind: conv.kind,
        ...(conv.projectId ? { projectId: conv.projectId, projectName: conv.projectName } : {}),
        conversationName: conv.name,
        senderName: 'Mock Person',
        messageCreatedAt: messageBody(conv, target.n).createdAt,
        starredAt: '2026-10-22T10:00:00Z',
        excerpt: String(messageBody(conv, target.n).msg),
        edited: false,
        permalink,
      },
    });
  });
  return hub;
}

export const rail = (pw: Page): Locator => pw.locator('scion-chat-space-rail');
export const filterButton = (pw: Page, label: string): Locator =>
  rail(pw).locator('.filter-toggle button', { hasText: label });
export const starredRow = (pw: Page, key: string): Locator =>
  rail(pw).locator(`[data-starred-key="${key}"]`);
export const banner = (pw: Page): Locator => pw.locator('scion-chat-thread .star-filter-banner');

export function isPhone(pw: Page): boolean {
  return (pw.viewportSize()?.width ?? Number.MAX_SAFE_INTEGER) <= 768;
}

/** Row names in display order. */
export function starredRowNames(pw: Page): Promise<string[]> {
  return rail(pw)
    .locator('[data-starred-key] .thread-name')
    .evaluateAll((els) => els.map((el) => (el.textContent ?? '').trim()));
}

/** Section headings of the rail body, in order. */
export function railHeadings(pw: Page): Promise<string[]> {
  return rail(pw)
    .locator('.rail-body .space-name')
    .evaluateAll((els) => els.map((el) => (el.textContent ?? '').trim()));
}

/** Open /chat and switch the rail to Starred. */
export async function openStarredRail(pw: Page): Promise<void> {
  await pw.goto('/chat', { waitUntil: 'domcontentloaded' });
  await expect(filterButton(pw, 'Starred')).toBeVisible({ timeout: 15_000 });
  await filterButton(pw, 'Starred').click();
  await expect(filterButton(pw, 'Starred')).toHaveAttribute('aria-pressed', 'true');
}

function menuItem(pw: Page, label: string): Locator {
  return isPhone(pw)
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
export async function chooseFromMenu(pw: Page, id: string, label: string): Promise<void> {
  await scrollIdle(pw);
  const item = menuItem(pw, label);
  await expect(async () => {
    if (await item.isVisible()) return;
    await pw.keyboard.press('Escape');
    await pw.locator(`#msg-${id}`).click({ button: 'right', timeout: 2_000 });
    await expect(item).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 15_000 });
  await item.click();
}
