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
 * W4 (design §9): the small and paged window states (P1c, design §11).
 */

import { describe, it, expect, vi } from 'vitest';
import type { Agent } from '../shared/types.js';
import { AgentListWindow } from './agent-list-window.js';
import type { AgentListViewState, PagedPageResult } from './agent-list-window.js';

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    ...overrides,
  } as Agent;
}

function makeViewState(partial: Partial<AgentListViewState> = {}): AgentListViewState {
  return {
    phaseFilter: '',
    label: '',
    sortField: 'updated',
    sortDir: 'desc',
    pageSize: 2,
    ...partial,
  };
}

describe('AgentListWindow — small state', () => {
  it('display/items match agent-sort over the held set for every filter (A5 parity surface)', () => {
    const agents = [
      agent('a', { name: 'Charlie', phase: 'running', updated: '2026-01-03T00:00:00Z' }),
      agent('b', { name: 'Alpha', phase: 'stopped', updated: '2026-01-02T00:00:00Z' }),
      agent('c', { name: 'Bravo', phase: 'running', updated: '2026-01-01T00:00:00Z' }),
    ];
    const win = new AgentListWindow({
      viewState: makeViewState({ pageSize: 10 }),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    win.setSmall(agents);
    expect(win.state).toBe('small');
    // updated desc: a, b, c
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b', 'c']);

    win.setViewState({ phaseFilter: 'running' });
    expect(win.items.map((a) => a.id)).toEqual(['a', 'c']);

    win.setViewState({ phaseFilter: '', sortField: 'name', sortDir: 'asc' });
    expect(win.items.map((a) => a.id)).toEqual(['b', 'c', 'a']); // Alpha, Bravo, Charlie
  });

  it('paginates locally with 0 fetches', () => {
    const fetchPage = vi.fn();
    const agents = [agent('a'), agent('b'), agent('c'), agent('d'), agent('e')];
    const win = new AgentListWindow({
      viewState: makeViewState({ pageSize: 2 }),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setSmall(agents);
    expect(win.total).toBe(5);
    expect(win.hasNext).toBe(true);
    expect(win.hasPrev).toBe(false);
    void win.next();
    expect(win.pageIndex).toBe(1);
    void win.prev();
    expect(win.pageIndex).toBe(0);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('stats come from the held set (isAgentRunning)', () => {
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    win.setSmall([agent('a', { phase: 'running' }), agent('b', { phase: 'stopped' })]);
    expect(win.stats).toEqual({ total: 2, running: 1 });
  });

  it('a label/phase/sort change never calls fetchPage while small', () => {
    const fetchPage = vi.fn();
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setSmall([agent('a', { labels: { env: 'prod' } }), agent('b')]);
    win.setViewState({ label: 'env=prod' });
    win.setViewState({ phaseFilter: 'running' });
    win.setViewState({ sortField: 'name', sortDir: 'asc' });
    expect(fetchPage).not.toHaveBeenCalled();
  });
});

describe('AgentListWindow — paged state', () => {
  function pagedResult(agents: Agent[], opts: Partial<PagedPageResult> = {}): PagedPageResult {
    return {
      agents,
      totalCount: agents.length,
      stats: {
        total: agents.length,
        running: agents.filter((a) => a.phase === 'running').length,
        agents: agents.map((a) => [a.id, a.phase]),
      },
      ...opts,
    };
  }

  it('next()/prev() call fetchPage with the right cursor and update pageIndex/total', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    const fetchPage = vi.fn(async (params: { cursor?: string }) => {
      if (!params.cursor) return pagedResult(page0, { nextCursor: 'cursor-1', totalCount: 4 });
      if (params.cursor === 'cursor-1') return pagedResult(page1, { totalCount: 4 });
      throw new Error('unexpected cursor');
    });
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }));
    expect(win.state).toBe('paged');
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
    expect(win.hasNext).toBe(true);

    await win.next();
    expect(fetchPage).toHaveBeenLastCalledWith({ cursor: 'cursor-1', limit: 2, wantStats: false });
    expect(win.items.map((a) => a.id)).toEqual(['c', 'd']);
    expect(win.pageIndex).toBe(1);
    expect(win.hasNext).toBe(false);

    await win.prev();
    expect(fetchPage).toHaveBeenLastCalledWith({ cursor: undefined, limit: 2, wantStats: true });
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
  });

  it('steps back a page when a refetch returns empty (an emptied last page)', async () => {
    const page0 = [agent('a'), agent('b')];
    let secondCall = 0;
    const fetchPage = vi.fn(async (params: { cursor?: string }) => {
      if (!params.cursor) return pagedResult(page0, { nextCursor: 'c1', totalCount: 3 });
      secondCall++;
      if (secondCall === 1) return pagedResult([], { totalCount: 2 }); // page 1 is now empty
      return pagedResult(page0, { totalCount: 2 }); // step-back refetch of page 0
    });
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }));
    await win.next();
    expect(win.pageIndex).toBe(0);
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
  });

  it('R2-B2: an off-page upsert of an ID already known updates the member index and raises the chip, with no page-row change', () => {
    const page0 = [agent('a', { phase: 'running' }), agent('b', { phase: 'running' })];
    const fetchPage = vi.fn();
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['b', page0[1]],
      ['c', agent('c', { phase: 'running' })], // off-page member, e.g. from an earlier page
    ]);
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 3 }));
    expect(win.updatesAvailable).toBe(false);

    // Off-page member 'c' changes phase.
    known.set('c', agent('c', { phase: 'stopped' }));
    win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(true);
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']); // on-page rows unchanged
    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.stats).toEqual({ total: 3, running: 2 });
    expect(fetchPage).not.toHaveBeenCalled(); // no request (design §6.2)
  });

  it('an on-page upsert replaces the row and re-sorts locally (Q-D)', () => {
    const page0 = [
      agent('a', { updated: '2026-01-02T00:00:00Z' }),
      agent('b', { updated: '2026-01-01T00:00:00Z' }),
    ];
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['b', page0[1]],
    ]);
    const win = new AgentListWindow({
      viewState: makeViewState({ sortDir: 'desc' }),
      fetchPage: vi.fn(),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 2 }));

    // 'b' becomes the most-recently-updated: should move to the top.
    const bUpdated = agent('b', { updated: '2026-01-05T00:00:00Z' });
    known.set('b', bUpdated);
    win.applyChanges({ upserted: ['b'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.items.map((a) => a.id)).toEqual(['b', 'a']);
  });

  it('an on-page agent that now fails the phase filter is removed (backfill chip)', () => {
    const page0 = [agent('a', { phase: 'running' }), agent('b', { phase: 'running' })];
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['b', page0[1]],
    ]);
    const win = new AgentListWindow({
      viewState: makeViewState({ phaseFilter: 'running' }),
      fetchPage: vi.fn(),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 2 }));

    known.set('b', agent('b', { phase: 'stopped' }));
    win.applyChanges({ upserted: ['b'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.items.map((a) => a.id)).toEqual(['a']);
    expect(win.updatesAvailable).toBe(true);
  });

  it('an on-page delete removes the row and raises the chip', () => {
    const page0 = [agent('a'), agent('b')];
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    win.setPaged(pagedResult(page0, { totalCount: 2 }));
    win.applyChanges({ upserted: [], deleted: ['a'], unknown: new Map(), generation: 1 });
    expect(win.items.map((a) => a.id)).toEqual(['b']);
    expect(win.updatesAvailable).toBe(true);
  });

  it('a delete for an ID that is neither on-page nor in the member index is a safe no-op', () => {
    const page0 = [agent('a')];
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    win.setPaged(pagedResult(page0, { totalCount: 1 }));
    win.applyChanges({ upserted: [], deleted: ['never-seen'], unknown: new Map(), generation: 1 });
    expect(win.updatesAvailable).toBe(false);
    expect(win.items.map((a) => a.id)).toEqual(['a']);
  });

  it('an unknown-ID delta for a member updates its phase and raises the chip; for a non-member it is ignored', () => {
    const page0 = [agent('a')];
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    win.setPaged(
      pagedResult(page0, {
        totalCount: 1,
        stats: {
          total: 2,
          running: 1,
          agents: [
            ['a', 'running'],
            ['c', 'running'],
          ],
        },
      })
    );

    win.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['c', { phase: 'stopped' }]]),
      generation: 1,
    });
    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.updatesAvailable).toBe(true);

    const freshWin = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
    });
    freshWin.setPaged(pagedResult(page0, { totalCount: 1 }));
    freshWin.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['never-seen', { phase: 'stopped' }]]),
      generation: 1,
    });
    expect(freshWin.updatesAvailable).toBe(false);
  });

  it('markResync raises the chip and issues no request (reconnect, design §6.2/§7)', () => {
    const fetchPage = vi.fn();
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setPaged(pagedResult([agent('a')], { totalCount: 1 }));
    win.markResync();
    expect(win.updatesAvailable).toBe(true);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('refresh() (the chip click) re-fetches the current page and clears the chip', async () => {
    const page0 = [agent('a')];
    const fetchPage = vi.fn(async () => pagedResult(page0, { totalCount: 1 }));
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: () => undefined,
    });
    win.setPaged(await fetchPage());
    win.markResync();
    expect(win.updatesAvailable).toBe(true);
    await win.refresh();
    expect(fetchPage).toHaveBeenCalledTimes(2);
    expect(win.updatesAvailable).toBe(false);
  });

  it('applyChanges is a no-op in the small state (design §11 P1c: small uses onAgentsUpdated instead)', () => {
    const win = new AgentListWindow({
      viewState: makeViewState(),
      fetchPage: vi.fn(),
      getAgent: () => agent('a', { phase: 'stopped' }),
    });
    win.setSmall([agent('a', { phase: 'running' })]);
    win.applyChanges({ upserted: ['a'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.items[0].phase).toBe('running'); // unaffected
    expect(win.updatesAvailable).toBe(false);
  });
});
