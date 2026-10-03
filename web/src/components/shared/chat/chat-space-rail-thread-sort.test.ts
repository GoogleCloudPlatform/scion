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
 * Tests for applying the rail's Alphabetical/Recent sort choice to the
 * threads inside a space, not just to the spaces themselves, including
 * threads inside a thread group.
 *
 * Also covers the invariant that a drag or nudge moves only the thread it
 * targets: every other thread, every other group in that space, and every
 * other space, must keep following the active alpha/activity sort (or their
 * own explicit arrangement) instead of freezing into a stale or hard-coded
 * order the instant anything elsewhere in the rail switches to custom.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import type { ChatSpace, ChatSpaceThread } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

// unreadCount > 0 so this space survives the space-level "Unread" filter in
// the tests that turn it on — those tests are about the thread list *inside*
// an already-shown space, same convention as chat-space-rail-unread-filter.test.ts.
const SPACE: ChatSpace = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 1,
  hasUnreadMention: false,
};

const SPACE_Q: ChatSpace = {
  projectId: 'proj-2',
  projectName: 'Chat Test Q',
  projectSlug: 'chat-test-q',
  unreadCount: 1,
  hasUnreadMention: false,
};

function thread(overrides: Partial<ChatSpaceThread> = {}): ChatSpaceThread {
  return {
    id: 'topic-1',
    name: 'thread-one',
    isGeneral: false,
    pinned: false,
    muted: false,
    hasUnread: false,
    hasUnreadMention: false,
    ...overrides,
  };
}

/** Mount a rail with one space holding the given threads. */
async function mount(threads: ChatSpaceThread[]): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await new Promise((resolve) => setTimeout(resolve, 0));
  el.spaces = [SPACE];
  el.threadsBySpace = new Map([[SPACE.projectId, threads]]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  await el.updateComplete;
  return el;
}

/** Mount a rail with two spaces, each holding their own threads. */
async function mountTwoSpaces(
  threadsP: ChatSpaceThread[],
  threadsQ: ChatSpaceThread[]
): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await new Promise((resolve) => setTimeout(resolve, 0));
  el.spaces = [SPACE, SPACE_Q];
  el.threadsBySpace = new Map([
    [SPACE.projectId, threadsP],
    [SPACE_Q.projectId, threadsQ],
  ]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  await el.updateComplete;
  return el;
}

function threadNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-item .thread-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function groupNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-group-header .group-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

/** The index a name appears at in the rendered thread list, for asserting
 * relative order between two names without depending on how spaces or
 * groups happen to interleave elsewhere in the same rail. */
function indexOfThread(el: any, name: string): number {
  return threadNames(el).indexOf(name);
}

async function setThreadSortMode(el: any, mode: 'alpha' | 'activity'): Promise<void> {
  el.prefs = { ...el.prefs, threadSortMode: mode };
  await el.updateComplete;
}

/**
 * Make `/api/v1/chat/user-prefs` echo PUT bodies back, the way the real
 * server does — `savePrefs` reconciles its optimistic local update with
 * whatever the response says was stored, so a mock that always answers `{}`
 * would silently wipe back to defaults on every save.
 */
function serveUserPrefsEcho(): void {
  apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
    if (path === '/api/v1/chat/user-prefs' && init?.method === 'PUT') {
      return Promise.resolve(new Response(String(init.body), { status: 200 }));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('space rail — thread sort control selection', () => {
  function selectSort(el: any, value: string): void {
    const item = document.createElement('div');
    item.setAttribute('value', value);
    el.handleSortSelect(new CustomEvent('sl-select', { detail: { item } }));
  }

  it('selecting Alphabetical from the one sort control also switches thread sort', async () => {
    const el = await mount([]);

    selectSort(el, 'alpha');
    await Promise.resolve();

    expect(el.prefs.spaceSortMode).toBe('alpha');
    expect(el.prefs.threadSortMode).toBe('alpha');
  });

  it('selecting Recent activity also switches thread sort back to activity', async () => {
    const el = await mount([]);
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };

    selectSort(el, 'activity');
    await Promise.resolve();

    expect(el.prefs.spaceSortMode).toBe('activity');
    expect(el.prefs.threadSortMode).toBe('activity');
  });

  it('leaves thread sort mode alone when Custom is picked (that only freezes space order)', async () => {
    const el = await mount([]);
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha', spaceOrder: [] };

    selectSort(el, 'custom');
    await Promise.resolve();

    expect(el.prefs.spaceSortMode).toBe('custom');
    expect(el.prefs.threadSortMode).toBe('alpha');
  });
});

describe('space rail — alphabetical thread sort within a space', () => {
  it('sorts ungrouped threads case-insensitively, locale-aware', async () => {
    const el = await mount([
      thread({ id: 't-b', name: 'banana' }),
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-c', name: 'cherry' }),
    ]);

    await setThreadSortMode(el, 'alpha');

    expect(threadNames(el)).toEqual(['Apple', 'banana', 'cherry']);
  });

  it('folds case to break ties, independent of input order', async () => {
    // "Apple" and "apple" collate identically under `sensitivity: 'base'` —
    // only the id tiebreak can separate them, so this is the one shape that
    // actually detects a missing `sensitivity: 'base'` (plain `localeCompare`
    // would instead order them by case — ICU collation deterministically
    // puts the lowercase form first — rather than leaving the tiebreak to
    // decide).
    const el = await mount([
      thread({ id: 't-2', name: 'apple' }),
      thread({ id: 't-1', name: 'Apple' }),
    ]);

    await setThreadSortMode(el, 'alpha');
    const firstOrder = threadNames(el);

    el.threadsBySpace = new Map([
      [
        SPACE.projectId,
        [thread({ id: 't-1', name: 'Apple' }), thread({ id: 't-2', name: 'apple' })],
      ],
    ]);
    await el.updateComplete;

    // The id tiebreak ('t-1' < 't-2') must win regardless of input order.
    expect(firstOrder).toEqual(['Apple', 'apple']);
    expect(threadNames(el)).toEqual(firstOrder);
  });

  it('keeps pinned threads first, sorted alphabetically within that bucket', async () => {
    const el = await mount([
      thread({ id: 't-z', name: 'zebra' }),
      thread({ id: 't-p2', name: 'rhino', pinned: true }),
      thread({ id: 't-p1', name: 'alpaca', pinned: true }),
      thread({ id: 't-a', name: 'anchovy' }),
    ]);

    await setThreadSortMode(el, 'alpha');

    // Pinned-first is preserved; alpha applies inside each bucket.
    expect(threadNames(el)).toEqual(['alpaca', 'rhino', 'anchovy', 'zebra']);
  });

  it('keeps #general first regardless of alphabetical order', async () => {
    const el = await mount([
      thread({ id: 't-general', name: 'general', isGeneral: true }),
      thread({ id: 't-a', name: 'announcements' }),
    ]);

    await setThreadSortMode(el, 'alpha');

    expect(threadNames(el)).toEqual(['general', 'announcements']);
  });
});

describe('space rail — recent-activity thread sort within a space', () => {
  it('orders threads newest-first by lastActivityAt', async () => {
    const el = await mount([
      thread({ id: 't-old', name: 'old-thread', lastActivityAt: '2026-01-01T00:00:00Z' }),
      thread({ id: 't-new', name: 'new-thread', lastActivityAt: '2026-06-01T00:00:00Z' }),
      thread({ id: 't-mid', name: 'mid-thread', lastActivityAt: '2026-03-01T00:00:00Z' }),
    ]);

    await setThreadSortMode(el, 'activity');

    expect(threadNames(el)).toEqual(['new-thread', 'mid-thread', 'old-thread']);
  });

  it('re-sorts live when a thread gets new activity, without losing the selection', async () => {
    const el = await mount([
      thread({ id: 't-1', name: 'thread-one', lastActivityAt: '2026-01-01T00:00:00Z' }),
      thread({ id: 't-2', name: 'thread-two', lastActivityAt: '2026-02-01T00:00:00Z' }),
    ]);
    el.selectedKey = 't-1';
    await setThreadSortMode(el, 'activity');
    expect(threadNames(el)).toEqual(['thread-two', 'thread-one']);

    // A new message bumps thread-one's activity past thread-two's — matches
    // how the rail already re-sorts spaces when activity changes.
    el.threadsBySpace = new Map([
      [
        SPACE.projectId,
        [
          thread({ id: 't-1', name: 'thread-one', lastActivityAt: '2026-03-01T00:00:00Z' }),
          thread({ id: 't-2', name: 'thread-two', lastActivityAt: '2026-02-01T00:00:00Z' }),
        ],
      ],
    ]);
    await el.updateComplete;

    expect(threadNames(el)).toEqual(['thread-one', 'thread-two']);
    expect(el.selectedKey).toBe('t-1');
  });

  it('breaks activity ties deterministically instead of depending on input order', async () => {
    const tie = '2026-01-01T00:00:00Z';
    const el = await mount([
      thread({ id: 't-b', name: 'b-thread', lastActivityAt: tie }),
      thread({ id: 't-a', name: 'a-thread', lastActivityAt: tie }),
    ]);

    await setThreadSortMode(el, 'activity');
    const firstOrder = threadNames(el);

    // Same data, reversed input order — the tiebreak (by id) must produce
    // the same result either way, not just "whatever came in first".
    el.threadsBySpace = new Map([
      [
        SPACE.projectId,
        [
          thread({ id: 't-a', name: 'a-thread', lastActivityAt: tie }),
          thread({ id: 't-b', name: 'b-thread', lastActivityAt: tie }),
        ],
      ],
    ]);
    await el.updateComplete;

    expect(threadNames(el)).toEqual(firstOrder);
  });
});

describe('space rail — thread sort applies within a thread group', () => {
  function withGroup(el: any, threadIds: string[]): void {
    el.prefs = {
      ...el.prefs,
      threadGroups: { [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds }] },
    };
  }

  it("sorts a group's members alphabetically, independent of membership order", async () => {
    const el = await mount([
      thread({ id: 't-c', name: 'cherry' }),
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
    ]);
    withGroup(el, ['t-c', 'a-does-not-exist', 't-a', 't-b']);

    await setThreadSortMode(el, 'alpha');

    expect(groupNames(el)).toEqual(['My Group']);
    expect(threadNames(el)).toEqual(['Apple', 'banana', 'cherry']);
  });

  it("sorts a group's members by recent activity, newest first", async () => {
    const el = await mount([
      thread({ id: 't-old', name: 'old-thread', lastActivityAt: '2026-01-01T00:00:00Z' }),
      thread({ id: 't-new', name: 'new-thread', lastActivityAt: '2026-06-01T00:00:00Z' }),
    ]);
    withGroup(el, ['t-old', 't-new']);

    await setThreadSortMode(el, 'activity');

    expect(threadNames(el)).toEqual(['new-thread', 'old-thread']);
  });

  it('does not reorder groups relative to ungrouped threads beyond the existing top-level sort', async () => {
    const el = await mount([
      thread({ id: 't-z', name: 'zzz-ungrouped' }),
      thread({ id: 't-g1', name: 'group-member-b' }),
      thread({ id: 't-g2', name: 'group-member-a' }),
    ]);
    withGroup(el, ['t-g1', 't-g2']);
    // The group's own name sorts between the ungrouped thread's name and
    // nothing else, under alpha — this just confirms group-vs-ungrouped
    // ordering at the top level is untouched by the within-group fix.
    el.prefs.threadGroups[SPACE.projectId][0].name = 'Alpha Group';

    await setThreadSortMode(el, 'alpha');

    expect(groupNames(el)).toEqual(['Alpha Group']);
    // Group (name "Alpha Group") sorts before the ungrouped "zzz-ungrouped"
    // thread, and its own members are alphabetized.
    expect(threadNames(el)).toEqual(['group-member-a', 'group-member-b', 'zzz-ungrouped']);
  });

  it('leaves group membership order untouched once the space has an explicit snapshot', async () => {
    const el = await mount([
      thread({ id: 't-c', name: 'cherry' }),
      thread({ id: 't-a', name: 'Apple' }),
    ]);
    withGroup(el, ['t-c', 't-a']);
    // A non-empty threadOrder entry for this project alone is the explicit
    // signal (see `hasExplicitOrder`) — threadSortMode (left at its default
    // here) plays no part in it.
    el.prefs = {
      ...el.prefs,
      threadOrder: { [SPACE.projectId]: ['g-1'] },
    };
    await el.updateComplete;

    expect(threadNames(el)).toEqual(['cherry', 'Apple']);
  });

  it('keeps pinned ungrouped threads first even when the space also has a group', async () => {
    const el = await mount([
      thread({ id: 't-z', name: 'zebra' }),
      thread({ id: 't-p', name: 'rhino', pinned: true }),
      thread({ id: 't-g', name: 'alpha-group-member' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [{ id: 'g-1', name: 'aaa-group', threadIds: ['t-g'] }],
      },
    };
    await setThreadSortMode(el, 'alpha');

    // "rhino" is pinned, so it leads even though the group's name
    // ("aaa-group") would otherwise sort first alphabetically.
    expect(threadNames(el)).toEqual(['rhino', 'alpha-group-member', 'zebra']);
    expect(groupNames(el)).toEqual(['aaa-group']);
  });
});

describe('space rail — thread sort interacts correctly with the unread filter', () => {
  it('sorts the filtered (unread-only) set, not the full membership', async () => {
    const el = await mount([
      thread({ id: 't-z', name: 'zebra', hasUnread: true }),
      thread({ id: 't-r', name: 'read-thread', hasUnread: false }),
      thread({ id: 't-a', name: 'anchovy', hasUnread: true }),
    ]);
    await setThreadSortMode(el, 'alpha');
    el.spaceFilter = 'unread';
    await el.updateComplete;

    expect(threadNames(el)).toEqual(['anchovy', 'zebra']);
  });

  it('keeps the selected thread visible and in sorted position under the filter', async () => {
    const el = await mount([
      thread({ id: 't-open', name: 'middle-thread', hasUnread: false }),
      thread({ id: 't-unread-a', name: 'aaa-unread', hasUnread: true }),
      thread({ id: 't-unread-z', name: 'zzz-unread', hasUnread: true }),
    ]);
    el.selectedKey = 't-open';
    await setThreadSortMode(el, 'alpha');
    el.spaceFilter = 'unread';
    await el.updateComplete;

    // The open (read) thread stays, slotted into its alphabetical position
    // alongside the unread ones.
    expect(threadNames(el)).toEqual(['aaa-unread', 'middle-thread', 'zzz-unread']);
  });

  it('sorts within a group under the unread filter, hiding fully-read groups', async () => {
    const el = await mount([
      thread({ id: 't-g-z', name: 'group-zebra', hasUnread: true }),
      thread({ id: 't-g-a', name: 'group-anchovy', hasUnread: true }),
      thread({ id: 't-g-read', name: 'group-read', hasUnread: false }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-g-z', 't-g-a'] }],
      },
    };
    await setThreadSortMode(el, 'alpha');
    el.spaceFilter = 'unread';
    await el.updateComplete;

    expect(groupNames(el)).toEqual(['My Group']);
    expect(threadNames(el)).toEqual(['group-anchovy', 'group-zebra']);
  });
});

describe('space rail — a nudge or drag moves only the thread that moved', () => {
  it('moving a grouped thread gives its space an explicit order so the nudge sticks', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadSortMode: 'alpha',
      threadGroups: {
        // Raw storage order ('t-b' then 't-a') is reversed from the alpha
        // display order ('Apple, banana') — using the raw array instead of
        // the displayed order (the mutation this guards against) would do
        // index math against the wrong sequence entirely.
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-b', 't-a'] }],
      },
    };
    await el.updateComplete;
    expect(threadNames(el)).toEqual(['Apple', 'banana']);

    // Displayed order is Apple, banana — move banana (displayed-last) up one slot.
    await el.moveThread('t-b', SPACE.projectId, -1);
    // The nudge gives this space its own threadOrder snapshot without
    // touching threadSortMode (see `hasExplicitOrder`).
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toEqual(['t-b', 't-a']);
    expect(threadNames(el)).toEqual(['banana', 'Apple']);

    // Move it back down — a second nudge must act on the now-current
    // (explicit) order, not silently re-derive from scratch again.
    await el.moveThread('t-b', SPACE.projectId, 1);
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toEqual(['t-a', 't-b']);
    expect(threadNames(el)).toEqual(['Apple', 'banana']);
  });

  it('isThreadAtEdge reports the displayed edge, not the raw-array edge', async () => {
    const el = await mount([
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadSortMode: 'alpha',
      threadGroups: {
        // Raw order 't-b','t-a' is reversed from the displayed alpha order.
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-b', 't-a'] }],
      },
    };
    await el.updateComplete;
    expect(threadNames(el)).toEqual(['Apple', 'banana']);

    // Displayed-first is Apple (t-a), which is raw-last.
    expect(el.isThreadAtEdge('t-a', SPACE.projectId, 'first')).toBe(true);
    expect(el.isThreadAtEdge('t-a', SPACE.projectId, 'last')).toBe(false);
    expect(el.isThreadAtEdge('t-b', SPACE.projectId, 'last')).toBe(true);
    expect(el.isThreadAtEdge('t-b', SPACE.projectId, 'first')).toBe(false);
  });

  it('nudging a thread in one space leaves every other space alone', async () => {
    serveUserPrefsEcho();
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      // Raw order is reversed from alpha so a passthrough (no real sort)
      // can't accidentally match the correct, sorted output, and activity
      // is reversed from alpha too — newer "zebra-q" sorts first under
      // Recent — so falling back to activity instead of the active alpha
      // choice would also be visible here.
      [
        thread({ id: 'q-zebra', name: 'zebra-q', lastActivityAt: '2026-02-01T00:00:00Z' }),
        thread({ id: 'q-apple', name: 'apple-q', lastActivityAt: '2026-01-01T00:00:00Z' }),
      ]
    );
    // Alphabetical, picked from the rail's one sort control, sets both
    // modes together (see the "sort control selection" tests above).
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await el.updateComplete;
    expect(indexOfThread(el, 'apple-q')).toBeLessThan(indexOfThread(el, 'zebra-q'));

    // Nudge a thread in space P.
    await el.moveThread('p-b', SPACE.projectId, -1);
    expect(el.prefs.threadSortMode).toBe('alpha');

    // Space Q has no snapshot of its own — it must still show alpha order,
    // not fall back to a hard-coded activity order.
    expect(indexOfThread(el, 'apple-q')).toBeLessThan(indexOfThread(el, 'zebra-q'));
  });

  it('nudging a thread inside one group leaves the space’s ungrouped threads and other groups alone', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 't-alpha', name: 'alpha' }),
      thread({ id: 't-zulu', name: 'zulu' }),
      thread({ id: 'g1-one', name: 'g-one' }),
      thread({ id: 'g1-two', name: 'g-two' }),
      thread({ id: 'g2-x', name: 'g2-x' }),
      thread({ id: 'g2-y', name: 'g2-y' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadSortMode: 'alpha',
      threadGroups: {
        [SPACE.projectId]: [
          { id: 'g-1', name: 'beta-group', threadIds: ['g1-one', 'g1-two'] },
          // Raw order reversed from alpha, so freezing this untouched group
          // to its displayed order (rather than leaving its raw array as-is)
          // is actually exercised.
          { id: 'g-2', name: 'delta-group', threadIds: ['g2-y', 'g2-x'] },
        ],
      },
    };
    await el.updateComplete;
    // Alpha display: alpha, beta-group[g-one,g-two], delta-group[g2-x,g2-y], zulu
    expect(threadNames(el)).toEqual(['alpha', 'g-one', 'g-two', 'g2-x', 'g2-y', 'zulu']);
    expect(groupNames(el)).toEqual(['beta-group', 'delta-group']);

    // Nudge g-two up within its own group (g-1).
    await el.moveThread('g1-two', SPACE.projectId, -1);

    expect(el.prefs.threadSortMode).toBe('alpha');
    // Only g-1's members swapped; the ungrouped threads, the other group's
    // members, and the relative top-level order are all untouched.
    expect(threadNames(el)).toEqual(['alpha', 'g-two', 'g-one', 'g2-x', 'g2-y', 'zulu']);
    expect(groupNames(el)).toEqual(['beta-group', 'delta-group']);
  });

  it('does not prune a group member that no longer resolves to a known thread', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadSortMode: 'alpha',
      threadGroups: {
        [SPACE.projectId]: [
          { id: 'g-1', name: 'My Group', threadIds: ['t-a', 't-b', 'stale-deleted-id'] },
        ],
      },
    };
    await el.updateComplete;

    await el.moveThread('t-b', SPACE.projectId, -1);

    // The stale id survives the nudge instead of being silently dropped.
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toContain('stale-deleted-id');
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toEqual([
      't-b',
      't-a',
      'stale-deleted-id',
    ]);
  });
});

describe('space rail — Alphabetical/Recent actually discards custom thread arrangements', () => {
  function selectSort(el: any, value: string): void {
    const item = document.createElement('div');
    item.setAttribute('value', value);
    el.handleSortSelect(new CustomEvent('sl-select', { detail: { item } }));
  }

  async function drop(
    el: any,
    sourceId: string,
    targetId: string,
    projectId: string
  ): Promise<void> {
    el.draggingThreadId = sourceId;
    await el.handleThreadDrop({ preventDefault: vi.fn() } as any, targetId, projectId);
  }

  it('a later drag elsewhere cannot bring back a snapshot that Alphabetical already discarded', async () => {
    serveUserPrefsEcho();
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      [
        thread({ id: 'q-zebra', name: 'zebra' }),
        thread({ id: 'q-apple', name: 'apple' }),
        thread({ id: 'q-mango', name: 'mango' }),
      ]
    );
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['zebra', 'apple', 'mango'].includes(n));

    // Drag "zebra" onto "apple" in Q — Q gets its own explicit snapshot.
    await drop(el, 'q-zebra', 'q-apple', SPACE_Q.projectId);
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['zebra', 'apple', 'mango']);

    // Re-pick Alphabetical from the rail's one sort control.
    selectSort(el, 'alpha');
    await Promise.resolve();
    await el.updateComplete;
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Nudge a thread in the other space — this must not revive Q's
    // already-discarded snapshot.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);
  });
});

describe('space rail — creating or deleting a group does not fabricate an explicit order', () => {
  it('creating a group in a space with no snapshot does not pollute its order', async () => {
    serveUserPrefsEcho();
    // Raw (storage) order deliberately differs from the alpha display order,
    // so a stray explicit-order entry that silently starts controlling
    // display shows up as the *raw* order instead of staying alphabetical —
    // a mutation that writes the entry but renders by coincidence in alpha
    // order either way would otherwise hide the bug.
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      [
        thread({ id: 'q-zebra', name: 'zebra' }),
        thread({ id: 'q-apple', name: 'apple' }),
        thread({ id: 'q-mango', name: 'mango' }),
      ]
    );
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['apple', 'mango', 'zebra'].includes(n));
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Create a group in Q — Q still has no explicit snapshot of its own.
    await el.createGroup(SPACE_Q.projectId, 'New Group');
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Nudge a thread in the other space — Q must still be unaffected.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);
  });

  it('deleting a group in a space with no snapshot does not pollute its order', async () => {
    serveUserPrefsEcho();
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      [
        thread({ id: 'q-apple', name: 'apple' }),
        thread({ id: 'q-mango', name: 'mango' }),
        thread({ id: 'q-zebra', name: 'zebra' }),
      ]
    );
    el.prefs = {
      ...el.prefs,
      spaceSortMode: 'alpha',
      threadSortMode: 'alpha',
      threadGroups: {
        [SPACE_Q.projectId]: [{ id: 'g-1', name: 'Soon Gone', threadIds: ['q-mango'] }],
      },
    };
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['apple', 'mango', 'zebra'].includes(n));
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Delete the group in Q — Q still has no explicit snapshot of its own.
    await el.deleteGroup('g-1', SPACE_Q.projectId);
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Nudge a thread in the other space — Q must still be unaffected.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);
  });
});

describe('space rail — drag-and-drop lands on the displayed position', () => {
  function withGroup(el: any, threadIds: string[], groupId = 'g-1', name = 'My Group'): void {
    el.prefs = {
      ...el.prefs,
      threadGroups: { [SPACE.projectId]: [{ id: groupId, name, threadIds }] },
    };
  }

  async function drop(el: any, sourceId: string, targetId: string): Promise<void> {
    el.draggingThreadId = sourceId;
    await el.handleThreadDrop({ preventDefault: vi.fn() } as any, targetId, SPACE.projectId);
  }

  it('dropping within the same group reorders only the dragged and target threads', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 'c', name: 'cc' }),
      thread({ id: 'a', name: 'aa' }),
      thread({ id: 'b', name: 'bb' }),
    ]);
    el.prefs = { ...el.prefs, threadSortMode: 'alpha' };
    withGroup(el, ['c', 'a', 'b']);
    await el.updateComplete;
    // Displayed order: aa, bb, cc (raw storage order is c, a, b).
    expect(threadNames(el)).toEqual(['aa', 'bb', 'cc']);

    // Drag "aa" onto "cc".
    await drop(el, 'a', 'c');
    await el.updateComplete;

    // "aa" lands where it was dropped; "bb" is untouched.
    expect(threadNames(el)).toEqual(['bb', 'aa', 'cc']);
  });

  it('dropping into a different group inserts at the displayed position', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 'src', name: 'source-thread' }),
      thread({ id: 'c', name: 'cc' }),
      thread({ id: 'a', name: 'aa' }),
      thread({ id: 'b', name: 'bb' }),
    ]);
    el.prefs = {
      ...el.prefs,
      threadSortMode: 'alpha',
      threadGroups: {
        [SPACE.projectId]: [
          { id: 'g-src', name: 'Source Group', threadIds: ['src'] },
          // Raw storage order (c, a, b) differs from the displayed order
          // (aa, bb, cc) under alpha.
          { id: 'g-dst', name: 'Dest Group', threadIds: ['c', 'a', 'b'] },
        ],
      },
    };
    await el.updateComplete;

    // Move "source-thread" into the destination group, dropped onto "cc".
    await drop(el, 'src', 'c');
    await el.updateComplete;

    const group = el.prefs.threadGroups[SPACE.projectId].find((g: any) => g.id === 'g-dst');
    // Inserted immediately before "c" in the group's own storage order.
    expect(group.threadIds).toEqual(['a', 'b', 'src', 'c']);
  });

  it('dragging a thread out of a sorted group onto an ungrouped thread leaves the rest of the group alone', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 'c', name: 'cc' }),
      thread({ id: 'a', name: 'aa' }),
      thread({ id: 'b', name: 'bb' }),
      thread({ id: 'x', name: 'xx' }),
    ]);
    el.prefs = { ...el.prefs, threadSortMode: 'alpha' };
    withGroup(el, ['c', 'a', 'b']);
    await el.updateComplete;
    // Displayed order: aa, bb, cc (raw storage order is c, a, b), then the
    // ungrouped xx.
    expect(threadNames(el)).toEqual(['aa', 'bb', 'cc', 'xx']);

    // Drag "bb" out of the group, onto the ungrouped "xx".
    await drop(el, 'b', 'x');
    await el.updateComplete;

    const group = el.prefs.threadGroups[SPACE.projectId].find((g: any) => g.id === 'g-1');
    // "bb" left; "aa" and "cc", never touched, keep their relative order
    // instead of swapping.
    expect(group.threadIds).toEqual(['a', 'c']);
    expect(threadNames(el)).toEqual(['aa', 'cc', 'bb', 'xx']);
  });

  it('a top-level nudge or drop freezes every group to its displayed order, not its raw storage order', async () => {
    serveUserPrefsEcho();
    const el = await mount([
      thread({ id: 'c', name: 'cc' }),
      thread({ id: 'a', name: 'aa' }),
      thread({ id: 'b', name: 'bb' }),
      thread({ id: 'x', name: 'xx' }),
      thread({ id: 'y', name: 'yy' }),
    ]);
    el.prefs = { ...el.prefs, threadSortMode: 'alpha' };
    // Group name sorts after both ungrouped threads, and its raw storage
    // order is reversed from its displayed (alpha) order.
    withGroup(el, ['c', 'a', 'b'], 'g-1', 'Zzz Group');
    await el.updateComplete;
    expect(threadNames(el)).toEqual(['xx', 'yy', 'aa', 'bb', 'cc']);

    // Nudge one ungrouped thread — the space's first explicit top-level
    // snapshot.
    await el.moveThread('y', SPACE.projectId, -1);
    await el.updateComplete;
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toEqual(['a', 'b', 'c']);
    expect(threadNames(el)).toEqual(['yy', 'xx', 'aa', 'bb', 'cc']);

    // Drop one ungrouped thread onto the other — the group must still read
    // back its displayed order, not revert to the original raw array.
    await drop(el, 'x', 'y');
    await el.updateComplete;
    expect(el.prefs.threadGroups[SPACE.projectId][0].threadIds).toEqual(['a', 'b', 'c']);
    expect(threadNames(el)).toEqual(['xx', 'yy', 'aa', 'bb', 'cc']);
  });
});

describe('space rail — a legacy threadOrder entry outside custom loads as an explicit order', () => {
  it('a pre-existing group-create entry loads as Q’s order, survives a nudge elsewhere, and clears on a sort pick', async () => {
    // Simulates a user who created a group in Q on an older build, before
    // createGroup/deleteGroup were guarded — Q has a stray threadOrder entry
    // (just the group id) alongside a plain, non-'custom' threadSortMode. The
    // wire shape can't tell that apart from an order this build wrote, so it
    // loads as Q's explicit order rather than being dropped.
    apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
      if (path === '/api/v1/chat/user-prefs' && init?.method === 'PUT') {
        return Promise.resolve(new Response(String(init.body), { status: 200 }));
      }
      if (path === '/api/v1/chat/user-prefs') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              threadSortMode: 'activity',
              threadOrder: JSON.stringify({ [SPACE_Q.projectId]: ['g-old'] }),
              threadGroups: JSON.stringify({
                [SPACE_Q.projectId]: [{ id: 'g-old', name: 'Old Group', threadIds: [] }],
              }),
            }),
            { status: 200 }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const el = document.createElement('scion-chat-space-rail') as any;
    document.body.appendChild(el);
    await new Promise((resolve) => setTimeout(resolve, 0));
    el.spaces = [SPACE, SPACE_Q];
    el.threadsBySpace = new Map([
      [
        SPACE.projectId,
        [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      ],
      [
        SPACE_Q.projectId,
        // Raw (storage) order deliberately differs from the Recent order, so
        // the group-first/raw-order bug (if not fixed) is visible.
        [
          thread({ id: 'q-zebra', name: 'zebra', lastActivityAt: '2026-03-01T00:00:00Z' }),
          thread({ id: 'q-apple', name: 'apple', lastActivityAt: '2026-01-01T00:00:00Z' }),
          thread({ id: 'q-mango', name: 'mango', lastActivityAt: '2026-02-01T00:00:00Z' }),
        ],
      ],
    ]);
    el.collapsedSpaces = new Set<string>();
    el.loading = false;

    await el.loadPrefs();
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['zebra', 'apple', 'mango'].includes(n));
    // The stale entry only names the group, so the ungrouped threads follow
    // raw server order (zebra, apple, mango) rather than Recent (which would
    // be zebra, mango, apple).
    expect(qNames()).toEqual(['zebra', 'apple', 'mango']);

    // Nudge a thread in the other space — Q's loaded order must stay put.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(qNames()).toEqual(['zebra', 'apple', 'mango']);

    // Picking a sort clears it for good.
    const item = document.createElement('div');
    item.setAttribute('value', 'activity');
    el.handleSortSelect(new CustomEvent('sl-select', { detail: { item } }));
    await Promise.resolve();
    await el.updateComplete;
    expect(qNames()).toEqual(['zebra', 'mango', 'apple']);
  });
});

describe('space rail — Alphabetical survives a space reorder or Custom, through a later thread nudge', () => {
  function selectSort(el: any, value: string): void {
    const item = document.createElement('div');
    item.setAttribute('value', value);
    el.handleSortSelect(new CustomEvent('sl-select', { detail: { item } }));
  }

  it('dragging a space to reorder it leaves every other space on alpha thread sort', async () => {
    serveUserPrefsEcho();
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      [
        thread({ id: 'q-zebra', name: 'zebra' }),
        thread({ id: 'q-apple', name: 'apple' }),
        thread({ id: 'q-mango', name: 'mango' }),
      ]
    );
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['zebra', 'apple', 'mango'].includes(n));
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Reorder the spaces (not a thread) — switches spaceSortMode to custom,
    // but must leave threadSortMode alone.
    await el.applySpaceOrder([SPACE_Q.projectId, SPACE.projectId]);
    expect(el.prefs.spaceSortMode).toBe('custom');
    expect(el.prefs.threadSortMode).toBe('alpha');

    // Nudge a thread in P — Q has no snapshot of its own and must stay alpha.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);
  });

  it('picking Custom from the sort menu leaves every other space on alpha thread sort', async () => {
    serveUserPrefsEcho();
    const el = await mountTwoSpaces(
      [thread({ id: 'p-a', name: 'alpha-p' }), thread({ id: 'p-b', name: 'beta-p' })],
      [
        thread({ id: 'q-zebra', name: 'zebra' }),
        thread({ id: 'q-apple', name: 'apple' }),
        thread({ id: 'q-mango', name: 'mango' }),
      ]
    );
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await el.updateComplete;

    const qNames = () => threadNames(el).filter((n) => ['zebra', 'apple', 'mango'].includes(n));
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);

    // Pick Custom from the one sort control — space order only.
    selectSort(el, 'custom');
    await Promise.resolve();
    await el.updateComplete;
    expect(el.prefs.spaceSortMode).toBe('custom');
    expect(el.prefs.threadSortMode).toBe('alpha');

    // Nudge a thread in P — Q has no snapshot of its own and must stay alpha.
    await el.moveThread('p-b', SPACE.projectId, -1);
    await el.updateComplete;
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(qNames()).toEqual(['apple', 'mango', 'zebra']);
  });
});

describe('space rail — an id missing from an explicit snapshot sorts after the ones in it', () => {
  it('a thread created after the snapshot stays at the end in the flat (ungrouped) reader', async () => {
    const el = await mount([
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
      thread({ id: 't-new', name: 'new-thread' }),
    ]);
    // t-new postdates this snapshot, so it has no entry.
    el.prefs = { ...el.prefs, threadOrder: { [SPACE.projectId]: ['t-b', 't-a'] } };
    await el.updateComplete;

    expect(threadNames(el)).toEqual(['banana', 'Apple', 'new-thread']);
  });

  it('a thread created after the snapshot stays at the end in the grouped reader', async () => {
    const el = await mount([
      thread({ id: 't-a', name: 'Apple' }),
      thread({ id: 't-b', name: 'banana' }),
      thread({ id: 't-new', name: 'new-thread' }),
    ]);
    el.prefs = {
      ...el.prefs,
      // The space has a group, so the top level renders via
      // sortTopLevelItems/currentTopLevelOrder rather than getSortedThreads —
      // t-new still postdates the snapshot and has no entry there either.
      threadGroups: { [SPACE.projectId]: [{ id: 'g-1', name: 'Group', threadIds: [] }] },
      threadOrder: { [SPACE.projectId]: ['t-b', 't-a', 'g-1'] },
    };
    await el.updateComplete;

    expect(threadNames(el)).toEqual(['banana', 'Apple', 'new-thread']);
  });
});
