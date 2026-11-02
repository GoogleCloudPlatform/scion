/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The page side of the Starred rail (web.chat_starred_messages): a Starred
 * row opens the conversation's starred-only center (`?messages=starred`),
 * and a filter picked in the rail applies to, or clears, the open
 * conversation's center in a new history entry.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

const pushRoute = vi.fn((..._args: unknown[]) => Promise.resolve());
const replaceRoute = vi.fn((..._args: unknown[]) => Promise.resolve());

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return {
    navigateTo: vi.fn(),
    pushRoute: (...args: unknown[]): unknown => pushRoute(...args) as unknown,
    replaceRoute: (...args: unknown[]): unknown => replaceRoute(...args) as unknown,
    stateManager: state.stateManager,
  };
});

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: (): Promise<Response> => Promise.resolve(new Response('{}', { status: 200 })),
  };
});

const ME = '00000000-0000-4000-8000-0000000000aa';
const PEER = '00000000-0000-4000-8000-0000000000bb';

interface ChatInternals extends HTMLElement {
  v2Conversation: { conversationKey: string; projectId: string; isDM: boolean } | null;
  centerMessageFilter: '' | 'starred';
  mobilePanel: string;
  handleThreadSelect(e: CustomEvent): void;
  handleRailDMSelect(e: CustomEvent): void;
  handleRailFilterChange(e: CustomEvent): void;
}

function page(): ChatInternals {
  return document.createElement('scion-page-chat') as ChatInternals;
}

function at(url: string): void {
  window.history.replaceState({}, '', url);
}

function setFlag(on: boolean): void {
  (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
    'web.chat_starred_messages': on,
  };
}

function pushed(): string[] {
  return pushRoute.mock.calls.map((c) => String(c[0]));
}

function threadSelect(messageFilter?: 'starred'): CustomEvent {
  return new CustomEvent('thread-select', {
    detail: {
      conversationKey: 'topic-a',
      projectId: 'p1',
      projectSlug: 'proj',
      threadName: 'Alpha',
      defaultAgent: '',
      ...(messageFilter ? { messageFilter } : {}),
    },
  });
}

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  pushRoute.mockClear();
  replaceRoute.mockClear();
  setFlag(true);
  at('/chat');
});

afterEach(() => {
  document.body.innerHTML = '';
  delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  at('/');
});

describe('a Starred rail row', () => {
  it('opens the thread in the starred-only center; any other row opens full context', () => {
    const p = page();
    p.handleThreadSelect(threadSelect('starred'));
    expect(pushed()).toEqual(['/chat/proj/topic-a?messages=starred']);
    expect(p.centerMessageFilter).toBe('starred');
    expect(p.mobilePanel).toBe('center');

    at('/chat');
    pushRoute.mockClear();
    p.handleThreadSelect(threadSelect());
    expect(pushed()).toEqual(['/chat/proj/topic-a']);
    expect(p.centerMessageFilter).toBe('');
  });

  it('opens full context while the experiment is off', () => {
    setFlag(false);
    const p = page();
    p.handleThreadSelect(threadSelect('starred'));
    expect(pushed()).toEqual(['/chat/proj/topic-a']);
    expect(p.centerMessageFilter).toBe('');
  });

  it('opens a DM by its key in the starred-only center (the mobile drawer closes)', () => {
    const p = page();
    const key = `dm:user:${ME}:user:${PEER}`;
    p.handleRailDMSelect(
      new CustomEvent('dm-select', {
        detail: {
          conversationKey: key,
          peerId: PEER,
          peerKind: 'user',
          displayName: 'Mia',
          messageFilter: 'starred',
        },
      })
    );
    expect(pushed()).toEqual([`/chat/dm/${encodeURIComponent(key)}?messages=starred`]);
    expect(p.v2Conversation).toMatchObject({ conversationKey: key, isDM: true });
    expect(p.centerMessageFilter).toBe('starred');
    expect(p.mobilePanel).toBe('center');
  });
});

describe('a filter picked in the rail', () => {
  function openThread(p: ChatInternals, url: string, filter: '' | 'starred'): void {
    at(url);
    p.v2Conversation = { conversationKey: 'topic-a', projectId: 'p1', isDM: false };
    p.centerMessageFilter = filter;
  }

  it('Starred shows the open conversation as its starred-only center', () => {
    const p = page();
    openThread(p, '/chat/proj/topic-a', '');
    p.handleRailFilterChange(
      new CustomEvent('rail-filter-change', { detail: { filter: 'starred' } })
    );
    expect(pushed()).toEqual(['/chat/proj/topic-a?messages=starred']);
    expect(p.centerMessageFilter).toBe('starred');
  });

  it('All or Unread returns a starred-only center to full context', () => {
    for (const filter of ['all', 'unread']) {
      pushRoute.mockClear();
      const p = page();
      openThread(p, '/chat/proj/topic-a?messages=starred', 'starred');
      p.handleRailFilterChange(new CustomEvent('rail-filter-change', { detail: { filter } }));
      expect(pushed()).toEqual(['/chat/proj/topic-a']);
      expect(p.centerMessageFilter).toBe('');
    }
  });

  it('changes nothing without an open conversation, or when the center already matches', () => {
    const p = page();
    p.handleRailFilterChange(
      new CustomEvent('rail-filter-change', { detail: { filter: 'starred' } })
    );
    openThread(p, '/chat/proj/topic-a', '');
    p.handleRailFilterChange(
      new CustomEvent('rail-filter-change', { detail: { filter: 'unread' } })
    );
    expect(pushed()).toEqual([]);
  });
});
