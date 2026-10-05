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
 * Tests for the unread tab-title badge.
 *
 * Two things are easy to get wrong and invisible when they are: the badge
 * surviving the title rewrites that happen on every navigation, and muted
 * conversations staying out of the count.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import { MENTION_STATUS } from './chat-notifications.js';
import {
  ChatUnreadCounter,
  countUnreadDMs,
  countUnreadSpaces,
  INITIAL_REFRESH_MAX_DELAY_MS,
  startChatUnreadIfEligible,
  UNREAD_REFRESH_DEBOUNCE_MS,
  type UnreadDM,
  type UnreadSpace,
} from './chat-unread.js';
import { CHAT_STARTUP_REUSE_MS, chatDMsLoad, chatSpacesLoad } from './chat-list-cache.js';
import { setDocumentTitle, setUnreadBadge, getUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

/** Answers the two endpoints the counter reads. */
function mockChatApi(spaces: UnreadSpace[], dms: UnreadDM[]): void {
  apiFetch.mockImplementation((url: string) => {
    const body = url.includes('/chat/dms') ? { dms } : { spaces };
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
  });
}

beforeEach(() => {
  apiFetch.mockReset();
  // The shared loads are page-wide; one test's result must not satisfy the next.
  chatSpacesLoad.invalidate();
  chatDMsLoad.invalidate();
  setUnreadBadge(0);
  setDocumentTitle();
});

afterEach(() => {
  setUnreadBadge(0);
  vi.useRealTimers();
});

describe('unread counting', () => {
  it('sums the server rollup across spaces', () => {
    const spaces: UnreadSpace[] = [{ unreadCount: 2 }, { unreadCount: 0 }, { unreadCount: 5 }];
    // Recomputed from the fixture, so adding a space to it cannot silently
    // leave the expectation behind.
    const expected = spaces.reduce((n, s) => n + (s.unreadCount ?? 0), 0);

    expect(countUnreadSpaces(spaces)).toBe(expected);
    expect(expected).toBeGreaterThan(0);
  });

  it('tolerates a space rollup the server did not send', () => {
    expect(countUnreadSpaces([{}, { unreadCount: 3 }])).toBe(3);
  });

  it('counts unread DMs but not muted ones', () => {
    const dms: UnreadDM[] = [
      { hasUnread: true },
      { hasUnread: true, muted: true },
      { hasUnread: false },
      { hasUnread: true, muted: false },
    ];
    const expected = dms.filter((d) => d.hasUnread && !d.muted).length;

    expect(countUnreadDMs(dms)).toBe(expected);
    // Guard against the fixture drifting into one where muting is untested.
    expect(dms.some((d) => d.hasUnread && d.muted)).toBe(true);
  });
});

describe('tab title badge', () => {
  it('prefixes the title and survives a route change', () => {
    setDocumentTitle('Dashboard');
    setUnreadBadge(3);
    expect(document.title).toBe('(3) Dashboard — Scion');

    // A navigation rewrites the title; the badge must still be there.
    setDocumentTitle('my-project', 'Projects');
    expect(document.title).toBe('(3) my-project — Projects — Scion');
  });

  it('disappears at zero', () => {
    setDocumentTitle('Chat');
    setUnreadBadge(2);
    setUnreadBadge(0);

    expect(document.title).toBe('Chat — Scion');
    expect(getUnreadBadge()).toBe(0);
  });

  it('badges the bare app name too', () => {
    setDocumentTitle();
    setUnreadBadge(1);
    expect(document.title).toBe('(1) Scion');
  });

  it('ignores nonsense counts rather than rendering them', () => {
    setDocumentTitle('Chat');
    setUnreadBadge(-4);
    expect(document.title).toBe('Chat — Scion');

    setUnreadBadge(Number.NaN);
    expect(document.title).toBe('Chat — Scion');
  });
});

describe('ChatUnreadCounter', () => {
  it('adds both halves and shows them in the title', async () => {
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }, { hasUnread: true, muted: true }]);
    setDocumentTitle('Chat');

    await new ChatUnreadCounter().refresh();

    // 2 unread threads + 1 unmuted unread DM.
    expect(getUnreadBadge()).toBe(3);
    expect(document.title).toBe('(3) Chat — Scion');
  });

  it('keeps the last known count when the server is unreachable', async () => {
    mockChatApi([{ unreadCount: 4 }], []);
    const counter = new ChatUnreadCounter();
    await counter.refresh();
    expect(getUnreadBadge()).toBe(4);

    apiFetch.mockRejectedValue(new Error('offline'));
    await counter.refresh();

    expect(getUnreadBadge()).toBe(4);
  });

  it('uses data pushed in by the chat page instead of fetching', () => {
    const counter = new ChatUnreadCounter();

    counter.setSpaceUnread([{ unreadCount: 7 }]);
    counter.setDMUnread([{ hasUnread: true }]);

    expect(getUnreadBadge()).toBe(8);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('shares the startup lists with the chat page and rail instead of fetching its own', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    // The rail and the chat page ask first (or at the same moment).
    const railSpaces = chatSpacesLoad.load({ maxAgeMs: 5_000 });
    const pageDMs = chatDMsLoad.load({ maxAgeMs: 5_000 });
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      await Promise.all([railSpaces, pageDMs]);
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);

      expect(getUnreadBadge()).toBe(3);
      expect(apiFetch).toHaveBeenCalledTimes(2);
    } finally {
      counter.stop();
    }
  });

  it('an event-driven refresh fetches even right after startup', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    expect(apiFetch).toHaveBeenCalledTimes(2);

    // A message arrives: the startup result may predate it.
    mockChatApi([{ unreadCount: 4 }], []);
    counter.scheduleRefresh();
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

    expect(apiFetch).toHaveBeenCalledTimes(4);
    expect(getUnreadBadge()).toBe(4);
    counter.stop();
  });

  it('a message refresh shares the DM fetch the chat page made for the same message', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(0);
    apiFetch.mockClear();

    try {
      vi.advanceTimersByTime(1);
      const event = new CustomEvent('chat-message-received', { detail: {} });
      vi.advanceTimersByTime(1);
      // The page reloads its DM dots the moment the message arrives.
      stateManager.dispatchEvent(event);
      void chatDMsLoad.load();
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      const dmCalls = apiFetch.mock.calls.filter((c) => String(c[0]).endsWith('/chat/dms'));
      const spaceCalls = apiFetch.mock.calls.filter((c) => String(c[0]).endsWith('/chat/spaces'));
      expect(dmCalls).toHaveLength(1);
      expect(spaceCalls).toHaveLength(1);
      expect(getUnreadBadge()).toBe(2);
    } finally {
      counter.stop();
    }
  });

  it('a burst refresh only shares a fetch made after the newest event', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(0);

    try {
      vi.advanceTimersByTime(1);
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      vi.advanceTimersByTime(1);
      // The page fetches DMs for the first message only...
      void chatDMsLoad.load();
      vi.advanceTimersByTime(1);
      // ...and a second message arrives after that fetch was sent.
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      apiFetch.mockClear();
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      // That fetch may predate the second message: the refresh asks again.
      const dmCalls = apiFetch.mock.calls.filter((c) => String(c[0]).endsWith('/chat/dms'));
      expect(dmCalls).toHaveLength(1);
    } finally {
      counter.stop();
    }
  });

  it('coalesces a burst of events into one refresh', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();

    for (let i = 0; i < 10; i++) counter.scheduleRefresh();
    expect(apiFetch).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

    // One refresh = one request per endpoint, not ten.
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

  it('refreshes when a chat message or notification arrives', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    // start() refreshes once, by the deferral bound at the latest.
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    apiFetch.mockClear();

    try {
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      expect(apiFetch).toHaveBeenCalled();
    } finally {
      counter.stop();
    }
  });

  it('keeps the thread count moving while DM pushes arrive on every message', async () => {
    // The scenario this phase exists to serve: a busy conversation while the
    // user is elsewhere. chat.ts calls loadUnreadDMPeers() — and so
    // setDMUnread() — on *every* inbound message, while the rail's own reload
    // is debounced at 2s and reset by each message. If a DM push cancels the
    // pending refresh, nothing is left to advance the space half and the
    // thread count in the tab title freezes for as long as the burst lasts.
    vi.useFakeTimers();
    let serverSpaces: UnreadSpace[] = [{ unreadCount: 0 }];
    let serverDMs: UnreadDM[] = [];
    apiFetch.mockImplementation((url: string) => {
      const body = url.includes('/chat/dms') ? { dms: serverDMs } : { spaces: serverSpaces };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });

    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    expect(getUnreadBadge()).toBe(0);

    try {
      // Threads go unread during the burst, and so does one DM.
      serverSpaces = [{ unreadCount: 3 }, { unreadCount: 1 }];
      serverDMs = [{ hasUnread: true }];

      for (let i = 0; i < 10; i++) {
        stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
        // Messages arrive closer together than the refresh debounce.
        await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS / 2);
        counter.setDMUnread(serverDMs);
      }
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 4);

      const spaceHalf = countUnreadSpaces(serverSpaces);
      // Guard: if the fixture ever drifts to zero unread threads the assertion
      // below would pass without the space half being under test at all.
      expect(spaceHalf).toBeGreaterThan(0);
      expect(getUnreadBadge()).toBe(spaceHalf + countUnreadDMs(serverDMs));
    } finally {
      counter.stop();
    }
  });

  it('keeps the DM count moving while rail loads arrive', async () => {
    // The mirror of the test above, in the other direction. Today the rail's
    // 2s debounce cannot fire inside the badge's 500ms window, so this cannot
    // happen in production — but that is an accident of two constants set
    // independently, and shortening the rail debounce below the badge debounce
    // would silently reintroduce the starvation with nothing to catch it.
    // Neither setter cancels a refresh it only half owns; this pins that.
    vi.useFakeTimers();
    let serverSpaces: UnreadSpace[] = [];
    let serverDMs: UnreadDM[] = [];
    apiFetch.mockImplementation((url: string) => {
      const body = url.includes('/chat/dms') ? { dms: serverDMs } : { spaces: serverSpaces };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });

    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    expect(getUnreadBadge()).toBe(0);

    try {
      serverSpaces = [{ unreadCount: 2 }];
      serverDMs = [{ hasUnread: true }, { hasUnread: true }];

      for (let i = 0; i < 10; i++) {
        stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
        await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS / 2);
        counter.setSpaceUnread(serverSpaces);
      }
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS * 4);

      const dmHalf = countUnreadDMs(serverDMs);
      expect(dmHalf).toBeGreaterThan(0);
      expect(getUnreadBadge()).toBe(countUnreadSpaces(serverSpaces) + dmHalf);
    } finally {
      counter.stop();
    }
  });

  // ChatUnreadCounter has no sender-identity logic — this just pins that an
  // inbound event drives a real 0→unread transition through a full refetch.
  it('updates the badge to a real unread count after a chat-message-received event', async () => {
    vi.useFakeTimers();
    let serverDMs: UnreadDM[] = [{ hasUnread: false }];
    apiFetch.mockImplementation((url: string) => {
      const body = url.includes('/chat/dms')
        ? { dms: serverDMs }
        : { spaces: [{ unreadCount: 0 }] };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    expect(getUnreadBadge()).toBe(0);

    try {
      serverDMs = [{ hasUnread: true }];
      stateManager.dispatchEvent(
        new CustomEvent('chat-message-received', { detail: { senderId: 'other-user' } })
      );
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      expect(getUnreadBadge()).toBe(1);
    } finally {
      counter.stop();
    }
  });

  it('refreshes for a chat notification', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    apiFetch.mockClear();

    try {
      stateManager.dispatchEvent(
        new CustomEvent('notification-created', {
          detail: { state: {}, data: { status: MENTION_STATUS } },
        })
      );
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      expect(apiFetch).toHaveBeenCalled();
    } finally {
      counter.stop();
    }
  });

  it('ignores notifications that cannot change an unread chat count', async () => {
    // Agent-status notifications still broadcast to every session (#1125).
    // Refreshing on them would put /chat/spaces on every page of every
    // signed-in browser for an event about somebody else's agent.
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    apiFetch.mockClear();

    try {
      // User-scoped, but not a chat status.
      stateManager.dispatchEvent(
        new CustomEvent('notification-created', {
          detail: { state: {}, data: { status: 'COMPLETED' } },
        })
      );
      // Unscoped agent-status broadcast: notify() sends the state, no payload.
      stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      expect(apiFetch).not.toHaveBeenCalled();
    } finally {
      counter.stop();
    }
  });

  it('stops listening after stop()', async () => {
    vi.useFakeTimers();
    mockChatApi([], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
    counter.stop();
    apiFetch.mockClear();

    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('shows the count with notification permission denied', async () => {
    // The badge is unread state, not push state: a user who refused desktop
    // popups still gets their own unread count.
    (window as unknown as { Notification: unknown }).Notification = class {
      static permission = 'denied';
    };
    localStorage.setItem('scion-push-notifications', 'false');
    mockChatApi([{ unreadCount: 6 }], []);

    await new ChatUnreadCounter().refresh();

    expect(getUnreadBadge()).toBe(6);
    localStorage.clear();
  });
});

/** Requests the counter sent, by endpoint. */
function chatRequests(): { spaces: number; dms: number } {
  const urls = apiFetch.mock.calls.map((c) => String(c[0]));
  return {
    spaces: urls.filter((u) => u.endsWith('/chat/spaces')).length,
    dms: urls.filter((u) => u.endsWith('/chat/dms')).length,
  };
}

/**
 * A controllable requestIdleCallback. happy-dom has none, so without this the
 * counter takes its timer fallback.
 */
function installIdleCallback(): {
  runIdle: () => void;
  timeouts: Array<number | undefined>;
  cancelled: number[];
  restore: () => void;
} {
  const pending = new Map<number, IdleRequestCallback>();
  const timeouts: Array<number | undefined> = [];
  const cancelled: number[] = [];
  let next = 1;
  const w = window as unknown as {
    requestIdleCallback?: unknown;
    cancelIdleCallback?: unknown;
  };
  w.requestIdleCallback = (cb: IdleRequestCallback, opts?: IdleRequestOptions): number => {
    const id = next++;
    pending.set(id, cb);
    timeouts.push(opts?.timeout);
    return id;
  };
  w.cancelIdleCallback = (id: number): void => {
    cancelled.push(id);
    pending.delete(id);
  };
  return {
    runIdle: () => {
      const cbs = [...pending.values()];
      pending.clear();
      for (const cb of cbs) cb({ didTimeout: false, timeRemaining: () => 50 });
    },
    timeouts,
    cancelled,
    restore: () => {
      delete w.requestIdleCallback;
      delete w.cancelIdleCallback;
    },
  };
}

describe('ChatUnreadCounter first refresh', () => {
  it('sends nothing on start, then one pair at the first idle period', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      await vi.advanceTimersByTimeAsync(0);
      expect(apiFetch).not.toHaveBeenCalled();
      // Bounded: idle may never come on a busy page.
      expect(idle.timeouts).toHaveLength(1);
      expect(idle.timeouts[0]).toBeGreaterThan(0);
      expect(idle.timeouts[0]).toBeLessThanOrEqual(3000);

      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(3);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('falls back to a bounded timer where requestIdleCallback is missing', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS - 1);
      expect(apiFetch).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(INITIAL_REFRESH_MAX_DELAY_MS).toBeLessThanOrEqual(3000);
    } finally {
      counter.stop();
    }
  });

  it('refreshes once for a chat notification that arrives before idle', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      stateManager.dispatchEvent(
        new CustomEvent('notification-created', {
          detail: { state: {}, data: { status: MENTION_STATUS } },
        })
      );
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });

      // The idle period arriving afterwards must not repeat it.
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(1);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('refreshes once when idle comes while a chat event refresh is pending', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      // Idle arrives inside the debounce window.
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS / 2);
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('skips the first refresh after an explicit refresh', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      await counter.refresh();
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('sends nothing when stopped before idle', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.stop();
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(apiFetch).not.toHaveBeenCalled();
      expect(idle.cancelled).toHaveLength(1);
    } finally {
      idle.restore();
    }
  });

  it('sends nothing when stopped before the fallback timer', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    counter.stop();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

    expect(apiFetch).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  describe('with no window (a non-browser environment)', () => {
    afterEach(() => {
      vi.unstubAllGlobals();
    });

    it('takes the timer fallback and sends one pair at the bound', async () => {
      vi.useFakeTimers();
      mockChatApi([{ unreadCount: 1 }], []);
      const counter = new ChatUnreadCounter();
      vi.stubGlobal('window', undefined);
      try {
        expect(typeof window).toBe('undefined');
        expect(() => counter.start()).not.toThrow();
        expect(vi.getTimerCount()).toBe(1);
        await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS - 1);
        expect(apiFetch).not.toHaveBeenCalled();

        await vi.advanceTimersByTimeAsync(1);
        expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      } finally {
        counter.stop();
      }
    });

    it('sends nothing and does not throw when stopped before the bound', async () => {
      vi.useFakeTimers();
      mockChatApi([{ unreadCount: 1 }], []);
      const counter = new ChatUnreadCounter();
      vi.stubGlobal('window', undefined);
      counter.start();
      expect(() => counter.stop()).not.toThrow();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(apiFetch).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    });

    it('does not throw cancelling an idle refresh once the window is gone', async () => {
      vi.useFakeTimers();
      const idle = installIdleCallback();
      mockChatApi([{ unreadCount: 1 }], []);
      const counter = new ChatUnreadCounter();
      try {
        counter.start();
        expect(idle.timeouts).toHaveLength(1);
        vi.stubGlobal('window', undefined);
        expect(() => counter.stop()).not.toThrow();
        await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);
        expect(apiFetch).not.toHaveBeenCalled();
      } finally {
        vi.unstubAllGlobals();
        idle.restore();
      }
    });
  });

  it('reuses the lists the chat page loaded before idle instead of fetching them again', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 4 }], [{ hasUnread: true }, { hasUnread: true, muted: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      // The user opens chat before idle: the rail and the page load both
      // lists and push their halves.
      const spaces = await chatSpacesLoad.load({ maxAgeMs: CHAT_STARTUP_REUSE_MS });
      const dms = await chatDMsLoad.load({ maxAgeMs: CHAT_STARTUP_REUSE_MS });
      counter.setSpaceUnread((spaces?.spaces ?? []) as UnreadSpace[]);
      counter.setDMUnread((dms?.dms ?? []) as UnreadDM[]);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });

      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(5);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('fetches only the list the chat page has not loaded yet', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      const spaces = await chatSpacesLoad.load({ maxAgeMs: CHAT_STARTUP_REUSE_MS });
      counter.setSpaceUnread((spaces?.spaces ?? []) as UnreadSpace[]);
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(3);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('runs no later than a load made since start may be reused', () => {
    // Any list load the chat page started after start() is at most this old
    // when the deferred first refresh runs, so the refresh shares it.
    expect(INITIAL_REFRESH_MAX_DELAY_MS).toBeLessThanOrEqual(CHAT_STARTUP_REUSE_MS);
  });
});

describe('startChatUnreadIfEligible', () => {
  it('starts only for a signed-in user with chat enabled, at once on a chat route', () => {
    const cases: Array<{
      name: string;
      signedIn: boolean;
      chatEnabled: boolean;
      onChatRoute: boolean;
      starts: Array<{ immediate: boolean }>;
    }> = [
      { name: 'signed out', signedIn: false, chatEnabled: true, onChatRoute: true, starts: [] },
      { name: 'chat disabled', signedIn: true, chatEnabled: false, onChatRoute: true, starts: [] },
      {
        name: 'signed out, chat disabled',
        signedIn: false,
        chatEnabled: false,
        onChatRoute: false,
        starts: [],
      },
      {
        name: 'chat route',
        signedIn: true,
        chatEnabled: true,
        onChatRoute: true,
        starts: [{ immediate: true }],
      },
      {
        name: 'non-chat route',
        signedIn: true,
        chatEnabled: true,
        onChatRoute: false,
        starts: [{ immediate: false }],
      },
    ];
    for (const c of cases) {
      const start = vi.fn();
      const started = startChatUnreadIfEligible(
        { start },
        c.signedIn,
        c.chatEnabled,
        c.onChatRoute
      );
      expect(started, c.name).toBe(c.starts.length > 0);
      expect(
        start.mock.calls.map((args) => args[0]),
        c.name
      ).toEqual(c.starts);
    }
  });
});

describe('ChatUnreadCounter started on a chat route', () => {
  it('sends the pair at once and the chat page and rail share it', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      startChatUnreadIfEligible(counter, true, true, true);
      // Sent synchronously, with no idle callback or timer.
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(idle.timeouts).toHaveLength(0);
      expect(vi.getTimerCount()).toBe(0);

      // The page module has loaded: the rail and the page ask for the lists.
      await vi.advanceTimersByTimeAsync(1000);
      await chatSpacesLoad.load({ maxAgeMs: CHAT_STARTUP_REUSE_MS });
      await chatDMsLoad.load({ maxAgeMs: CHAT_STARTUP_REUSE_MS });
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(3);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('still fetches for a chat event right after the startup pair', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      startChatUnreadIfEligible(counter, true, true, true);
      await vi.advanceTimersByTimeAsync(0);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });

      mockChatApi([{ unreadCount: 4 }], []);
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);

      expect(chatRequests()).toEqual({ spaces: 2, dms: 2 });
      expect(getUnreadBadge()).toBe(4);
    } finally {
      counter.stop();
    }
  });
});

describe('ChatUnreadCounter started elsewhere', () => {
  it('sends nothing at start and one pair at the first idle period', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      startChatUnreadIfEligible(counter, true, true, false);
      await vi.advanceTimersByTimeAsync(0);
      expect(apiFetch).not.toHaveBeenCalled();
      expect(idle.timeouts).toEqual([INITIAL_REFRESH_MAX_DELAY_MS]);

      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(1);
    } finally {
      counter.stop();
      idle.restore();
    }
  });
});

describe('ChatUnreadCounter restart', () => {
  it('schedules a new deferred first refresh after stop and start', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.stop();
      expect(idle.cancelled).toHaveLength(1);

      counter.start();
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('sends the pair again when restarted at once', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start({ immediate: true });
      await vi.advanceTimersByTimeAsync(0);
      counter.stop();
      // Past the reuse window, so the restart cannot share the first pair.
      await vi.advanceTimersByTimeAsync(CHAT_STARTUP_REUSE_MS + 1);

      counter.start({ immediate: true });
      await vi.advanceTimersByTimeAsync(0);

      expect(chatRequests()).toEqual({ spaces: 2, dms: 2 });
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      counter.stop();
    }
  });
});
