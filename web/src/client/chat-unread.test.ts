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
 * Tests for the unread conversation count shown on the chat mode badge and
 * in the tab title.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import {
  CHAT_UNREAD_COUNT_EVENT,
  CHAT_UNREAD_COUNT_URL,
  ChatUnreadCounter,
  INITIAL_REFRESH_MAX_DELAY_MS,
  startChatUnreadIfEligible,
  UNREAD_REFRESH_DEBOUNCE_MS,
  unreadConversations,
} from './chat-unread.js';
import { setDocumentTitle, setUnreadBadge, getUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

/** Answers the count endpoint with `conversations`. */
function serveCount(conversations: number): void {
  apiFetch.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify({ conversations, threads: conversations, dms: 0 }), {
        status: 200,
      })
    )
  );
}

/** Counters started during a test, stopped afterwards. */
let counters: ChatUnreadCounter[] = [];

function counter(): ChatUnreadCounter {
  const c = new ChatUnreadCounter();
  counters.push(c);
  return c;
}

/** Lets fetch and json promise chains settle under fake timers. */
async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await vi.advanceTimersByTimeAsync(0);
}

beforeEach(() => {
  apiFetch.mockReset();
  setUnreadBadge(0);
  counters = [];
  vi.useFakeTimers();
  // Use the timer fallback, so the first refresh is driven by fake timers.
  vi.stubGlobal('requestIdleCallback', undefined);
  vi.stubGlobal('cancelIdleCallback', undefined);
});

afterEach(() => {
  for (const c of counters) c.stop();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  setUnreadBadge(0);
});

describe('unreadConversations', () => {
  it('reads the conversations field', () => {
    expect(unreadConversations({ conversations: 4, threads: 3, dms: 1 })).toBe(4);
    expect(unreadConversations({ conversations: 0 })).toBe(0);
  });

  it('rejects a body without a usable count', () => {
    expect(unreadConversations({})).toBeNull();
    expect(unreadConversations(null)).toBeNull();
    expect(unreadConversations({ conversations: 'x' })).toBeNull();
    expect(unreadConversations({ conversations: Number.NaN })).toBeNull();
  });

  it('clamps nonsense', () => {
    expect(unreadConversations({ conversations: -2 })).toBe(0);
    expect(unreadConversations({ conversations: 2.7 })).toBe(2);
  });
});

describe('tab title badge', () => {
  it('prefixes the title and survives a route change', () => {
    setUnreadBadge(3);
    setDocumentTitle('Chat');
    expect(document.title).toBe('(3) Chat — Scion');
    setDocumentTitle('Agents');
    expect(document.title).toBe('(3) Agents — Scion');
  });

  it('disappears at zero', () => {
    setUnreadBadge(2);
    setDocumentTitle('Chat');
    setUnreadBadge(0);
    expect(document.title).toBe('Chat — Scion');
    expect(getUnreadBadge()).toBe(0);
  });
});

describe('ChatUnreadCounter', () => {
  it('fetches the count endpoint at start when immediate, and publishes it', async () => {
    serveCount(5);
    const events: number[] = [];
    const onCount = (e: Event): void => {
      events.push((e as CustomEvent<{ count: number }>).detail.count);
    };
    window.addEventListener(CHAT_UNREAD_COUNT_EVENT, onCount);
    try {
      const c = counter();
      c.start({ immediate: true });
      await settle();
      expect(apiFetch).toHaveBeenCalledWith(CHAT_UNREAD_COUNT_URL);
      expect(c.count).toBe(5);
      expect(getUnreadBadge()).toBe(5);
      expect(events).toEqual([5]);
    } finally {
      window.removeEventListener(CHAT_UNREAD_COUNT_EVENT, onCount);
    }
  });

  it('defers the first refresh when not immediate', async () => {
    serveCount(2);
    const c = counter();
    c.start();
    await settle();
    expect(apiFetch).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(2);
  });

  it('uses the idle callback where the browser has one', async () => {
    serveCount(1);
    const idle = vi.fn((cb: () => void) => {
      cb();
      return 1;
    });
    vi.stubGlobal('requestIdleCallback', idle);
    vi.stubGlobal('cancelIdleCallback', vi.fn());
    const c = counter();
    c.start();
    await settle();
    expect(idle).toHaveBeenCalledWith(expect.any(Function), {
      timeout: INITIAL_REFRESH_MAX_DELAY_MS,
    });
    expect(c.count).toBe(1);
  });

  it('refreshes, debounced, on chat message and read-state events', async () => {
    serveCount(0);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    apiFetch.mockClear();

    serveCount(3);
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    stateManager.dispatchEvent(new CustomEvent('chat-read-state-updated', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS - 1);
    expect(apiFetch).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(3);
  });

  it('ignores agent notifications', async () => {
    serveCount(0);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    apiFetch.mockClear();
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('an event before the deferred first refresh replaces it', async () => {
    serveCount(4);
    const c = counter();
    c.start();
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(4);
  });

  it('refreshes when the chat page reports a local change', async () => {
    serveCount(0);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    serveCount(1);
    c.scheduleRefresh();
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(c.count).toBe(1);
  });

  it('keeps the last known count when the server is unreachable or errors', async () => {
    serveCount(2);
    const c = counter();
    c.start({ immediate: true });
    await settle();

    apiFetch.mockRejectedValue(new Error('offline'));
    await c.refresh();
    expect(c.count).toBe(2);

    apiFetch.mockResolvedValue(new Response('{}', { status: 500 }));
    await c.refresh();
    expect(c.count).toBe(2);
    expect(getUnreadBadge()).toBe(2);
  });

  it('discards a stale response that lands after a newer one', async () => {
    const resolvers: Array<(n: number) => void> = [];
    apiFetch.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolvers.push((n) =>
            resolve(new Response(JSON.stringify({ conversations: n }), { status: 200 }))
          );
        })
    );
    const c = counter();
    c.start({ immediate: true });
    const second = c.refresh();
    resolvers[1](7);
    await second;
    resolvers[0](1);
    await settle();
    expect(c.count).toBe(7);
  });

  it('stops listening and clears the count after stop()', async () => {
    serveCount(3);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    c.stop();
    expect(c.count).toBe(0);
    expect(getUnreadBadge()).toBe(0);
    apiFetch.mockClear();
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('sends nothing when stopped before the deferred first refresh', async () => {
    serveCount(3);
    const c = counter();
    c.start();
    c.stop();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

describe('startChatUnreadIfEligible', () => {
  it('starts only for a signed-in user with chat enabled, at once on a chat route', () => {
    const start = vi.fn();
    expect(startChatUnreadIfEligible({ start }, false, true, true)).toBe(false);
    expect(startChatUnreadIfEligible({ start }, true, false, true)).toBe(false);
    expect(start).not.toHaveBeenCalled();
    expect(startChatUnreadIfEligible({ start }, true, true, true)).toBe(true);
    expect(start).toHaveBeenLastCalledWith({ immediate: true });
    expect(startChatUnreadIfEligible({ start }, true, true, false)).toBe(true);
    expect(start).toHaveBeenLastCalledWith({ immediate: false });
  });
});
