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
  /** Thread lists report #general unread. */
  threadUnread: boolean;
  /** Status for thread-list responses per space (default 200). */
  threadStatus: Record<string, number>;
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
      const respond = (): Response => {
        const status = server.threadStatus[projectId] ?? 200;
        if (status !== 200) return new Response('{}', { status });
        const threads = threadsFor(projectId, server.threadActivity).map((t) =>
          server.threadUnread ? { ...t, hasUnread: true } : t
        );
        return new Response(JSON.stringify({ threads }), { status: 200 });
      };
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
    threadUnread: false,
    threadStatus: {},
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

  it('a reload lets an in-flight thread load land, then refetches it once (busy hub)', async () => {
    // Reproduces a slow hub under a message stream: the page reloads the
    // rail every burst, faster than the thread list answers.
    server.holdThreads = true;
    const el = await mount();
    el.expandSpace('p0');
    await el.updateComplete;
    expect(server.held).toHaveLength(1);

    for (let round = 0; round < 3; round++) {
      await el.reload();
      await el.updateComplete;
      // Nothing aborted, nothing restarted while the request is in flight.
      expect(server.held.every((h) => !h.signal?.aborted)).toBe(true);
    }
    expect(threadRequests()).toEqual(['p0']);

    server.held.shift()!.release();
    await flush();
    await el.updateComplete;
    expect(el.threadsBySpace.has('p0')).toBe(true);
    expect(el.shadowRoot.querySelectorAll('.threads-loading')).toHaveLength(0);
    // The landed list predates the reloads: exactly one trailing fetch.
    expect(threadRequests()).toEqual(['p0', 'p0']);

    server.held.shift()!.release();
    await flush();
    expect(threadRequests()).toEqual(['p0', 'p0']);
    expect(el.loadingThreads.size).toBe(0);
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

describe('space rail — read state while the selected list is loading', () => {
  it('opening a thread whose list is in flight applies the read when it lands, without a reload', async () => {
    // Reproduces a cold deep link: the thread view advances its watermark
    // (read POST, then read-state-updated) before the space's list arrives.
    server.spaces = [space(0), { ...space(1), unreadCount: 1 }];
    server.threadUnread = true;
    server.holdThreads = true;
    const el = await mount({ selectedKey: 'p1-general', selectedProjectId: 'p1' });
    await flush();
    expect(threadRequests()).toEqual(['p1']);

    el.markThreadRead('p1-general');
    await flush();
    expect(spacesRequests()).toBe(1);
    expect(threadRequests()).toEqual(['p1']);
    expect(server.held[0].signal?.aborted).toBe(false);

    server.held[0].release();
    await flush();
    await el.updateComplete;

    // The list was requested before the read: its dot is cleared on landing,
    // and the badge with it.
    expect(el.threadsBySpace.get('p1')[0].hasUnread).toBe(false);
    expect(el.spaces.find((s: ChatSpace) => s.projectId === 'p1').unreadCount).toBe(0);
    expect(spacesRequests()).toBe(1);
    expect(threadRequests()).toEqual(['p1']);
  });

  it('falls back to a rollup refresh once nothing is loading and the thread turned up nowhere', async () => {
    server.holdThreads = true;
    server.threadStatus = { p1: 500 };
    const el = await mount({ selectedKey: 'p1-general', selectedProjectId: 'p1' });
    await flush();
    el.markThreadRead('p1-general');
    apiFetchMock.mockClear();

    // The failed list is retried once automatically; both answers fail.
    server.held.shift()!.release();
    await flush();
    server.held.shift()!.release();
    await flush();

    expect(spacesRequests()).toBe(1);
  });
});

describe('space rail — failed thread loads', () => {
  it('retries a failed load once automatically, then waits for the user', async () => {
    server.threadStatus = { p0: 500 };
    const el = await mount();
    el.expandSpace('p0');
    await flush();
    expect(threadRequests()).toEqual(['p0', 'p0']);

    // Collapse and expand again: an explicit request, so it tries again.
    server.threadStatus = {};
    el.handleSpaceHeaderClick(el.spaces[0]);
    await el.updateComplete;
    el.expandSpace('p0');
    await flush();
    await el.updateComplete;
    expect(threadRequests()).toEqual(['p0', 'p0', 'p0']);
    expect(el.threadsBySpace.get('p0')).toHaveLength(1);
  });

  it('an explicit request (a space deep link) loads again after failures, without an expand', async () => {
    server.threadStatus = { p1: 500 };
    const el = await mount();
    expect(await el.threadsFor('p1')).toEqual([]);
    expect(await el.threadsFor('p1')).toEqual([]);
    expect(threadRequests()).toEqual(['p1', 'p1']);

    server.threadStatus = {};
    const threads = await el.threadsFor('p1');

    expect(threads.map((t: ChatSpaceThread) => t.id)).toEqual(['p1-general']);
    expect(el.collapsedSpaces.has('p1')).toBe(true);
  });

  it('a header click on a space whose load failed loads it again and opens #general', async () => {
    server.threadStatus = { p1: 500 };
    const el = await mount();
    el.expandSpace('p1');
    await flush();
    el.handleSpaceHeaderClick(el.spaces[1]); // collapse
    await el.updateComplete;
    server.threadStatus = {};
    const selected = new Promise<string>((resolve) => {
      el.addEventListener(
        'thread-select',
        (e: Event) => resolve((e as CustomEvent).detail.conversationKey),
        { once: true }
      );
    });

    const before = threadRequests().length;
    el.handleCollapsedSpaceClick(el.spaces[1]);

    expect(await selected).toBe('p1-general');
    await flush();
    // One fetch: the expand does not retry the click's own load in flight.
    expect(threadRequests().slice(before)).toEqual(['p1']);
  });

  it('a space deep link after failures, then the routed expand, fetches the list once', async () => {
    server.threadStatus = { p1: 500 };
    const el = await mount();
    el.expandSpace('p1');
    await flush();
    el.handleSpaceHeaderClick(el.spaces[1]); // collapse
    await el.updateComplete;
    server.threadStatus = {};
    const before = threadRequests().length;

    const threads = el.threadsFor('p1');
    el.expandSpace('p1');
    await threads;
    await flush();

    expect(threadRequests().slice(before)).toEqual(['p1']);
  });
});

describe('space rail — lazy paths', () => {
  it('creating a thread in a never-loaded space loads its list instead of publishing one thread', async () => {
    const el = await mount();
    const base = apiFetchMock.getMockImplementation()!;
    apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
      if (path.endsWith('/threads') && init?.method === 'POST') {
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'p2-new', name: 'new' }), { status: 200 })
        );
      }
      return base(path, init);
    });
    el.creatingThread = 'p2';
    el.newThreadName = 'new';

    await el.submitCreateThread('p2');
    await flush();

    expect(threadRequests().filter((p) => p === 'p2').length).toBeGreaterThanOrEqual(1);
    // The space's real list, not a list holding only the new thread.
    expect(el.threadsBySpace.get('p2').map((t: ChatSpaceThread) => t.id)).toEqual(['p2-general']);
  });

  it('ensureThreads waits for a load that superseded the one it joined', async () => {
    server.holdThreads = true;
    const el = await mount();
    let settled = false;
    const ensured = el.ensureThreads('p0').then(() => (settled = true));
    await flush();
    // An explicit newer load (as thread creation starts) supersedes it.
    void el.loadThreads('p0');
    server.held[0].release(); // aborted already; its answer is ignored
    await flush();
    expect(settled).toBe(false);

    server.held[1].release();
    await ensured;
    expect(el.threadsBySpace.has('p0')).toBe(true);
  });

  it('opening a space from its header gives up if it was collapsed while loading', async () => {
    server.holdThreads = true;
    const el = await mount();
    const selects: string[] = [];
    el.addEventListener('thread-select', (e: Event) =>
      selects.push((e as CustomEvent).detail.conversationKey)
    );

    el.handleCollapsedSpaceClick(el.spaces[2]);
    await el.updateComplete;
    el.handleSpaceHeaderClick(el.spaces[2]); // collapse again
    await el.updateComplete;
    server.held[0].release();
    await flush();

    expect(selects).toEqual([]);
  });

  it('a newly selected space starts loading in the same render, while still collapsed', async () => {
    const el = await mount();
    // hostUpdated runs after render and before updated(), where the routed
    // space is expanded: the load must already be under way by then.
    const seen: boolean[] = [];
    el.addController({
      hostUpdated: () => seen.push(el.collapsedSpaces.has('p3') && el.loadingThreads.has('p3')),
    });

    el.selectedKey = 'p3-general';
    el.selectedProjectId = 'p3';
    await el.updateComplete;

    expect(seen[0]).toBe(true);
    await flush();
    expect(threadRequests()).toEqual(['p3']);
  });

  it('the trailing reload answers the newest of the calls it absorbed', async () => {
    const el = await mount();
    let releaseSpaces: () => void = () => {};
    const base = apiFetchMock.getMockImplementation()!;
    apiFetchMock.mockImplementation(async (path: string, init?: RequestInit) => {
      if (path === '/api/v1/chat/spaces' && spacesRequests() === 1) {
        await new Promise<void>((r) => (releaseSpaces = r));
      }
      return base(path, init);
    });
    apiFetchMock.mockClear();

    const running = el.reload(); // pass 1, held
    await flush();
    const t1 = performance.now();
    await new Promise((r) => setTimeout(r, 2));
    void el.reload({ startedAfter: t1 });
    // Another owner fetches between the two absorbed events...
    void chatSpacesLoad.load();
    await new Promise((r) => setTimeout(r, 2));
    const t2 = performance.now();
    void el.reload({ startedAfter: t2 });
    releaseSpaces();
    await running;
    await flush();

    // ...which answers the first but not the second: the trailing pass
    // must fetch again rather than reuse it.
    expect(spacesRequests()).toBe(3);
  });
});

describe('space rail — queued read state branches', () => {
  /** Lets the clock move between requests and changes. */
  const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 3));

  it('list older, rollup newer: clears the row without touching the badge again', async () => {
    server.spaces = [space(0), { ...space(1), unreadCount: 2 }];
    server.threadUnread = true;
    server.holdThreads = true;
    const el = await mount({ selectedKey: 'p1-general', selectedProjectId: 'p1' });
    await flush();
    await tick();
    el.markThreadRead('p1-general');
    await tick();
    server.spaces = [space(0), { ...space(1), unreadCount: 1 }]; // server rolled the read in
    await el.reload();
    await flush();
    server.held.shift()!.release();
    await flush();
    await el.updateComplete;
    expect(el.threadsBySpace.get('p1')[0].hasUnread).toBe(false);
    expect(el.spaces.find((s: ChatSpace) => s.projectId === 'p1').unreadCount).toBe(1);
  });

  it('list newer, rollup older: refreshes the rollup once', async () => {
    server.spaces = [space(0), { ...space(1), unreadCount: 1 }];
    server.holdThreads = true;
    const el = await mount();
    el.expandSpace('p0');
    await flush();
    await tick();
    el.markThreadRead('p1-general'); // queued: p0 is loading
    await tick();
    const before = spacesRequests();
    el.expandSpace('p1');
    await flush();
    server.held.find((h) => h.projectId === 'p1')!.release();
    await flush();
    await el.updateComplete;
    expect(spacesRequests()).toBe(before + 1);
  });

  it('a queued unread applies as unread', async () => {
    server.holdThreads = true;
    const el = await mount({ selectedKey: 'p1-general', selectedProjectId: 'p1' });
    await flush();
    await tick();
    el.markThreadUnread('p1-general');
    await tick();
    server.held.shift()!.release();
    await flush();
    await el.updateComplete;
    expect(el.threadsBySpace.get('p1')[0].hasUnread).toBe(true);
    expect(el.spaces.find((s: ChatSpace) => s.projectId === 'p1').unreadCount).toBe(1);
  });
});
