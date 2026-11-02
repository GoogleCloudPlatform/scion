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
 * The starred-only center's URL (web.chat_starred_messages): the page takes
 * `?messages=starred` from a conversation route, drops it from any other
 * route or while the experiment is off, pushes a separate history entry
 * when the thread leaves the filter, and opens a header entry in full
 * context from the filtered center.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

const pushRoute = vi.fn((..._args: unknown[]) => Promise.resolve());
const replaceRoute = vi.fn((..._args: unknown[]) => Promise.resolve());
const navigateTo = vi.fn();
const showToastMock = vi.fn();

vi.mock('../../utils/toast.js', () => ({
  showToast: (...args: unknown[]): unknown => showToastMock(...args) as unknown,
}));

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return {
    navigateTo: (...args: unknown[]): unknown => navigateTo(...args) as unknown,
    pushRoute: (...args: unknown[]): unknown => pushRoute(...args) as unknown,
    replaceRoute: (...args: unknown[]): unknown => replaceRoute(...args) as unknown,
    stateManager: state.stateManager,
  };
});

const apiFetch = vi.fn((url: string) => {
  const id = decodeURIComponent(url.split('/').pop() ?? '');
  return Promise.resolve(
    new Response(
      JSON.stringify({ messageId: id, conversationKey: 'topic-a', permalink: '/chat/x' }),
      { status: 200 }
    )
  );
});

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: (url: string): Promise<Response> => apiFetch(url) };
});

const M1 = '00000000-0000-4000-8000-000000000001';

interface FakeThread {
  jumpSequence: number;
  scrollToMessageById: ReturnType<typeof vi.fn>;
  openInFullContext: ReturnType<typeof vi.fn>;
}

interface ChatInternals extends HTMLElement {
  v2Conversation: { conversationKey: string; projectId: string; isDM: boolean } | null;
  centerMessageFilter: '' | 'starred';
  syncMessageFilterFromUrl(): void;
  handleMessageFilterChange(e: Event): void;
  handleStarredMessageOpen(e: CustomEvent<{ messageId: string }>): void;
  threadElement(): FakeThread | null;
}

function page(): ChatInternals {
  const p = document.createElement('scion-page-chat') as ChatInternals;
  Object.defineProperty(p, 'updateComplete', { value: Promise.resolve(true) });
  return p;
}

function at(url: string): void {
  window.history.replaceState({}, '', url);
}

function setFlag(on: boolean): void {
  (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
    'web.chat_starred_messages': on,
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  pushRoute.mockClear();
  replaceRoute.mockClear();
  navigateTo.mockClear();
  showToastMock.mockClear();
  setFlag(true);
});

afterEach(() => {
  document.body.innerHTML = '';
  delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  at('/');
});

describe('the starred-only center URL', () => {
  it('takes the filter from a conversation route and keeps it in the URL', () => {
    for (const url of [
      '/chat/space/p1/thread/topic-a?messages=starred',
      '/chat/my-project/topic-a?messages=starred',
      '/chat/dm/dm%3Aagent%3Aa%3Auser%3Ab?messages=starred',
    ]) {
      at(url);
      const p = page();
      p.syncMessageFilterFromUrl();
      expect(p.centerMessageFilter, url).toBe('starred');
      expect(window.location.search, url).toBe('?messages=starred');
    }
    expect(replaceRoute).not.toHaveBeenCalled();
  });

  it('drops the filter from a route that is not one conversation', () => {
    for (const [url, path] of [
      ['/chat/my-project?messages=starred', '/chat/my-project'],
      ['/chat/space/p1?messages=starred&x=1', '/chat/space/p1'],
      ['/chat?messages=starred', '/chat'],
    ]) {
      at(url);
      const p = page();
      p.syncMessageFilterFromUrl();
      expect(p.centerMessageFilter, url).toBe('');
      expect(window.location.pathname + window.location.search, url).toBe(
        url.includes('x=1') ? `${path}?x=1` : path
      );
      expect(replaceRoute).toHaveBeenLastCalledWith(path);
    }
    // A stray filter on a non-conversation route is dropped silently.
    expect(showToastMock).not.toHaveBeenCalled();
  });

  it('shows ordinary history and drops the filter while the experiment is off', () => {
    setFlag(false);
    at('/chat/my-project/topic-a?messages=starred#msg-' + M1);
    const p = page();
    p.syncMessageFilterFromUrl();
    expect(p.centerMessageFilter).toBe('');
    expect(window.location.search).toBe('');
    expect(window.location.hash).toBe('#msg-' + M1);
    // The change is made visible: the full history is not the starred one.
    expect(showToastMock).toHaveBeenCalledWith(
      'Starred messages are not available. Showing all messages.',
      'warning'
    );
  });

  it('a jump out of the filter is a new entry naming the message; Show all names none', () => {
    at('/chat/my-project/topic-a?messages=starred');
    const p = page();
    p.centerMessageFilter = 'starred';
    p.handleMessageFilterChange(
      new CustomEvent('chat-message-filter-change', { detail: { filter: '', messageId: M1 } })
    );
    expect(pushRoute).toHaveBeenCalledWith(`/chat/my-project/topic-a#msg-${M1}`);
    expect(p.centerMessageFilter).toBe('');

    at('/chat/my-project/topic-a?messages=starred');
    p.centerMessageFilter = 'starred';
    p.handleMessageFilterChange(
      new CustomEvent('chat-message-filter-change', { detail: { filter: '' } })
    );
    expect(pushRoute).toHaveBeenLastCalledWith('/chat/my-project/topic-a');
    expect(p.centerMessageFilter).toBe('');
  });

  it('opens a header entry of the open conversation in full context from the filtered center', async () => {
    at('/chat/my-project/topic-a?messages=starred');
    const p = page();
    p.v2Conversation = { conversationKey: 'topic-a', projectId: 'p1', isDM: false };
    p.centerMessageFilter = 'starred';
    const thread: FakeThread = {
      jumpSequence: 1,
      scrollToMessageById: vi.fn(),
      openInFullContext: vi.fn(),
    };
    p.threadElement = (): FakeThread => thread;
    p.handleStarredMessageOpen(
      new CustomEvent('starred-message-open', { detail: { messageId: M1 } })
    );
    await settle();
    expect(thread.openInFullContext).toHaveBeenCalledWith(M1);
    expect(thread.scrollToMessageById).not.toHaveBeenCalled();
  });
});
