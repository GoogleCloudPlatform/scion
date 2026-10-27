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

import { describe, it, expect, vi, afterEach } from 'vitest';

import {
  STARRED_RAIL_PAGE,
  StarredRailController,
  fetchStarredConversations,
  groupStarredRows,
  mergeStarredRows,
  starredRailSort,
  type StarredConversationPage,
  type StarredConversationRow,
  type StarredRailComparators,
} from './chat-starred-rail.js';

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (v: T) => void;
  reject: (e: unknown) => void;
} {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function thread(key: string, name: string, projectId: string, at?: string): StarredConversationRow {
  return {
    conversationKey: key,
    conversationKind: 'thread',
    conversationName: name,
    projectId,
    projectName: `Project ${projectId}`,
    ...(at ? { lastActivityAt: at } : {}),
  };
}

function dm(key: string, name: string, at?: string, projectId?: string): StarredConversationRow {
  return {
    conversationKey: key,
    conversationKind: 'dm',
    conversationName: name,
    ...(at ? { lastActivityAt: at } : {}),
    ...(projectId ? { projectId, projectName: `Project ${projectId}` } : {}),
  };
}

const cmp: StarredRailComparators = {
  byName: (a, b, ai, bi) => {
    const n = a.localeCompare(b, undefined, { sensitivity: 'base' });
    return n !== 0 ? n : ai.localeCompare(bi);
  },
  byActivity: (a, b, ai, bi) => (a !== b ? b - a : ai.localeCompare(bi)),
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('fetchStarredConversations', () => {
  it('asks for the sort, page size and cursor and keeps an omitted cursor absent', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ items: [thread('t1', 'One', 'p1')], nextCursor: 'c2' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);
    const page = await fetchStarredConversations('alpha', 30, 'c1');
    expect(page.items.map((i) => i.conversationKey)).toEqual(['t1']);
    expect(page.nextCursor).toBe('c2');
    const url = String((fetchMock.mock.calls[0] as unknown[])[0]);
    expect(url).toContain('/api/v1/chat/starred-conversations?');
    const q = new URL(url, 'http://x').searchParams;
    expect(q.get('sort')).toBe('alpha');
    expect(q.get('limit')).toBe('30');
    expect(q.get('cursor')).toBe('c1');

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ items: [] }), { status: 200 }));
    const last = await fetchStarredConversations('activity', 30);
    expect(last).toEqual({ items: [] });
    expect(
      new URL(String((fetchMock.mock.calls[1] as unknown[])[0]), 'http://x').searchParams.has(
        'cursor'
      )
    ).toBe(false);
  });

  it('throws the hub error with its status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ error: { message: 'nope' } }), { status: 400 })
        )
      )
    );
    await expect(fetchStarredConversations('activity', 30)).rejects.toMatchObject({ status: 400 });
  });
});

describe('starredRailSort', () => {
  it('maps Recent and Alphabetical to themselves and shows Recent for Custom', () => {
    expect(starredRailSort('activity')).toBe('activity');
    expect(starredRailSort('alpha')).toBe('alpha');
    expect(starredRailSort('custom')).toBe('activity');
    expect(starredRailSort(undefined)).toBe('activity');
  });
});

describe('mergeStarredRows', () => {
  it('merges by key, the newer row winning, with no duplicates', () => {
    const merged = mergeStarredRows(
      [thread('a', 'A', 'p1'), thread('b', 'B', 'p1')],
      [thread('b', 'B renamed', 'p1'), thread('c', 'C', 'p2')]
    );
    expect(merged.map((r) => [r.conversationKey, r.conversationName])).toEqual([
      ['a', 'A'],
      ['b', 'B renamed'],
      ['c', 'C'],
    ]);
  });
});

describe('groupStarredRows', () => {
  const rows = [
    thread('t-b', 'beta', 'p1', '2026-10-22T10:00:00Z'),
    thread('t-a', 'Alpha', 'p1', '2026-10-22T09:00:00Z'),
    thread('t-z', 'zulu', 'p2', '2026-10-22T11:00:00Z'),
    dm('dm:agent:x:user:me', 'Zed agent', '2026-10-22T08:00:00Z', 'p1'),
    dm('dm:user:me:user:y', 'mia', '2026-10-22T12:00:00Z'),
  ];

  it('puts every DM (agent DMs too) in the DMs section and threads under their project', () => {
    const g = groupStarredRows(rows, 'activity', cmp);
    expect(g.dms.map((r) => r.conversationKey)).toEqual([
      'dm:user:me:user:y',
      'dm:agent:x:user:me',
    ]);
    expect(
      g.projects.map((p) => p.projectId),
      'headings by their newest row'
    ).toEqual(['p2', 'p1']);
    expect(g.projects[1].rows.map((r) => r.conversationKey)).toEqual(['t-b', 't-a']);
  });

  it('sorts by name, case-folded, under Alphabetical', () => {
    const g = groupStarredRows(rows, 'alpha', cmp);
    expect(g.dms.map((r) => r.conversationName)).toEqual(['mia', 'Zed agent']);
    expect(g.projects.map((p) => p.projectName)).toEqual(['Project p1', 'Project p2']);
    expect(g.projects[0].rows.map((r) => r.conversationName)).toEqual(['Alpha', 'beta']);
  });

  it('never makes an empty heading', () => {
    const g = groupStarredRows([dm('dm:user:me:user:y', 'mia')], 'activity', cmp);
    expect(g.projects).toEqual([]);
    expect(groupStarredRows([], 'alpha', cmp)).toEqual({ dms: [], projects: [] });
  });
});

function page(keys: string[], nextCursor?: string): StarredConversationPage {
  return { items: keys.map((k) => thread(k, k, 'p1')), ...(nextCursor ? { nextCursor } : {}) };
}

describe('StarredRailController', () => {
  it('loads the first page, then appends Load more by key without duplicates', async () => {
    const fetchPage = vi
      .fn()
      .mockResolvedValueOnce(page(['a', 'b'], 'c1'))
      .mockResolvedValueOnce(page(['b', 'c']));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    expect(c.status).toBe('ready');
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['a', 'b']);
    expect(c.nextCursor).toBe('c1');
    expect(fetchPage).toHaveBeenLastCalledWith(
      'activity',
      STARRED_RAIL_PAGE,
      undefined,
      expect.any(AbortSignal)
    );
    await c.loadMore();
    expect(fetchPage).toHaveBeenLastCalledWith(
      'activity',
      STARRED_RAIL_PAGE,
      'c1',
      expect.any(AbortSignal)
    );
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['a', 'b', 'c']);
    expect(c.nextCursor).toBeUndefined();
  });

  it('keeps loading, error, continuation and exhaustion distinct', async () => {
    const first = deferred<StarredConversationPage>();
    const fetchPage = vi.fn().mockReturnValueOnce(first.promise);
    const c = new StarredRailController(fetchPage);
    const started = c.start('activity');
    expect(c.status).toBe('loading');
    first.reject(new Error('down'));
    await started;
    expect(c.status).toBe('error');
    expect(c.error).toBe('down');

    // An empty page with a cursor (hidden rows spent the hub's scan budget) is a continuation.
    fetchPage.mockResolvedValueOnce(page([], 'more'));
    await c.refresh();
    expect(c.status).toBe('ready');
    expect(c.rows).toEqual([]);
    expect(c.nextCursor).toBe('more');
    expect(fetchPage).toHaveBeenCalledTimes(2);

    // A failed Load more keeps the rows and can be retried.
    fetchPage.mockRejectedValueOnce(new Error('flaky'));
    await c.loadMore();
    expect(c.moreStatus).toBe('error');
    fetchPage.mockResolvedValueOnce(page(['z']));
    await c.loadMore();
    expect(c.moreStatus).toBe('idle');
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['z']);
    expect(c.nextCursor).toBeUndefined();
  });

  it('refresh reloads as many rows as are loaded and replaces them (a lost row leaves)', async () => {
    const fetchPage = vi
      .fn()
      .mockResolvedValueOnce(
        page(
          Array.from({ length: 30 }, (_, i) => `k${i}`),
          'c1'
        )
      )
      .mockResolvedValueOnce(page(['k30', 'k31']));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    await c.loadMore();
    expect(c.rows).toHaveLength(32);

    fetchPage.mockReset();
    fetchPage.mockResolvedValueOnce(page(Array.from({ length: 31 }, (_, i) => `k${i + 1}`)));
    await c.refresh();
    expect(fetchPage).toHaveBeenCalledTimes(1);
    expect(fetchPage).toHaveBeenCalledWith('activity', 32, undefined, expect.any(AbortSignal));
    expect(c.rows.map((r) => r.conversationKey)).not.toContain('k0');
    expect(c.rows).toHaveLength(31);
  });

  it('a failed refresh keeps the rows shown and reports the error', async () => {
    const fetchPage = vi.fn().mockResolvedValueOnce(page(['a']));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    fetchPage.mockRejectedValueOnce(new Error('offline'));
    await c.refresh();
    expect(c.status).toBe('ready');
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['a']);
    expect(c.error).toBe('offline');
    fetchPage.mockResolvedValueOnce(page(['a']));
    await c.refresh();
    expect(c.error).toBe('');
  });

  it('coalesces refreshes that arrive while one runs into one more', async () => {
    const gate = deferred<StarredConversationPage>();
    const fetchPage = vi.fn().mockResolvedValueOnce(page(['a']));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    fetchPage.mockReturnValueOnce(gate.promise).mockResolvedValueOnce(page(['b']));
    const running = c.refresh();
    void c.refresh();
    void c.refresh();
    gate.resolve(page(['a']));
    await running;
    expect(fetchPage).toHaveBeenCalledTimes(3);
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['b']);
  });

  it('a sort restart reports loading, not the empty collection, until its first page', async () => {
    const next = deferred<StarredConversationPage>();
    const fetchPage = vi
      .fn()
      .mockResolvedValueOnce(page(['a']))
      .mockReturnValueOnce(next.promise);
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    expect(c.status).toBe('ready');
    const restarted = c.start('alpha');
    expect(c.status).toBe('loading');
    expect(c.rows).toEqual([]);
    next.resolve(page(['b']));
    await restarted;
    expect(c.status).toBe('ready');
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['b']);
  });

  it('marks the rail unavailable when the hub answers 404 (experiment off there)', async () => {
    const { StarredRailError } = await import('./chat-starred-rail.js');
    const fetchPage = vi.fn().mockRejectedValueOnce(new StarredRailError('gone', 404));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    expect(c.unavailable).toBe(true);
    fetchPage.mockRejectedValueOnce(new StarredRailError('boom', 500));
    await c.start('alpha');
    expect(c.unavailable).toBe(false);
  });

  it('a Load more answered 404 marks the rail unavailable too', async () => {
    const { StarredRailError } = await import('./chat-starred-rail.js');
    const fetchPage = vi.fn().mockResolvedValueOnce(page(['a'], 'c1'));
    const c = new StarredRailController(fetchPage);
    await c.start('activity');
    fetchPage.mockRejectedValueOnce(new StarredRailError('gone', 404));
    await c.loadMore();
    expect(c.unavailable).toBe(true);
    expect(c.moreStatus).toBe('error');
  });

  it('drops answers for a stopped rail or an older sort', async () => {
    const old = deferred<StarredConversationPage>();
    const fetchPage = vi
      .fn()
      .mockReturnValueOnce(old.promise)
      .mockResolvedValueOnce(page(['alpha-row']));
    const c = new StarredRailController(fetchPage);
    const first = c.start('activity');
    const second = c.start('alpha');
    await second;
    old.resolve(page(['stale']));
    await first;
    expect(c.sort).toBe('alpha');
    expect(c.rows.map((r) => r.conversationKey)).toEqual(['alpha-row']);

    const late = deferred<StarredConversationPage>();
    fetchPage.mockReturnValueOnce(late.promise);
    const pending = c.refresh();
    c.stop();
    late.resolve(page(['late']));
    await pending;
    expect(c.status).toBe('idle');
    expect(c.rows).toEqual([]);
    await c.refresh();
    expect(fetchPage).toHaveBeenCalledTimes(3);
  });
});
