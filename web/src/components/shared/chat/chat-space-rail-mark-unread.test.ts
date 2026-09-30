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
 * Tests for the rail's "Mark unread" context-menu action.
 *
 * Unlike mute/pin, mark-unread is not optimistic: the server call happens
 * first and the local state (and space badge) only update on success — the
 * same shape as handleMarkRead, just in the opposite direction.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const SPACE = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 0,
  hasUnreadMention: false,
};

function thread(overrides: Record<string, unknown> = {}): any {
  return {
    id: 'topic-1',
    name: 'deploys',
    isGeneral: false,
    pinned: false,
    muted: false,
    lastMessageId: 'm-1',
    hasUnread: false,
    hasUnreadMention: false,
    ...overrides,
  };
}

/** A rail with one expanded space holding the given threads. */
function createRail(threads: any[]): any {
  const el = document.createElement('scion-chat-space-rail') as any;
  el.spaces = [SPACE];
  el.threadsBySpace = new Map([[SPACE.projectId, threads]]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  return el;
}

/** The rail's copy of a thread after an action has run. */
function storedThread(el: any, id = 'topic-1'): any {
  return (el.threadsBySpace.get(SPACE.projectId) || []).find((t: any) => t.id === id);
}

/** The rail's copy of the space badge. */
function badge(el: any): number {
  return el.spaces.find((s: any) => s.projectId === SPACE.projectId).unreadCount;
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('space rail — mark unread', () => {
  it('POSTs the unread endpoint and marks the thread unread', async () => {
    const el = createRail([thread()]);

    await el.handleMarkUnread(thread(), SPACE.projectId);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/topic-1/unread',
      expect.objectContaining({ method: 'POST' })
    );
    expect(storedThread(el).hasUnread).toBe(true);
  });

  it('dispatches conversation-marked-unread on success, for same-tab suppression without the SSE round trip', async () => {
    const el = createRail([thread()]);
    document.body.appendChild(el);
    const handler = vi.fn();
    el.addEventListener('conversation-marked-unread', handler);

    await el.handleMarkUnread(thread(), SPACE.projectId);

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0].detail).toEqual({ conversationKey: 'topic-1' });
  });

  it('does not dispatch conversation-marked-unread when the server refuses', async () => {
    const el = createRail([thread()]);
    document.body.appendChild(el);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));
    const handler = vi.fn();
    el.addEventListener('conversation-marked-unread', handler);

    await el.handleMarkUnread(thread(), SPACE.projectId);

    expect(handler).not.toHaveBeenCalled();
  });

  it('bumps the space badge on success', async () => {
    const el = createRail([thread()]);

    await el.handleMarkUnread(thread(), SPACE.projectId);

    expect(badge(el)).toBe(1);
  });

  it('does not bump the badge for a muted thread', async () => {
    const el = createRail([thread({ muted: true })]);

    await el.handleMarkUnread(thread({ muted: true }), SPACE.projectId);

    expect(storedThread(el).hasUnread).toBe(true);
    expect(badge(el)).toBe(0);
  });

  it('leaves state alone when the server refuses', async () => {
    const el = createRail([thread()]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.handleMarkUnread(thread(), SPACE.projectId);

    expect(storedThread(el).hasUnread).toBe(false);
    expect(badge(el)).toBe(0);
  });

  it('is a no-op for an already-unread thread — no request is sent', async () => {
    const el = createRail([thread({ hasUnread: true })]);
    el.spaces = [{ ...SPACE, unreadCount: 1 }];

    await el.handleMarkUnread(thread({ hasUnread: true }), SPACE.projectId);

    expect(apiFetchMock).not.toHaveBeenCalled();
    expect(badge(el)).toBe(1);
  });

  it('is a no-op for a thread with no messages — no request is sent', async () => {
    const el = createRail([thread({ lastMessageId: undefined })]);

    await el.handleMarkUnread(thread({ lastMessageId: undefined }), SPACE.projectId);

    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('markThreadUnread (used for cross-tab sync) sets the flag and bumps the badge once', () => {
    const el = createRail([thread()]);

    el.markThreadUnread('topic-1');
    el.markThreadUnread('topic-1'); // idempotent: a second call must not double-count

    expect(storedThread(el).hasUnread).toBe(true);
    expect(badge(el)).toBe(1);
  });
});

describe('space rail — mark-unread menu visibility', () => {
  async function mount(threads: any[]): Promise<any> {
    const el = createRail(threads);
    document.body.appendChild(el);
    await new Promise((resolve) => setTimeout(resolve, 0));
    el.spaces = [SPACE];
    el.threadsBySpace = new Map([[SPACE.projectId, threads]]);
    el.collapsedSpaces = new Set<string>();
    el.loading = false;
    await el.updateComplete;
    return el;
  }

  function menuItemNames(el: any): string[] {
    return Array.from(el.shadowRoot.querySelectorAll('.context-menu-item')).map(
      (n) => (n as HTMLElement).textContent?.trim() ?? ''
    );
  }

  it('offers Mark unread for a read thread with messages', async () => {
    const el = await mount([thread()]);
    el.contextMenuTarget = { type: 'thread', thread: thread(), projectId: SPACE.projectId };
    await el.updateComplete;

    expect(menuItemNames(el).some((t) => t === 'Mark unread')).toBe(true);
  });

  it('hides Mark unread for an already-unread thread', async () => {
    const el = await mount([thread({ hasUnread: true })]);
    el.contextMenuTarget = {
      type: 'thread',
      thread: thread({ hasUnread: true }),
      projectId: SPACE.projectId,
    };
    await el.updateComplete;

    expect(menuItemNames(el).some((t) => t === 'Mark unread')).toBe(false);
  });

  it('hides Mark unread for a thread with no messages', async () => {
    const el = await mount([thread({ lastMessageId: undefined })]);
    el.contextMenuTarget = {
      type: 'thread',
      thread: thread({ lastMessageId: undefined }),
      projectId: SPACE.projectId,
    };
    await el.updateComplete;

    expect(menuItemNames(el).some((t) => t === 'Mark unread')).toBe(false);
  });
});
