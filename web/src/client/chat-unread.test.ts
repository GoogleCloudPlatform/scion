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
    // start() refreshes once immediately.
    await vi.advanceTimersByTimeAsync(0);
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
    await vi.advanceTimersByTimeAsync(0);
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
    await vi.advanceTimersByTimeAsync(0);
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

  it('refreshes for a chat notification', async () => {
    vi.useFakeTimers();
    mockChatApi([{ unreadCount: 1 }], []);
    const counter = new ChatUnreadCounter();
    counter.start();
    await vi.advanceTimersByTimeAsync(0);
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
    await vi.advanceTimersByTimeAsync(0);
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
    await vi.advanceTimersByTimeAsync(0);
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
