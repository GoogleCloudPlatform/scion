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
 * Tests for the rail's mute and pin context-menu actions (#1029, #1030).
 *
 * Both toggles apply locally before the server answers so the rail reorders on
 * the click, and both roll back when the PUT fails — a pin or mute the server
 * does not have must not survive on screen. Muting also suppresses the unread
 * marker, which is the visible payoff of the feature.
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
  unreadCount: 1,
  hasUnreadMention: false,
};

function thread(overrides: Record<string, unknown> = {}): any {
  return {
    id: 'topic-1',
    name: 'deploys',
    isGeneral: false,
    pinned: false,
    muted: false,
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

describe('space rail — mute toggle', () => {
  it('PUTs the mute endpoint and marks the thread muted', async () => {
    const el = createRail([thread()]);
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ muted: true }), { status: 200 }));

    await el.handleToggleMute(thread(), SPACE.projectId);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/topic-1/mute',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ muted: true }) })
    );
    expect(storedThread(el).muted).toBe(true);
  });

  it('unmutes a muted thread', async () => {
    const el = createRail([thread({ muted: true })]);
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ muted: false }), { status: 200 }));

    await el.handleToggleMute(thread({ muted: true }), SPACE.projectId);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/topic-1/mute',
      expect.objectContaining({ body: JSON.stringify({ muted: false }) })
    );
    expect(storedThread(el).muted).toBe(false);
  });

  it('rolls back when the server refuses', async () => {
    const el = createRail([thread()]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 403 }));

    await el.handleToggleMute(thread(), SPACE.projectId);

    expect(storedThread(el).muted).toBe(false);
  });
});

/**
 * The space badge is a server-side rollup that skips muted threads, so every
 * local adjustment the rail makes has to skip them too. Getting this wrong is
 * invisible until the counts drift apart.
 */
describe('space rail — space badge follows mute', () => {
  /** The rail's copy of the space badge. */
  function badge(el: any): number {
    return el.spaces.find((s: any) => s.projectId === SPACE.projectId).unreadCount;
  }

  it('takes an unread thread out of the badge when it is muted', async () => {
    const el = createRail([thread({ hasUnread: true })]);
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ muted: true }), { status: 200 }));

    await el.handleToggleMute(thread({ hasUnread: true }), SPACE.projectId);

    expect(badge(el)).toBe(0);
  });

  it('puts it back when the thread is unmuted', async () => {
    const el = createRail([thread({ hasUnread: true, muted: true })]);
    el.spaces = [{ ...SPACE, unreadCount: 0 }];
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ muted: false }), { status: 200 }));

    await el.handleToggleMute(thread({ hasUnread: true, muted: true }), SPACE.projectId);

    expect(badge(el)).toBe(1);
  });

  it('restores the badge when the mute is rolled back', async () => {
    const el = createRail([thread({ hasUnread: true })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.handleToggleMute(thread({ hasUnread: true }), SPACE.projectId);

    expect(badge(el)).toBe(1);
  });

  it('leaves the badge alone for a read thread', async () => {
    const el = createRail([thread({ hasUnread: false })]);
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ muted: true }), { status: 200 }));

    await el.handleToggleMute(thread({ hasUnread: false }), SPACE.projectId);

    expect(badge(el)).toBe(1);
  });

  it('does not decrement the badge when a muted thread is marked read', async () => {
    const el = createRail([thread({ hasUnread: true, muted: true, lastMessageId: 'm-1' })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));

    await el.handleMarkRead(
      thread({ hasUnread: true, muted: true, lastMessageId: 'm-1' }),
      SPACE.projectId
    );

    expect(storedThread(el).hasUnread).toBe(false);
    expect(badge(el)).toBe(1);
  });

  it('still decrements for an unmuted thread marked read', async () => {
    const el = createRail([thread({ hasUnread: true, lastMessageId: 'm-1' })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));

    await el.handleMarkRead(thread({ hasUnread: true, lastMessageId: 'm-1' }), SPACE.projectId);

    expect(badge(el)).toBe(0);
  });

  it('leaves the badge alone when an already-read thread is marked read', async () => {
    // "Mark as read" is offered on every thread, read or not. Each click used
    // to take one off the badge, so a few clicks on a read thread could drive
    // it to zero while other threads still showed their unread dots.
    const el = createRail([
      thread({ id: 'topic-1', hasUnread: false, lastMessageId: 'm-1' }),
      thread({ id: 'topic-2', hasUnread: true }),
    ]);
    el.spaces = [{ ...SPACE, unreadCount: 1 }];
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));

    const target = thread({ id: 'topic-1', hasUnread: false, lastMessageId: 'm-1' });
    await el.handleMarkRead(target, SPACE.projectId);
    await el.handleMarkRead(target, SPACE.projectId);

    expect(badge(el)).toBe(1);
    expect(storedThread(el, 'topic-2').hasUnread).toBe(true);
  });

  it('leaves the badge alone when the server refuses the mark-read', async () => {
    const el = createRail([thread({ hasUnread: true, lastMessageId: 'm-1' })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.handleMarkRead(thread({ hasUnread: true, lastMessageId: 'm-1' }), SPACE.projectId);

    expect(storedThread(el).hasUnread).toBe(true);
    expect(badge(el)).toBe(1);
  });

  it('markThreadRead skips the badge for a muted thread', () => {
    const el = createRail([thread({ hasUnread: true, muted: true })]);

    el.markThreadRead('topic-1');

    expect(storedThread(el).hasUnread).toBe(false);
    expect(badge(el)).toBe(1);
  });

  it('clears the space badge when mark-all-read succeeds', async () => {
    const el = createRail([thread({ hasUnread: true })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));

    await el.handleMarkSpaceRead(SPACE.projectId);

    expect(storedThread(el).hasUnread).toBe(false);
    expect(badge(el)).toBe(0);
  });

  it('keeps the space badge when the server refuses mark-all-read', async () => {
    const el = createRail([thread({ hasUnread: true })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.handleMarkSpaceRead(SPACE.projectId);

    expect(storedThread(el).hasUnread).toBe(true);
    expect(badge(el)).toBe(1);
  });
});

describe('space rail — pin toggle', () => {
  it('PUTs the pin endpoint and marks the thread pinned', async () => {
    const el = createRail([thread()]);
    apiFetchMock.mockResolvedValue(new Response(JSON.stringify({ pinned: true }), { status: 200 }));

    await el.handleTogglePin(thread(), SPACE.projectId);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/topic-1/pin',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ pinned: true }) })
    );
    expect(storedThread(el).pinned).toBe(true);
  });

  it('rolls back when the server refuses', async () => {
    const el = createRail([thread({ pinned: true })]);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.handleTogglePin(thread({ pinned: true }), SPACE.projectId);

    expect(storedThread(el).pinned).toBe(true);
  });

  it('sorts a pinned thread above an unpinned one', () => {
    const el = createRail([
      thread({ id: 'topic-plain', name: 'plain' }),
      thread({ id: 'topic-pinned', name: 'pinned', pinned: true }),
    ]);

    const order = el.getSortedThreads(SPACE.projectId).map((t: any) => t.id);

    expect(order).toEqual(['topic-pinned', 'topic-plain']);
  });
});

describe('space rail — muted rendering', () => {
  async function mount(threads: any[]): Promise<any> {
    const el = createRail(threads);
    document.body.appendChild(el);
    // connectedCallback kicks off a load that resets state from the (mocked)
    // API; let it settle, then put the fixture back and re-render.
    await new Promise((resolve) => setTimeout(resolve, 0));
    el.spaces = [SPACE];
    el.threadsBySpace = new Map([[SPACE.projectId, threads]]);
    el.collapsedSpaces = new Set<string>();
    el.loading = false;
    await el.updateComplete;
    return el;
  }

  it('shows the unread dot for an unmuted thread', async () => {
    const el = await mount([thread({ hasUnread: true })]);

    expect(el.shadowRoot.querySelector('.unread-dot')).not.toBeNull();
    expect(el.shadowRoot.querySelector('sl-icon[name="bell-slash"]')).toBeNull();
  });

  it('suppresses the unread dot and shows bell-slash for a muted thread', async () => {
    const el = await mount([thread({ hasUnread: true, muted: true })]);

    expect(el.shadowRoot.querySelector('.unread-dot')).toBeNull();
    expect(el.shadowRoot.querySelector('.thread-name.unread')).toBeNull();
    expect(el.shadowRoot.querySelector('sl-icon[name="bell-slash"]')).not.toBeNull();
  });

  it('suppresses the mention dot for a muted thread', async () => {
    const el = await mount([thread({ hasUnread: true, hasUnreadMention: true, muted: true })]);

    expect(el.shadowRoot.querySelector('.mention-dot')).toBeNull();
  });

  it('offers Mute and Pin to top in the context menu, and their inverses when set', async () => {
    const el = await mount([thread()]);
    el.contextMenuTarget = { type: 'thread', thread: thread(), projectId: SPACE.projectId };
    await el.updateComplete;

    expect(el.shadowRoot.querySelector('.mute-toggle')?.textContent?.trim()).toBe('Mute');
    expect(el.shadowRoot.querySelector('.pin-toggle')?.textContent?.trim()).toBe('Pin to top');

    el.contextMenuTarget = {
      type: 'thread',
      thread: thread({ muted: true, pinned: true }),
      projectId: SPACE.projectId,
    };
    await el.updateComplete;

    expect(el.shadowRoot.querySelector('.mute-toggle')?.textContent?.trim()).toBe('Unmute');
    expect(el.shadowRoot.querySelector('.pin-toggle')?.textContent?.trim()).toBe('Unpin');
  });

  it('marks the menu glyphs with the current state, not the pending action', async () => {
    const el = await mount([thread()]);
    el.contextMenuTarget = { type: 'thread', thread: thread(), projectId: SPACE.projectId };
    await el.updateComplete;

    const glyph = (selector: string): string | null | undefined =>
      el.shadowRoot.querySelector(`${selector} sl-icon`)?.getAttribute('name');

    expect(glyph('.pin-toggle')).toBe('star');
    expect(glyph('.mute-toggle')).toBe('bell');

    el.contextMenuTarget = {
      type: 'thread',
      thread: thread({ muted: true, pinned: true }),
      projectId: SPACE.projectId,
    };
    await el.updateComplete;

    expect(glyph('.pin-toggle')).toBe('star-fill');
    expect(glyph('.mute-toggle')).toBe('bell-slash');
  });
});
