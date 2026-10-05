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

  it('sends nothing when both halves were pushed before idle', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 9 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.setSpaceUnread([{ unreadCount: 4 }]);
      counter.setDMUnread([{ hasUnread: true }, { hasUnread: true, muted: true }]);
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS);

      expect(apiFetch).not.toHaveBeenCalled();
      expect(getUnreadBadge()).toBe(5);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('still fetches both halves when only one was pushed before idle', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.setSpaceUnread([{ unreadCount: 2 }]);
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(3);
    } finally {
      counter.stop();
      idle.restore();
    }
  });
});

describe('ChatUnreadCounter hold for chat page pushes', () => {
  it('sends nothing when the page pushes both halves during the hold', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 9 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.holdFirstRefreshForPagePushes();
      // Idle arriving while the page's own requests are in flight is ignored.
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(1000);
      expect(apiFetch).not.toHaveBeenCalled();

      counter.setSpaceUnread([{ unreadCount: 3 }]);
      counter.setDMUnread([{ hasUnread: true }]);
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(apiFetch).not.toHaveBeenCalled();
      expect(getUnreadBadge()).toBe(4);
      expect(idle.cancelled).toHaveLength(1);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('fetches the pair at the bound when only one half was pushed', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 2 }], [{ hasUnread: true }]);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.holdFirstRefreshForPagePushes();
      counter.setSpaceUnread([{ unreadCount: 2 }]);
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS - 1);
      expect(apiFetch).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(getUnreadBadge()).toBe(3);
    } finally {
      counter.stop();
    }
  });

  it('fetches the pair at the bound when nothing was pushed', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.holdFirstRefreshForPagePushes();
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS - 1);
      expect(apiFetch).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('measures the bound from start, not from the hold', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    const idle = installIdleCallback();
    try {
      counter.start();
      await vi.advanceTimersByTimeAsync(1000);
      counter.holdFirstRefreshForPagePushes();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS - 1000 - 1);
      expect(apiFetch).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('has no effect once the first refresh has run', async () => {
    vi.useFakeTimers();
    const idle = installIdleCallback();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      idle.runIdle();
      await vi.advanceTimersByTimeAsync(0);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });

      counter.holdFirstRefreshForPagePushes();
      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      counter.stop();
      idle.restore();
    }
  });

  it('sends nothing and leaves no timer when stopped during the hold', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    counter.holdFirstRefreshForPagePushes();
    await vi.advanceTimersByTimeAsync(1000);
    counter.stop();
    expect(vi.getTimerCount()).toBe(0);

    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('never holds a refresh for a live chat event', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    try {
      counter.start();
      counter.holdFirstRefreshForPagePushes();
      stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
      await vi.advanceTimersByTimeAsync(UNREAD_REFRESH_DEBOUNCE_MS + 1);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });

      await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);
      expect(chatRequests()).toEqual({ spaces: 1, dms: 1 });
    } finally {
      counter.stop();
    }
  });

  it('does nothing when the counter was never started', async () => {
    // Chat disabled or nobody signed in: the chat page's hold must not turn
    // into a fetch of its own.
    vi.useFakeTimers();
    mockChatApi([], []);
    const counter = new ChatUnreadCounter();
    counter.holdFirstRefreshForPagePushes();
    await vi.advanceTimersByTimeAsync(INITIAL_REFRESH_MAX_DELAY_MS * 2);

    expect(apiFetch).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('startChatUnreadIfEligible', () => {
  it('starts only for a signed-in user with chat enabled', () => {
    const cases: Array<[boolean, boolean, boolean]> = [
      [true, true, true],
      [true, false, false],
      [false, true, false],
      [false, false, false],
    ];
    for (const [signedIn, chatEnabled, expected] of cases) {
      const start = vi.fn();
      expect(startChatUnreadIfEligible({ start }, signedIn, chatEnabled)).toBe(expected);
      expect(start).toHaveBeenCalledTimes(expected ? 1 : 0);
    }
  });
});
