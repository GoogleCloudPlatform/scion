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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import {
  CHAT_UNREAD_COUNT_EVENT,
  CHAT_UNREAD_COUNT_URL,
  ChatUnreadCounter,
  INITIAL_REFRESH_MAX_DELAY_MS,
  startChatUnreadIfEligible,
  UNREAD_ASK_TIMEOUT_MS,
  UNREAD_LOCK_NAME,
  UNREAD_REFRESH_DEBOUNCE_MS,
  UNREAD_SHARE_KEY,
  unreadConversations,
} from './chat-unread.js';
import { chatLoadClock } from './chat-list-cache.js';
import { setDocumentTitle, setUnreadBadge, getUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

// The load clock follows Date, which the fake timers drive, so perf-clock
// instants and wall-clock times stay consistent as the tests advance time.
// It starts level with performance.now(), the clock Event.timeStamp uses.
const clockBase = vi.hoisted(() => Date.now() - performance.now());
vi.mock('./chat-list-cache.js', async (orig) => ({
  ...(await orig<typeof import('./chat-list-cache.js')>()),
  chatLoadClock: () => Date.now() - clockBase,
}));

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
      expect(apiFetch).toHaveBeenCalledWith(CHAT_UNREAD_COUNT_URL, {
        signal: expect.anything(),
      });
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

  it('refreshes, debounced, on a burst of chat message events', async () => {
    serveCount(0);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    apiFetch.mockClear();

    serveCount(3);
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS - 1);
    expect(apiFetch).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(3);
  });

  it('refreshes on a read-state event alone', async () => {
    serveCount(2);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    apiFetch.mockClear();

    serveCount(1);
    stateManager.dispatchEvent(new CustomEvent('chat-read-state-updated', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(1);
  });

  it('does nothing on scheduleRefresh when not started', async () => {
    serveCount(5);
    const c = counter();
    c.scheduleRefresh();
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(c.count).toBe(0);
  });

  it('does nothing on scheduleRefresh after stop()', async () => {
    serveCount(0);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    c.stop();
    apiFetch.mockClear();
    c.scheduleRefresh();
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
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

  it('does not retry or announce a change after a server error', async () => {
    serveCount(2);
    const c = counter();
    c.start({ immediate: true });
    await settle();

    const changes = vi.fn();
    window.addEventListener(CHAT_UNREAD_COUNT_EVENT, changes);
    apiFetch.mockClear();
    apiFetch.mockImplementation(() => Promise.resolve(new Response('{}', { status: 500 })));
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);

    // Nothing asks again until the next event.
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 10);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(changes).not.toHaveBeenCalled();
    expect(c.count).toBe(2);
    window.removeEventListener(CHAT_UNREAD_COUNT_EVENT, changes);
  });

  it('runs a refresh asked during a load once, after it, and keeps the newer answer', async () => {
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
    const third = c.refresh();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    resolvers[0](1);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(2);
    resolvers[1](7);
    await Promise.all([second, third]);
    expect(c.count).toBe(7);
    expect(apiFetch).toHaveBeenCalledTimes(2);
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
    expect(startChatUnreadIfEligible({ start }, true, true, false, 'u1')).toBe(true);
    expect(start).toHaveBeenLastCalledWith({ immediate: false, userId: 'u1' });
  });
});

describe('ChatUnreadCounter and the hub subjects', () => {
  /** Feeds one SSE update through the page-wide state manager. */
  function emit(subject: string, data: unknown): void {
    (
      stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
    ).handleUpdate({ subject, data });
  }

  async function started(count: number): Promise<ChatUnreadCounter> {
    serveCount(count);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    apiFetch.mockClear();
    return c;
  }

  it('asks once for a thread message on both the project and user subject', async () => {
    const c = await started(0);
    serveCount(1);
    const msg = { id: 'm1', threadId: 'topic-1', senderId: 'u2', msg: 'hi' };
    emit('project.p1.chat.message', msg);
    emit('user.u1.chat.message', msg);
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(1);
  });

  it('refreshes on the user own read or mute change', async () => {
    const c = await started(3);
    serveCount(2);
    // The hub's own-state event: no name or preview, unread false.
    emit('user.u1.chat.read-state', {
      conversationKey: 'topic-1',
      userId: 'u1',
      messageId: 'm9',
      readAt: '2026-10-10T00:00:00.000Z',
      unread: false,
    });
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(2);

    serveCount(1);
    emit('user.u1.chat.read-state', {
      conversationKey: 'dm:user:u1:user:u2',
      userId: 'u1',
      messageId: '',
      readAt: '2026-10-10T00:00:01.000Z',
      muted: true,
    });
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(2);
    expect(c.count).toBe(1);
  });
});

describe('ChatUnreadCounter cost control', () => {
  it('reads an installed source instead of the endpoint, with the event time', async () => {
    const source = vi.fn((_after: number) => Promise.resolve({ count: 4, startedAt: 0 }));
    const c = counter();
    c.setSource(source);
    c.start();
    // An event delivered at eventAt: a request started after it may answer.
    const eventAt = chatLoadClock() - 250;
    c.scheduleRefresh(eventAt);
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).not.toHaveBeenCalled();
    expect(source).toHaveBeenCalledTimes(1);
    expect(source.mock.calls[0][0]).toBe(eventAt);
    expect(c.count).toBe(4);

    c.setSource(null);
    serveCount(5);
    await c.refresh();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(c.count).toBe(5);
  });

  it('keeps the last count when the source cannot answer', async () => {
    serveCount(2);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    c.setSource(() => Promise.resolve(null));
    await c.refresh();
    expect(c.count).toBe(2);
    c.setSource(() => Promise.reject(new Error('boom')));
    await c.refresh();
    expect(c.count).toBe(2);
  });

  describe('across tabs', () => {
    /** A one-holder-at-a-time stand-in for navigator.locks that reports whether it is held. */
    function fakeLocks(): { request: ReturnType<typeof vi.fn>; held: () => boolean } {
      let holders = 0;
      let tail: Promise<unknown> = Promise.resolve();
      const request = vi.fn(
        (_name: string, _opts: unknown, cb: () => Promise<unknown>): Promise<unknown> => {
          const run = tail.then(async () => {
            holders++;
            try {
              return await cb();
            } finally {
              holders--;
            }
          });
          tail = run.catch(() => undefined);
          return run;
        }
      );
      return { request, held: () => holders > 0 };
    }

    function share(answer: { userId: string; count: number; askedAt: number }): void {
      localStorage.setItem(UNREAD_SHARE_KEY, JSON.stringify(answer));
    }

    beforeEach(() => localStorage.removeItem(UNREAD_SHARE_KEY));

    it('adopts an answer another tab asked for after the event', async () => {
      const locks = fakeLocks();
      vi.stubGlobal('navigator', { ...navigator, locks });
      const c = counter();
      c.start({ userId: 'u1' });
      stateManager.dispatchEvent(new Event('chat-message-received'));
      // Another tab's request for the same event started after it.
      share({ userId: 'u1', count: 9, askedAt: Date.now() + 1 });
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
      await settle();
      expect(locks.request).toHaveBeenCalledWith(
        UNREAD_LOCK_NAME,
        expect.anything(),
        expect.any(Function)
      );
      expect(apiFetch).not.toHaveBeenCalled();
      expect(c.count).toBe(9);
    });

    it('asks, and shares the answer, when the shared one predates the event', async () => {
      vi.stubGlobal('navigator', { ...navigator, locks: fakeLocks() });
      share({ userId: 'u1', count: 9, askedAt: Date.now() - 60_000 });
      serveCount(3);
      const c = counter();
      c.start({ userId: 'u1' });
      stateManager.dispatchEvent(new Event('chat-message-received'));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
      await settle();
      expect(apiFetch).toHaveBeenCalledTimes(1);
      expect(c.count).toBe(3);
      const shared = JSON.parse(localStorage.getItem(UNREAD_SHARE_KEY) ?? '{}');
      expect(shared).toMatchObject({ userId: 'u1', count: 3 });
    });

    it('two tabs reacting to one event send one request', async () => {
      vi.stubGlobal('navigator', { ...navigator, locks: fakeLocks() });
      serveCount(6);
      const a = counter();
      const b = counter();
      a.start({ userId: 'u1' });
      b.start({ userId: 'u1' });
      stateManager.dispatchEvent(new Event('chat-message-received'));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
      await settle();
      expect(apiFetch).toHaveBeenCalledTimes(1);
      expect(a.count).toBe(6);
      expect(b.count).toBe(6);
    });

    it('asks on its own when the lock fails', async () => {
      vi.stubGlobal('navigator', {
        ...navigator,
        locks: { request: vi.fn(() => Promise.reject(new Error('timeout'))) },
      });
      serveCount(1);
      const c = counter();
      c.start({ immediate: true, userId: 'u1' });
      await settle();
      expect(apiFetch).toHaveBeenCalledTimes(1);
      expect(c.count).toBe(1);
    });

    it("never adopts another user's answer, and shares nothing without a user", async () => {
      const locks = fakeLocks();
      vi.stubGlobal('navigator', { ...navigator, locks });
      share({ userId: 'someone-else', count: 9, askedAt: Date.now() + 60_000 });
      serveCount(2);
      const c = counter();
      c.start({ immediate: true, userId: 'u1' });
      await settle();
      expect(apiFetch).toHaveBeenCalledTimes(1);
      expect(c.count).toBe(2);
      expect(JSON.parse(localStorage.getItem(UNREAD_SHARE_KEY) ?? '{}')).toMatchObject({
        userId: 'u1',
        count: 2,
      });

      localStorage.removeItem(UNREAD_SHARE_KEY);
      locks.request.mockClear();
      const anon = counter();
      anon.start({ immediate: true });
      await settle();
      expect(locks.request).not.toHaveBeenCalled();
      expect(localStorage.getItem(UNREAD_SHARE_KEY)).toBeNull();
    });

    it('dates a shared answer by the oldest request behind it, so a later event is still asked', async () => {
      const locks = fakeLocks();
      // Another tab holds the lock while this tab's refresh waits.
      let release!: () => void;
      void locks.request(UNREAD_LOCK_NAME, {}, () => new Promise<void>((r) => (release = r)));
      vi.stubGlobal('navigator', { ...navigator, locks });

      const t0 = chatLoadClock();
      // The first answer joined a load that started after the first event
      // but before the second one.
      const source = vi
        .fn()
        .mockResolvedValueOnce({ count: 1, startedAt: t0 + 100 })
        .mockImplementationOnce(() => Promise.resolve({ count: 2, startedAt: chatLoadClock() }));
      const c = counter();
      c.setSource(source);
      c.start({ userId: 'u1' });

      c.scheduleRefresh(t0); // first event
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS); // now waits for the lock
      c.scheduleRefresh(); // second event, at t0 + 500, while waiting
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
      release();
      await settle();

      // Dated t0+100, the first answer predates the second event, so the
      // trailing refresh for that event asks again rather than adopting it.
      expect(source).toHaveBeenCalledTimes(2);
      expect(c.count).toBe(2);
    });

    it('abandons a hung source and releases the lock', async () => {
      const locks = fakeLocks();
      vi.stubGlobal('navigator', { ...navigator, locks });
      const source = vi
        .fn()
        .mockReturnValueOnce(new Promise(() => {}))
        .mockImplementationOnce(() => Promise.resolve({ count: 5, startedAt: chatLoadClock() }));
      const c = counter();
      c.setSource(source);
      c.start({ immediate: true, userId: 'u1' });
      await vi.advanceTimersByTimeAsync(UNREAD_ASK_TIMEOUT_MS);
      await settle();
      expect(locks.held()).toBe(false);

      c.scheduleRefresh();
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
      await settle();
      expect(source).toHaveBeenCalledTimes(2);
      expect(c.count).toBe(5);
    });
  });

  it('drops a refresh still in flight across stop() and a new start()', async () => {
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
    c.start({ immediate: true, userId: 'u1' });
    c.stop();
    c.start({ userId: 'u2' });
    resolvers[0](8);
    await settle();
    expect(c.count).toBe(0);
  });

  it('abandons a hung count request, keeps the count, and asks again later', async () => {
    serveCount(3);
    const c = counter();
    c.start({ immediate: true });
    await settle();
    expect(c.count).toBe(3);

    let signal: AbortSignal | undefined;
    apiFetch.mockImplementation((_url: string, init?: { signal?: AbortSignal }) => {
      signal = init?.signal;
      return new Promise<Response>(() => {});
    });
    const hung = c.refresh();
    await vi.advanceTimersByTimeAsync(UNREAD_ASK_TIMEOUT_MS);
    await hung;
    expect(signal?.aborted).toBe(true);
    expect(c.count).toBe(3);

    serveCount(4);
    c.scheduleRefresh();
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS);
    await settle();
    expect(apiFetch).toHaveBeenCalledTimes(3);
    expect(c.count).toBe(4);
  });
});
