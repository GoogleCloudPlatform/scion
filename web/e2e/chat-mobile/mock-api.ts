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
 * Mocked chat fixtures for the mobile-layout e2e guard suite: three spaces
 * with long names, 45 threads in one space (one with a very long name), 8
 * threads in each of the others, and about 40 messages on the general
 * thread covering long prose, an unbroken URL, code, a table, a list and a
 * blockquote — content shapes known to stress narrow-viewport layout.
 *
 * Sets only `web.native_chat` in `__SCION_FEATURES__` — the one chat feature
 * flag that still exists (`DEFAULT_ON_FLAGS` in feature-flags.ts). There is
 * no longer a separate v2/wave-1 split to account for.
 */

import type { Page } from '@playwright/test';

export const PROJECT_A = {
  id: 'proj-a-mobile-layout',
  slug: 'mobile-layout-test-project-with-a-long-name',
  name: 'Mobile Layout Test Project With A Long Name',
};
export const PROJECT_B = { id: 'proj-b-second-space', slug: 'second-space', name: 'Second Space' };
export const PROJECT_C = {
  id: 'proj-c-third-space',
  slug: 'third-space-with-an-extremely-long-unbroken-slug-name-xyz',
  name: 'Third-space-with-an-extremely-long-unbroken-slug-name-xyz',
};

export const GENERAL_THREAD_ID = 'thread-01';
export const LONG_NAME_THREAD_ID = 'thread-07';

export const HUMAN_MEMBER_ID = 'user-ada';
export const AGENT_MEMBER_ID = 'agent-coder-one';

const THREAD_COUNT_MAIN = 45;
const THREAD_COUNT_OTHER = 8;

interface MockThread {
  id: string;
  name: string;
  isGeneral: boolean;
  pinned: boolean;
  hasUnread: boolean;
  hasUnreadMention: boolean;
}

function buildThreads(prefix: string, count: number): MockThread[] {
  const threads: MockThread[] = [];
  for (let i = 1; i <= count; i++) {
    const idx = String(i).padStart(2, '0');
    const isLongName = prefix === 'thread' && i === 7;
    threads.push({
      id: `${prefix}-${idx}`,
      name: isLongName
        ? 'a-really-long-thread-name-that-does-not-wrap-nicely-on-small-screens-07'
        : i === 1
          ? 'general'
          : `${prefix}-${idx} discussion`,
      isGeneral: i === 1,
      pinned: false,
      hasUnread: false,
      hasUnreadMention: false,
    });
  }
  return threads;
}

const MAIN_THREADS = buildThreads('thread', THREAD_COUNT_MAIN);
const OTHER_B_THREADS = buildThreads('other-b', THREAD_COUNT_OTHER);
const OTHER_C_THREADS = buildThreads('other-c', THREAD_COUNT_OTHER);

/** Message bodies covering long prose, an unbroken URL, code, a table, a list and a blockquote. */
const MESSAGE_BODIES: string[] = [
  'Short hello.',
  'Lorem ipsum dolor sit amet, consectetur adipiscing elit. '.repeat(8),
  'An unbroken URL: https://example.com/' + 'a'.repeat(160) + '?q=1',
  'Unbroken token: ' + 'X'.repeat(120),
  '```go\nfunc reallyLongFunctionNameThatGoesOnAndOn(argumentNumberOne string, argumentNumberTwo int, argumentNumberThree map[string]interface{}) error { return nil }\n```',
  '| col one | col two | col three | col four | col five | col six |\n|---|---|---|---|---|---|\n| aaaaaaaaaaaaaa | bbbbbbbbbbbbbbbb | cccccccccccccc | dddddddddddd | eeeeeeeeeeee | ffffffffffff |',
  'Inline `code_with_a_very_long_identifier_name_that_will_not_wrap_at_all_in_a_narrow_viewport` here.',
  '- list item one\n- list item two with **bold** and _italics_\n  - nested item\n1. numbered',
  '> a blockquote that is reasonably long and should wrap across multiple lines on a phone',
];

const MESSAGE_COUNT = 40;

export function buildMessages(threadId: string, projectId: string): Record<string, unknown>[] {
  const messages: Record<string, unknown>[] = [];
  const base = Date.parse('2026-09-01T00:00:00Z');
  for (let i = 0; i < MESSAGE_COUNT; i++) {
    const body =
      MESSAGE_BODIES[i % MESSAGE_BODIES.length] + (i >= MESSAGE_BODIES.length ? ` (#${i})` : '');
    messages.push({
      id: `${threadId}-msg-${i}`,
      projectId,
      sender: i % 3 === 0 ? 'agent' : 'user',
      senderId: i % 3 === 0 ? AGENT_MEMBER_ID : HUMAN_MEMBER_ID,
      recipient: 'thread',
      recipientId: threadId,
      msg: body,
      type: 'chat',
      agentId: AGENT_MEMBER_ID,
      threadId,
      createdAt: new Date(base + i * 60_000).toISOString(),
    });
  }
  return messages;
}

const HUMAN_MEMBERS = [
  { id: HUMAN_MEMBER_ID, kind: 'user', displayName: 'Ada Lovelace', email: 'ada@example.test' },
  { id: 'user-grace', kind: 'user', displayName: 'Grace Hopper', email: 'grace@example.test' },
];

const AGENT_MEMBERS = [
  { id: AGENT_MEMBER_ID, kind: 'agent', displayName: 'Coder One', slug: 'coder-one' },
  { id: 'agent-review-bot', kind: 'agent', displayName: 'Review Bot', slug: 'review-bot' },
];

/**
 * Installs every `page.route` mock the mobile chat-layout suite needs, then
 * suppresses SSE (no real server behind these mocks). Call before `page.goto`.
 */
export async function setupChatMobileMocks(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.native_chat': true };
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
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => route.fulfill({ json: { agents: [] } }));
  await page.route('**/api/v1/users**', (route) => route.fulfill({ json: { users: [] } }));
  // Only used by the desktop checks that the document never scrolls on
  // /, /projects and /agents (outside the chat shell) — chat itself never
  // calls these.
  await page.route(/\/api\/v1\/projects(\?|$)/, (route) =>
    route.fulfill({ json: { projects: [] } })
  );

  await page.route(/\/api\/v1\/chat\/spaces$/, (route) =>
    route.fulfill({
      json: {
        spaces: [
          {
            projectId: PROJECT_A.id,
            projectSlug: PROJECT_A.slug,
            projectName: PROJECT_A.name,
            unreadCount: 0,
            hasUnreadMention: false,
          },
          {
            projectId: PROJECT_B.id,
            projectSlug: PROJECT_B.slug,
            projectName: PROJECT_B.name,
            unreadCount: 0,
            hasUnreadMention: false,
          },
          {
            projectId: PROJECT_C.id,
            projectSlug: PROJECT_C.slug,
            projectName: PROJECT_C.name,
            unreadCount: 0,
            hasUnreadMention: false,
          },
        ],
      },
    })
  );

  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/threads/, (route) => {
    const url = route.request().url();
    const threads =
      url.includes(encodeURIComponent(PROJECT_A.id)) || url.includes(PROJECT_A.id)
        ? MAIN_THREADS
        : url.includes(PROJECT_B.id)
          ? OTHER_B_THREADS
          : OTHER_C_THREADS;
    void route.fulfill({ json: { threads } });
  });

  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/members/, (route) =>
    route.fulfill({ json: { humans: HUMAN_MEMBERS, agents: AGENT_MEMBERS } })
  );

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
        threadGroups: '{}',
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
  await page.route(
    /\/api\/v1\/chat\/conversations\/[^/]+\/(read|unread|typing|pin|mute)/,
    (route) => route.fulfill({ json: {} })
  );

  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const url = route.request().url();
    const match = url.match(/\/conversations\/([^/?]+)\/messages/);
    const threadId = match ? decodeURIComponent(match[1]) : '';
    const messages = threadId === GENERAL_THREAD_ID ? buildMessages(threadId, PROJECT_A.id) : [];
    void route.fulfill({ json: { items: messages, messages } });
  });
}
