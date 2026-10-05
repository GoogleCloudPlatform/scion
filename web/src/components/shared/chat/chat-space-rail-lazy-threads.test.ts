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
 * The rail's progressive loading: project names and badges render once the
 * spaces and preferences are in, and thread lists load per space — the
 * expanded and selected spaces only, or every space in the background when
 * activity sort has no per-space activity from the server.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { chatSpacesLoad } from '../../../client/chat-list-cache.js';
import type { ChatSpace, ChatSpaceThread } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

function space(i: number, withActivity = true): ChatSpace {
  return {
    projectId: `p${i}`,
    projectName: `Project ${i}`,
    projectSlug: `project-${i}`,
    unreadCount: 0,
    hasUnreadMention: false,
    ...(withActivity
      ? { lastActivityAt: new Date(Date.UTC(2026, 9, 5, 12 - i)).toISOString() }
      : {}),
  };
}

/** One #general per space; with `withActivity`, newer for lower space numbers. */
function threadsFor(projectId: string, withActivity = false): ChatSpaceThread[] {
  const hour = 12 - Number(projectId.slice(1));
  return [
    {
      id: `${projectId}-general`,
      name: 'general',
      isGeneral: true,
      pinned: false,
      hasUnread: false,
      hasUnreadMention: false,
      ...(withActivity
        ? { lastActivityAt: new Date(Date.UTC(2026, 9, 5, hour)).toISOString() }
        : {}),
    },
  ];
}

interface Server {
  spaces: ChatSpace[];
  prefs: Record<string, unknown>;
  /** Thread requests held until released, in arrival order. */
  held: Array<{ projectId: string; signal: AbortSignal | undefined; release: () => void }>;
  holdThreads: boolean;
  /** Thread lists carry `lastActivityAt` (for activity order from threads). */
  threadActivity: boolean;
}

let server: Server;

function threadRequests(): string[] {
  return apiFetchMock.mock.calls
    .map((c) => String(c[0]))
    .filter((p) => /\/threads$/.test(p))
    .map((p) => decodeURIComponent(p.split('/').at(-2) ?? ''));
}

function spacesRequests(): number {
  return apiFetchMock.mock.calls.filter((c) => c[0] === '/api/v1/chat/spaces').length;
}

function serve(): void {
  apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
    if (path === '/api/v1/chat/spaces') {
      return Promise.resolve(
        new Response(JSON.stringify({ spaces: server.spaces }), { status: 200 })
      );
    }
    if (path === '/api/v1/chat/user-prefs') {
      return Promise.resolve(new Response(JSON.stringify(server.prefs), { status: 200 }));
    }
    const m = path.match(/^\/api\/v1\/chat\/spaces\/([^/]+)\/threads$/);
    if (m) {
      const projectId = decodeURIComponent(m[1]);
      const respond = (): Response =>
        new Response(JSON.stringify({ threads: threadsFor(projectId, server.threadActivity) }), {
          status: 200,
        });
      if (!server.holdThreads) return Promise.resolve(respond());
      return new Promise<Response>((resolve, reject) => {
        const signal = init?.signal ?? undefined;
        signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
        server.held.push({ projectId, signal, release: () => resolve(respond()) });
      });
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
}

function waitForRailLoaded(el: EventTarget): Promise<void> {
  return new Promise((resolve) => {
    el.addEventListener('rail-loaded', () => resolve(), { once: true });
  });
}

async function mount(props: Record<string, string> = {}): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  Object.assign(el, props);
  const loaded = waitForRailLoaded(el);
  document.body.appendChild(el);
  await loaded;
  await el.updateComplete;
  return el;
}

function spaceNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.space-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  server = {
    spaces: [space(0), space(1), space(2), space(3)],
    prefs: {},
    held: [],
    holdThreads: false,
    threadActivity: false,
  };
  chatSpacesLoad.invalidate();
  serve();
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
  localStorage.clear();
});

describe('space rail — progressive thread loading', () => {
  it('renders names and loads no thread list while every space is collapsed', async () => {
    server.holdThreads = true;
    const el = await mount();

    expect(el.loading).toBe(false);
    expect(spaceNames(el)).toEqual(['Project 0', 'Project 1', 'Project 2', 'Project 3']);
    await flush();
    expect(threadRequests()).toEqual([]);
  });

  it('orders spaces by the server-reported activity without thread lists', async () => {
    server.spaces = [space(2), space(0), space(3), space(1)];
    const el = await mount();

    expect(spaceNames(el)).toEqual(['Project 0', 'Project 1', 'Project 2', 'Project 3']);
    expect(threadRequests()).toEqual([]);
  });

  it('expanding a space loads its threads once, showing a loading state meanwhile', async () => {
    server.holdThreads = true;
    const el = await mount();

    el.expandSpace('p2');
    await el.updateComplete;
    expect(threadRequests()).toEqual(['p2']);
    expect(el.shadowRoot.querySelectorAll('.threads-loading')).toHaveLength(1);

    server.held[0].release();
    await flush();
    await el.updateComplete;
    expect(el.shadowRoot.querySelectorAll('.threads-loading')).toHaveLength(0);
    expect(el.shadowRoot.querySelector('.thread-item .thread-name')?.textContent?.trim()).toBe(
      'general'
    );

    // Collapse and expand again: the list is current, nothing to fetch.
    el.handleSpaceHeaderClick(server.spaces[2]);
    await el.updateComplete;
    el.expandSpace('p2');
    await el.updateComplete;
    expect(threadRequests()).toEqual(['p2']);
  });

  it('a routed selection expands its space and loads only its threads', async () => {
    const el = await mount({ selectedKey: 'p3-general', selectedProjectId: 'p3' });
    await flush();

    expect(el.collapsedSpaces.has('p3')).toBe(false);
    expect(threadRequests()).toEqual(['p3']);
    await el.updateComplete;
    expect(
      el.shadowRoot.querySelector('.thread-item.selected .thread-name')?.textContent?.trim()
    ).toBe('general');
  });

  it('a reload refetches visible spaces now and collapsed ones only when next expanded', async () => {
    const el = await mount();
    el.expandSpace('p0');
    el.expandSpace('p1');
    await flush();
    el.handleSpaceHeaderClick(server.spaces[1]); // collapse p1 again
    await el.updateComplete;
    apiFetchMock.mockClear();

    await el.reload();
    await flush();
    expect(threadRequests()).toEqual(['p0']);

    el.expandSpace('p1');
    await flush();
    expect(threadRequests()).toEqual(['p0', 'p1']);
  });

  it('coalesces overlapping reloads into one trailing load', async () => {
    const el = await mount();
    apiFetchMock.mockClear();

    await Promise.all([el.reload(), el.reload(), el.reload(), el.reload()]);

    // The first call's load, then a single trailing one for the other three
    // (the first may have read the server before what prompted them).
    expect(spacesRequests()).toBe(2);
  });

  it('a newer load of a space aborts the one in flight', async () => {
    server.holdThreads = true;
    const el = await mount();
    el.expandSpace('p0');
    await el.updateComplete;
    expect(server.held).toHaveLength(1);

    await el.reload();
    await el.updateComplete;

    expect(server.held).toHaveLength(2);
    expect(server.held[0].signal?.aborted).toBe(true);
    expect(server.held[1].signal?.aborted).toBe(false);
  });

  it('detaching the rail aborts its thread loads', async () => {
    server.holdThreads = true;
    const el = await mount();
    el.expandSpace('p0');
    await el.updateComplete;

    el.remove();

    expect(server.held[0].signal?.aborted).toBe(true);
  });

  it('opening a never-loaded space from its header loads it, then opens #general', async () => {
    const el = await mount();
    const selected = new Promise<string>((resolve) => {
      el.addEventListener(
        'thread-select',
        (e: Event) => resolve((e as CustomEvent).detail.conversationKey),
        { once: true }
      );
    });

    el.handleCollapsedSpaceClick(server.spaces[1]);

    expect(await selected).toBe('p1-general');
    expect(threadRequests()).toEqual(['p1']);
  });

  it('without server activity, activity sort loads the rest in the background, two at a time', async () => {
    server.spaces = [space(0, false), space(1, false), space(2, false), space(3, false)];
    server.holdThreads = true;
    const el = await mount();
    await flush();

    expect(threadRequests()).toEqual(['p0', 'p1']);
    server.held[0].release();
    await flush();
    expect(threadRequests()).toEqual(['p0', 'p1', 'p2']);
    server.held[1].release();
    server.held[2].release();
    await flush();
    server.held[3].release();
    await flush();
    expect(threadRequests()).toEqual(['p0', 'p1', 'p2', 'p3']);
    expect(el.loadingThreads.size).toBe(0);
  });

  it('alpha sort never needs the background loads', async () => {
    server.spaces = [space(0, false), space(1, false)];
    server.prefs = { spaceSortMode: 'alpha' };
    await mount();
    await flush();

    expect(threadRequests()).toEqual([]);
  });

  it('the first load shares a spaces request already made by another owner', async () => {
    const early = chatSpacesLoad.load({ maxAgeMs: 5_000 });
    await mount();
    await early;

    expect(spacesRequests()).toBe(1);
  });
});

describe('space rail — writes to a space whose list never loaded', () => {
  it('marking a never-loaded space read keeps it unloaded, so its header still opens #general', async () => {
    server.spaces = [
      { ...space(0), unreadCount: 2 },
      { ...space(1), unreadCount: 3 },
    ];
    const el = await mount();

    await el.handleMarkSpaceRead('p1');
    expect(el.threadsBySpace.has('p1')).toBe(false);
    expect(el.spaces.find((s: ChatSpace) => s.projectId === 'p1').unreadCount).toBe(0);

    const selected = new Promise<string>((resolve) => {
      el.addEventListener(
        'thread-select',
        (e: Event) => resolve((e as CustomEvent).detail.conversationKey),
        { once: true }
      );
    });
    el.handleCollapsedSpaceClick(el.spaces[1]);
    expect(await selected).toBe('p1-general');
  });

  it('a thread update for a never-loaded space writes nothing', async () => {
    const el = await mount();

    el.updateThread('p2', 'p2-general', { muted: true });

    expect(el.threadsBySpace.has('p2')).toBe(false);
  });

  it('while a never-loaded space loads after a mark-read, it shows the loading state', async () => {
    server.holdThreads = true;
    const el = await mount();
    await el.handleMarkSpaceRead('p3');

    el.expandSpace('p3');
    await el.updateComplete;

    expect(el.shadowRoot.querySelectorAll('.threads-loading')).toHaveLength(1);
  });
});

describe('space rail — first-load epoch', () => {
  it('a thread list requested before the first spaces load is not fetched again', async () => {
    // A cold space deep link asks the rail for a space's threads while the
    // spaces response is still on its way.
    let releaseSpaces: () => void = () => {};
    const spacesHeld = new Promise<void>((resolve) => {
      releaseSpaces = resolve;
    });
    const base = apiFetchMock.getMockImplementation()!;
    apiFetchMock.mockImplementation(async (path: string, init?: RequestInit) => {
      if (path === '/api/v1/chat/spaces') await spacesHeld;
      return base(path, init);
    });
    const el = document.createElement('scion-chat-space-rail') as any;
    el.selectedProjectId = 'p1';
    el.selectedKey = 'p1-general';
    const loaded = waitForRailLoaded(el);
    document.body.appendChild(el);

    const threads = await el.threadsFor('p1');
    expect(threads.map((t: ChatSpaceThread) => t.id)).toEqual(['p1-general']);
    releaseSpaces();
    await loaded;
    await flush();

    expect(threadRequests()).toEqual(['p1']);
  });
});

describe('space rail — read state for a thread in an unloaded space', () => {
  for (const action of ['markThreadRead', 'markThreadUnread'] as const) {
    it(`${action} refreshes the spaces rollup instead of leaving the badge stale`, async () => {
      const el = await mount();
      apiFetchMock.mockClear();

      el[action]('p2-general');
      el[action]('p2-general');
      await flush();

      // Two calls, one coalesced reload pass (plus at most one trailing).
      expect(spacesRequests()).toBeGreaterThanOrEqual(1);
      expect(spacesRequests()).toBeLessThanOrEqual(2);
    });
  }

  it('does nothing once every list is loaded and none holds the thread', async () => {
    server.spaces = [space(0)];
    const el = await mount();
    el.expandSpace('p0');
    await flush();
    apiFetchMock.mockClear();

    el.markThreadRead('elsewhere');
    await flush();

    expect(spacesRequests()).toBe(0);
  });
});

describe('space rail — loading state accessibility', () => {
  it('names the space being loaded and marks its list busy', async () => {
    server.holdThreads = true;
    const el = await mount();

    el.expandSpace('p2');
    await el.updateComplete;

    const status = el.shadowRoot.querySelector('.threads-loading');
    expect(status.getAttribute('role')).toBe('status');
    expect(status.textContent).toContain('Loading threads for Project 2');
    expect(el.shadowRoot.querySelector('.thread-list').getAttribute('aria-busy')).toBe('true');

    server.held[0].release();
    await flush();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.thread-list').getAttribute('aria-busy')).toBe('false');
  });
});

describe('space rail — background order without server activity', () => {
  it('keeps the server order until every background load lands, then sorts once and stays sorted', async () => {
    // Server order 3, 2, 1, 0; activity order is 0, 1, 2, 3.
    server.spaces = [space(3, false), space(2, false), space(1, false), space(0, false)];
    server.threadActivity = true;
    server.holdThreads = true;
    const el = await mount();
    await flush();
    const releaseNext = async (): Promise<void> => {
      server.held.shift()!.release();
      await flush();
      await el.updateComplete;
    };

    await releaseNext();
    expect(spaceNames(el)).toEqual(['Project 3', 'Project 2', 'Project 1', 'Project 0']);
    await releaseNext();
    await releaseNext();
    expect(spaceNames(el)).toEqual(['Project 3', 'Project 2', 'Project 1', 'Project 0']);
    await releaseNext();
    expect(spaceNames(el)).toEqual(['Project 0', 'Project 1', 'Project 2', 'Project 3']);

    // A reload refetches in the background; the order does not fall back.
    await el.reload();
    await el.updateComplete;
    expect(el.loadingThreads.size).toBeGreaterThan(0);
    expect(spaceNames(el)).toEqual(['Project 0', 'Project 1', 'Project 2', 'Project 3']);
  });
});

describe('space rail — reload sharing', () => {
  it('a reload for an event shares a spaces request started after it, and only that', async () => {
    const el = await mount();
    apiFetchMock.mockClear();

    const eventAt = performance.now();
    await new Promise((r) => setTimeout(r, 2));
    // Another owner (the tab-title counter) refreshes for the same event.
    await chatSpacesLoad.load();
    await el.reload({ startedAfter: eventAt });
    expect(spacesRequests()).toBe(1);

    // A plain reload needs a request newer than itself.
    await el.reload();
    expect(spacesRequests()).toBe(2);
  });
});
