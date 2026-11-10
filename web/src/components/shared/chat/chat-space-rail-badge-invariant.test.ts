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
 * The rail's space badge as a whole (ptone/scion#1105).
 *
 * The badge is the server's rollup of a space's unread, unmuted threads
 * (chatSpaceRollupsMode in pkg/hub/handlers_chat_v2.go). The rail adjusts
 * its copy locally after each action rather than reloading it, and every
 * defect in that arithmetic showed up the same way: after some action the
 * rail's number no longer matched what a reload would show.
 *
 * So these tests do not assert hard-coded numbers. A fake server keeps the
 * read and mute state of every thread and answers the rail's requests, and
 * after every action the rail's badge must equal the rollup recomputed from
 * that state.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { chatUnread } from '../../../client/chat-unread.js';
import { requestBodyText } from '../../../client/__fixtures__/request-url.js';
import type { ChatSpace, ChatSpaceThread } from './chat-space-rail.js';

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const PROJECT = 'proj-1';

/** The server's state for one thread. */
interface ServerThread {
  id: string;
  lastMessageId: string;
  lastReadMessageId: string;
  muted: boolean;
  mentioned: boolean;
}

/**
 * A fake hub: thread state, the rollup computed from it, and handlers for the
 * requests the rail's actions make. While `refusing`, every request is
 * answered 403 and changes nothing.
 */
class FakeHub {
  threads = new Map<string, ServerThread>();
  refusing = false;

  constructor(seed: Array<Partial<ServerThread> & { id: string; unread: boolean }>) {
    for (const t of seed) {
      const lastMessageId = `${t.id}-m2`;
      this.threads.set(t.id, {
        id: t.id,
        lastMessageId,
        lastReadMessageId: t.unread ? `${t.id}-m1` : lastMessageId,
        muted: t.muted ?? false,
        mentioned: t.mentioned ?? false,
      });
    }
  }

  private unread(t: ServerThread): boolean {
    return t.lastMessageId !== '' && t.lastMessageId !== t.lastReadMessageId;
  }

  /** The space's unreadCount, as GET /api/v1/chat/spaces computes it. */
  rollup(): number {
    let n = 0;
    for (const t of this.threads.values()) if (!t.muted && this.unread(t)) n++;
    return n;
  }

  /** The space as GET /api/v1/chat/spaces reports it. */
  space(): ChatSpace {
    return {
      projectId: PROJECT,
      projectName: 'Chat Test',
      projectSlug: 'chat-test',
      unreadCount: this.rollup(),
      hasUnreadMention: [...this.threads.values()].some(
        (t) => !t.muted && t.mentioned && this.unread(t)
      ),
    };
  }

  /** The space's threads as GET /api/v1/chat/spaces/{id}/threads reports them. */
  threadList(): ChatSpaceThread[] {
    return [...this.threads.values()].map((t) => ({
      id: t.id,
      name: t.id,
      isGeneral: false,
      pinned: false,
      muted: t.muted,
      lastMessageId: t.lastMessageId,
      hasUnread: this.unread(t),
      hasUnreadMention: this.unread(t) && t.mentioned,
    }));
  }

  /** Mark read from another view (the open thread advancing the watermark). */
  read(id: string): void {
    const t = this.threads.get(id)!;
    t.lastReadMessageId = t.lastMessageId;
  }

  /** Mark unread (the endpoint moves the watermark back one message). */
  unreadAgain(id: string): void {
    const t = this.threads.get(id)!;
    t.lastReadMessageId = `${id}-m1`;
  }

  handle(path: string, init?: RequestInit): Response {
    const conv = /^\/api\/v1\/chat\/conversations\/([^/]+)\/(read|unread|mute)$/.exec(path);
    const space = /^\/api\/v1\/chat\/spaces\/([^/]+)\/read$/.exec(path);
    if (!conv && !space) return new Response('{}', { status: 200 });
    if (this.refusing) return new Response('{}', { status: 403 });
    if (space) {
      for (const t of this.threads.values()) t.lastReadMessageId = t.lastMessageId;
      return new Response('{}', { status: 200 });
    }
    const id = decodeURIComponent(conv![1]);
    const t = this.threads.get(id);
    if (!t) return new Response('{}', { status: 404 });
    switch (conv![2]) {
      case 'read': {
        const body = JSON.parse(requestBodyText(init?.body)) as { messageId?: string };
        if (!body.messageId) return new Response('{}', { status: 400 });
        t.lastReadMessageId = body.messageId;
        return new Response('{}', { status: 200 });
      }
      case 'unread':
        this.unreadAgain(id);
        return new Response('{}', { status: 200 });
      default: {
        const body = JSON.parse(requestBodyText(init?.body)) as { muted?: boolean };
        t.muted = body.muted === true;
        return new Response(JSON.stringify({ muted: t.muted }), { status: 200 });
      }
    }
  }
}

/** The rail's private surface these tests drive, as the context menu does. */
interface RailInternals {
  spaces: ChatSpace[];
  threadsBySpace: Map<string, ChatSpaceThread[]>;
  collapsedSpaces: Set<string>;
  loading: boolean;
  handleMarkRead(thread: ChatSpaceThread, projectId: string): Promise<void>;
  handleMarkUnread(thread: ChatSpaceThread, projectId: string): Promise<void>;
  handleToggleMute(thread: ChatSpaceThread, projectId: string): Promise<void>;
  handleMarkSpaceRead(projectId: string): Promise<void>;
  markThreadRead(threadId: string): void;
  markThreadUnread(threadId: string): void;
}

/** A rail showing the hub's space with its thread list loaded. */
function createRail(hub: FakeHub): RailInternals {
  const el = document.createElement('scion-chat-space-rail') as unknown as RailInternals;
  el.spaces = [hub.space()];
  el.threadsBySpace = new Map([[PROJECT, hub.threadList()]]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  return el;
}

function badge(rail: RailInternals): number {
  return rail.spaces.find((s) => s.projectId === PROJECT)!.unreadCount;
}

/** The thread as the rail holds it now: what a context menu opened on it gets. */
function shown(rail: RailInternals, id: string): ChatSpaceThread {
  return rail.threadsBySpace.get(PROJECT)!.find((t) => t.id === id)!;
}

type Action =
  | 'mark read'
  | 'mark unread'
  | 'toggle mute'
  | 'mark space read'
  | 'read in the thread view'
  | 'unread in another tab';

const ACTIONS: Action[] = [
  'mark read',
  'mark unread',
  'toggle mute',
  'mark space read',
  'read in the thread view',
  'unread in another tab',
];

/** Run one action the way the UI does: the request, then the rail's update. */
async function act(rail: RailInternals, hub: FakeHub, action: Action, id: string): Promise<void> {
  switch (action) {
    case 'mark read':
      return rail.handleMarkRead(shown(rail, id), PROJECT);
    case 'mark unread':
      return rail.handleMarkUnread(shown(rail, id), PROJECT);
    case 'toggle mute':
      return rail.handleToggleMute(shown(rail, id), PROJECT);
    case 'mark space read':
      return rail.handleMarkSpaceRead(PROJECT);
    case 'read in the thread view':
      // The thread view's own POST is not refused here: it reports to the
      // rail only once it has succeeded.
      hub.read(id);
      rail.markThreadRead(id);
      return;
    case 'unread in another tab':
      hub.unreadAgain(id);
      rail.markThreadUnread(id);
      return;
  }
}

/** Every combination of read state, mute and mention a thread can start in. */
const SEED = [
  { id: 'unread', unread: true },
  { id: 'read', unread: false },
  { id: 'muted-unread', unread: true, muted: true },
  { id: 'muted-read', unread: false, muted: true },
  { id: 'mentioned', unread: true, mentioned: true },
  { id: 'muted-mentioned', unread: true, muted: true, mentioned: true },
];

/** Deterministic pseudo-random numbers (mulberry32), so a failure replays. */
function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

let hub: FakeHub;

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  hub = new FakeHub(SEED);
  apiFetchMock.mockImplementation((path: string, init?: RequestInit) =>
    Promise.resolve(hub.handle(path, init))
  );
  // A saved mute schedules a refresh of the global unread count; it is not
  // what these tests are about.
  vi.spyOn(chatUnread, 'scheduleRefresh').mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
  document.body.innerHTML = '';
});

describe('space rail — the badge equals the server rollup', () => {
  it('starts equal to the rollup', () => {
    const rail = createRail(hub);
    expect(badge(rail)).toBe(hub.rollup());
    expect(hub.rollup()).toBe(2);
  });

  describe.each(ACTIONS)('after %s', (action) => {
    it.each(SEED.map((t) => t.id))('on a thread that starts %s', async (id) => {
      const rail = createRail(hub);
      await act(rail, hub, action, id);
      expect(badge(rail)).toBe(hub.rollup());
      // The same action again (a second click, a repeated event).
      await act(rail, hub, action, id);
      expect(badge(rail)).toBe(hub.rollup());
    });

    it.each(SEED.map((t) => t.id))('refused, on a thread that starts %s', async (id) => {
      const rail = createRail(hub);
      hub.refusing = true;
      await act(rail, hub, action, id);
      hub.refusing = false;
      expect(badge(rail)).toBe(hub.rollup());
    });
  });

  it.each([1, 2, 3, 4, 5, 6, 7, 8])('through a random run of actions (seed %i)', async (seed) => {
    const next = prng(seed);
    const pick = <T>(xs: readonly T[]): T => xs[Math.floor(next() * xs.length)];
    const rail = createRail(hub);
    const log: string[] = [];
    for (let step = 0; step < 60; step++) {
      const action = pick(ACTIONS);
      const id = pick(SEED).id;
      hub.refusing = next() < 0.15 && action !== 'read in the thread view';
      log.push(`${action} ${id}${hub.refusing ? ' (refused)' : ''}`);
      await act(rail, hub, action, id);
      hub.refusing = false;
      expect(badge(rail), log.join(' → ')).toBe(hub.rollup());
    }
  });
});
