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
 * Covers the small and paged window states.
 */

import { describe, it, expect, vi } from 'vitest';
import type { Agent } from '../shared/types.js';
import { AgentListWindow } from './agent-list-window.js';
import type {
  AgentListViewState,
  AgentListWindowOptions,
  PagedPageResult,
} from './agent-list-window.js';

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
    view: 'list',
    ...partial,
  };
}

/**
 * Builds a window plus a `setHeld` helper, since the small state reads the
 * held agents through a live callback rather than a copy — `getProjectId`
 * and `getHeldAgents` default to fixed/closed-over values so most tests
 * don't need to care about them.
 */
function createWindow(
  options: Partial<AgentListWindowOptions> & { viewState: AgentListViewState }
): { win: AgentListWindow; setHeld: (agents: Agent[]) => void } {
  let held: Agent[] = [];
  const win = new AgentListWindow({
    viewState: options.viewState,
    fetchPage: options.fetchPage ?? vi.fn(),
    getAgent: options.getAgent ?? (() => undefined),
    getProjectId: options.getProjectId ?? (() => 'p-1'),
    getHeldAgents: options.getHeldAgents ?? (() => held),
  });
  return {
    win,
    setHeld: (agents: Agent[]) => {
      held = agents;
      win.setSmall();
    },
  };
}

describe('AgentListWindow — small state', () => {
  it('display/items match agent-sort over the held set for every filter', () => {
    const agents = [
      agent('a', { name: 'Charlie', phase: 'running', updated: '2026-01-03T00:00:00Z' }),
      agent('b', { name: 'Alpha', phase: 'stopped', updated: '2026-01-02T00:00:00Z' }),
      agent('c', { name: 'Bravo', phase: 'running', updated: '2026-01-01T00:00:00Z' }),
    ];
    const { win, setHeld } = createWindow({ viewState: makeViewState({ pageSize: 10 }) });
    setHeld(agents);
    expect(win.state).toBe('small');
    // updated desc: a, b, c
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b', 'c']);

    win.setViewState({ phaseFilter: 'running' });
    expect(win.items.map((a) => a.id)).toEqual(['a', 'c']);

    win.setViewState({ phaseFilter: '', sortField: 'name', sortDir: 'asc' });
    expect(win.items.map((a) => a.id)).toEqual(['b', 'c', 'a']); // Alpha, Bravo, Charlie
  });

  it('re-derives from the held array by reference — no copy, no re-adoption step', () => {
    let agents = [agent('a', { phase: 'running' })];
    const { win } = createWindow({
      viewState: makeViewState({ pageSize: 10 }),
      getHeldAgents: () => agents,
    });
    win.setSmall();
    expect(win.items[0].phase).toBe('running');

    // Reassign the array the host owns (as `onAgentsUpdated` does on every
    // SSE flush) with no call back into the window at all.
    agents = [agent('a', { phase: 'stopped' })];
    expect(win.items[0].phase).toBe('stopped');
    expect(win.stats).toEqual({ total: 1, running: 0, incomplete: false });
  });

  it('setSmall() does not reset pageIndex when already small (only setViewState does)', () => {
    const agents = [agent('a'), agent('b'), agent('c'), agent('d'), agent('e')];
    const { win, setHeld } = createWindow({ viewState: makeViewState({ pageSize: 2 }) });
    setHeld(agents);
    void win.next();
    expect(win.pageIndex).toBe(1);
    win.setSmall(); // a later trigger re-affirms small state (small -> small)
    expect(win.pageIndex).toBe(1); // unchanged
  });

  it('setSmall() resets pageIndex to 0 when the previous state was paged', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    const fetchPage = vi.fn(async (params: { cursor?: string }) =>
      !params.cursor
        ? pagedResult(page0, { nextCursor: 'c1', totalCount: 4 })
        : pagedResult(page1, { totalCount: 4 })
    );
    const { win, setHeld } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    await win.next();
    expect(win.state).toBe('paged');
    expect(win.pageIndex).toBe(1);

    // A label commit (or any trigger) whose response makes the set small —
    // e.g. it now fits under `fit` — adopts an entirely different data set.
    setHeld([agent('x'), agent('y')]);
    expect(win.state).toBe('small');
    expect(win.pageIndex).toBe(0); // reset: paged -> small always swaps data sets
    expect(win.items.map((a) => a.id)).toEqual(['x', 'y']);
  });

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

  it('paginates locally with 0 fetches', () => {
    const fetchPage = vi.fn();
    const agents = [agent('a'), agent('b'), agent('c'), agent('d'), agent('e')];
    const { win, setHeld } = createWindow({ viewState: makeViewState({ pageSize: 2 }), fetchPage });
    setHeld(agents);
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
    const { setHeld, win } = createWindow({ viewState: makeViewState() });
    setHeld([agent('a', { phase: 'running' }), agent('b', { phase: 'stopped' })]);
    expect(win.stats).toEqual({ total: 2, running: 1, incomplete: false });
  });

  it('a label/phase/sort change never calls fetchPage while small', () => {
    const fetchPage = vi.fn();
    const { win, setHeld } = createWindow({ viewState: makeViewState(), fetchPage });
    setHeld([agent('a', { labels: { env: 'prod' } }), agent('b')]);
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
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
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
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    await win.next();
    expect(win.pageIndex).toBe(0);
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
  });

  it('an off-page upsert of an already-known member updates the member index and raises the chip, with no page-row change', () => {
    const page0 = [agent('a', { phase: 'running' }), agent('b', { phase: 'running' })];
    const fetchPage = vi.fn();
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['b', page0[1]],
      ['c', agent('c', { phase: 'running' })], // off-page member, e.g. from an earlier page
    ]);
    const { win } = createWindow({
      viewState: makeViewState(),
      fetchPage,
      getAgent: (id) => known.get(id),
    });
    win.setPaged(
      pagedResult(page0, {
        totalCount: 3,
        stats: {
          total: 3,
          running: 3,
          agents: [
            ['a', 'running'],
            ['b', 'running'],
            ['c', 'running'],
          ],
        },
      }),
      ''
    );
    expect(win.updatesAvailable).toBe(false);

    // Off-page member 'c' changes phase. Its (default, tied) key is >= the
    // page's first key, so on page 0 it counts as "entering range" and the chip shows regardless of the phase change itself.
    known.set('c', agent('c', { phase: 'stopped' }));
    win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(true);
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']); // on-page rows unchanged
    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.stats).toEqual({ total: 3, running: 2, incomplete: false });
    expect(fetchPage).not.toHaveBeenCalled(); // no request
  });

  it('an off-page member change affecting counts only (no filter, key stays outside the page range) raises no chip', () => {
    const page0 = [agent('a', { phase: 'running', updated: '2026-02-01T00:00:00Z' })];
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['c', agent('c', { phase: 'running', updated: '2026-01-01T00:00:00Z' })], // off-page, older key
    ]);
    // Page 1 (not page 0): "entering range" requires the key to fall inside
    // [last, first], and the page-0 top exception does not apply.
    const { win } = createWindow({
      viewState: makeViewState(),
      getAgent: (id) => known.get(id),
    });
    // 'c' must already be a member (seeded via stats.agents) for this to be
    // an *existing* off-page member's update, not a new admission (every
    // new admission raises the chip unconditionally, matching a `created`
    // event — see the off-page-upsert and add-rule tests above/below).
    win.setPaged(
      pagedResult(page0, {
        totalCount: 2,
        stats: {
          total: 2,
          running: 2,
          agents: [
            ['a', 'running'],
            ['c', 'running'],
          ],
        },
      }),
      ''
    );

    known.set('c', agent('c', { phase: 'stopped', updated: '2026-01-01T00:00:00Z' }));
    win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(win.updatesAvailable).toBe(false); // counts-only: no chip
  });

  it('an off-page member that newly passes the active phase filter raises the chip', () => {
    const page0 = [agent('a', { phase: 'running', updated: '2026-02-01T00:00:00Z' })];
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['c', agent('c', { phase: 'stopped', updated: '2026-01-01T00:00:00Z' })],
    ]);
    const { win } = createWindow({
      viewState: makeViewState({ phaseFilter: 'running' }),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(
      pagedResult(page0, {
        totalCount: 2,
        stats: {
          total: 2,
          running: 1,
          agents: [
            ['a', 'running'],
            ['c', 'stopped'],
          ],
        },
      }),
      ''
    );
    expect(win.updatesAvailable).toBe(false);

    known.set('c', agent('c', { phase: 'running', updated: '2026-01-01T00:00:00Z' }));
    win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(true); // newly passes the filter
  });

  it('an off-page upsert for a non-member is added only if it passes the project + committed-label add rule', () => {
    const page0 = [agent('a')];
    const nonMatchingLabel = agent('x', { projectId: 'p-1', labels: { env: 'dev' } });
    const wrongProject = agent('y', { projectId: 'p-2', labels: { env: 'prod' } });
    const matching = agent('z', { projectId: 'p-1', labels: { env: 'prod' } });
    const known = new Map<string, Agent>([
      ['a', page0[0]],
      ['x', nonMatchingLabel],
      ['y', wrongProject],
      ['z', matching],
    ]);
    const { win } = createWindow({
      viewState: makeViewState(),
      getAgent: (id) => known.get(id),
      getProjectId: () => 'p-1',
    });
    win.setPaged(pagedResult(page0, { totalCount: 1 }), 'env=prod');

    win.applyChanges({ upserted: ['x'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.memberIndex.has('x')).toBe(false); // wrong label: never added
    expect(win.stats.total).toBe(1);

    win.applyChanges({ upserted: ['y'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.memberIndex.has('y')).toBe(false); // wrong project: never added
    expect(win.stats.total).toBe(1);

    win.applyChanges({ upserted: ['z'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.memberIndex.has('z')).toBe(true); // matches: added
    expect(win.stats.total).toBe(2);
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
    const { win } = createWindow({
      viewState: makeViewState({ sortDir: 'desc' }),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 2 }), '');

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
    const { win } = createWindow({
      viewState: makeViewState({ phaseFilter: 'running' }),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 2 }), '');

    known.set('b', agent('b', { phase: 'stopped' }));
    win.applyChanges({ upserted: ['b'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.items.map((a) => a.id)).toEqual(['a']);
    expect(win.updatesAvailable).toBe(true);
  });

  it('an on-page delete removes the row and raises the chip', () => {
    const page0 = [agent('a'), agent('b')];
    const { win } = createWindow({ viewState: makeViewState() });
    win.setPaged(pagedResult(page0, { totalCount: 2 }), '');
    win.applyChanges({ upserted: [], deleted: ['a'], unknown: new Map(), generation: 1 });
    expect(win.items.map((a) => a.id)).toEqual(['b']);
    expect(win.updatesAvailable).toBe(true);
  });

  it('a delete for an ID that is neither on-page nor in the member index is a safe no-op', () => {
    const page0 = [agent('a')];
    const { win } = createWindow({ viewState: makeViewState() });
    win.setPaged(pagedResult(page0, { totalCount: 1 }), '');
    win.applyChanges({ upserted: [], deleted: ['never-seen'], unknown: new Map(), generation: 1 });
    expect(win.updatesAvailable).toBe(false);
    expect(win.items.map((a) => a.id)).toEqual(['a']);
  });

  it('an unknown-ID delta for a member updates its phase; counts-only raises no chip, newly-passing-the-filter does, and a non-member delta is ignored', () => {
    const page0 = [agent('a', { updated: '2026-02-01T00:00:00Z' })];
    const { win } = createWindow({ viewState: makeViewState() }); // no phase filter
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
      }),
      ''
    );

    // No active phase filter: a phase-only change is counts-only.
    win.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['c', { phase: 'stopped' }]]),
      generation: 1,
    });
    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.updatesAvailable).toBe(false);

    const { win: filtered } = createWindow({
      viewState: makeViewState({ phaseFilter: 'running' }),
    });
    filtered.setPaged(
      pagedResult(page0, {
        totalCount: 1,
        stats: {
          total: 2,
          running: 0,
          agents: [
            ['a', 'running'],
            ['c', 'stopped'],
          ],
        },
      }),
      ''
    );
    filtered.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['c', { phase: 'running' }]]),
      generation: 1,
    });
    expect(filtered.updatesAvailable).toBe(true); // newly passes the active filter

    const { win: freshWin } = createWindow({ viewState: makeViewState() });
    freshWin.setPaged(pagedResult(page0, { totalCount: 1 }), '');
    freshWin.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['never-seen', { phase: 'stopped' }]]),
      generation: 1,
    });
    expect(freshWin.updatesAvailable).toBe(false); // neither on-page nor a member: ignored
  });

  it('markResync raises the chip and issues no request (reconnect)', () => {
    const fetchPage = vi.fn();
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(pagedResult([agent('a')], { totalCount: 1 }), '');
    win.markResync();
    expect(win.updatesAvailable).toBe(true);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('refresh() (the chip click) re-fetches the current page and clears the chip', async () => {
    const page0 = [agent('a')];
    const fetchPage = vi.fn(async () => pagedResult(page0, { totalCount: 1 }));
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage(), '');
    win.markResync();
    expect(win.updatesAvailable).toBe(true);
    await win.refresh();
    expect(fetchPage).toHaveBeenCalledTimes(2);
    expect(win.updatesAvailable).toBe(false);
  });

  it("a sort/phase/page-size change never calls fetchPage by itself — project-detail.ts's syncAgentsForViewState owns that decision", () => {
    const fetchPage = vi.fn();
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(pagedResult([agent('a')], { totalCount: 1 }), '');
    win.setViewState({ sortField: 'name' });
    win.setViewState({ phaseFilter: 'running' });
    win.setViewState({ pageSize: 50 });
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('applyChanges is a no-op in the small state', () => {
    const { win, setHeld } = createWindow({
      viewState: makeViewState(),
      getAgent: () => agent('a', { phase: 'stopped' }),
    });
    setHeld([agent('a', { phase: 'running' })]);
    win.applyChanges({ upserted: ['a'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.items[0].phase).toBe('running'); // unaffected
    expect(win.updatesAvailable).toBe(false);
  });
});

describe('AgentListWindow — page-0 K-range chip predicate and off-page add-rule edge cases', () => {
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

  it('setViewState does not reset pageIndex or the current page while paged (a label keystroke must not desync them)', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c', { labels: { env: 'prod' } }), agent('d')];
    const fetchPage = vi.fn(async (params: { cursor?: string }) =>
      !params.cursor
        ? pagedResult(page0, { nextCursor: 'c1', totalCount: 4 })
        : pagedResult(page1, { totalCount: 4 })
    );
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    await win.next();
    expect(win.pageIndex).toBe(1);
    expect(win.hasPrev).toBe(true);
    expect(win.items.map((a) => a.id)).toEqual(['c', 'd']);

    const callsBefore = fetchPage.mock.calls.length;
    win.setViewState({ label: 'env' }); // sl-input: local preview only
    expect(win.pageIndex).toBe(1); // unchanged — no re-adoption, no reset
    expect(win.hasPrev).toBe(true); // unchanged
    // The live preview filter IS applied to the
    // loaded page's rows, same as the small state — only 'c'
    // (which carries the `env` label) remains.
    expect(win.items.map((a) => a.id)).toEqual(['c']);
    expect(fetchPage.mock.calls.length).toBe(callsBefore); // no request
  });

  describe('on-page page-0 chip predicate is K vs last, not the off-page K vs first test', () => {
    it('desc, page 0: a mid-page row changes with its key unchanged raises no chip', () => {
      const items = [
        agent('a', { updated: '2026-01-03T00:00:00Z' }),
        agent('b', { updated: '2026-01-02T00:00:00Z' }),
        agent('c', { updated: '2026-01-01T00:00:00Z' }),
      ];
      const known = new Map(items.map((a) => [a.id, a]));
      const { win } = createWindow({
        viewState: makeViewState({ pageSize: 3 }),
        getAgent: (id) => known.get(id),
      });
      win.setPaged(pagedResult(items, { totalCount: 10, nextCursor: 'c' }), '');
      known.set('b', agent('b', { updated: '2026-01-02T00:00:00Z', phase: 'stopped' }));
      win.applyChanges({ upserted: ['b'], deleted: [], unknown: new Map(), generation: 1 });
      expect(win.updatesAvailable).toBe(false);
    });

    it('asc, page 0: a row whose key moves but stays within [first,last] raises no chip', () => {
      const items = [
        agent('a', { updated: '2026-01-01T00:00:00Z' }),
        agent('b', { updated: '2026-01-02T00:00:00Z' }),
        agent('c', { updated: '2026-01-05T00:00:00Z' }),
      ];
      const known = new Map(items.map((a) => [a.id, a]));
      const { win } = createWindow({
        viewState: makeViewState({ pageSize: 3, sortDir: 'asc' }),
        getAgent: (id) => known.get(id),
      });
      win.setPaged(pagedResult(items, { totalCount: 10, nextCursor: 'c' }), '');
      known.set('a', agent('a', { updated: '2026-01-03T00:00:00Z' }));
      win.applyChanges({ upserted: ['a'], deleted: [], unknown: new Map(), generation: 1 });
      expect(win.updatesAvailable).toBe(false);
      expect(win.items.map((a) => a.id)).toEqual(['b', 'a', 'c']); // re-sorted within the page
    });

    it('desc, page 0: a row rising to the new top (no longer bounded above) raises no chip', () => {
      const items = [
        agent('a', { updated: '2026-01-03T00:00:00Z' }),
        agent('b', { updated: '2026-01-02T00:00:00Z' }),
        agent('c', { updated: '2026-01-01T00:00:00Z' }),
      ];
      const known = new Map(items.map((a) => [a.id, a]));
      const { win } = createWindow({
        viewState: makeViewState({ pageSize: 3 }),
        getAgent: (id) => known.get(id),
      });
      win.setPaged(pagedResult(items, { totalCount: 10, nextCursor: 'c' }), '');
      known.set('c', agent('c', { updated: '2026-01-09T00:00:00Z' }));
      win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });
      expect(win.updatesAvailable).toBe(false);
      expect(win.items.map((a) => a.id)).toEqual(['c', 'a', 'b']);
    });

    it('desc, page 1 (not page 0): a row rising past the old top DOES raise the chip (the page-0 exception does not apply elsewhere)', async () => {
      const page0 = [agent('a', { updated: '2026-02-01T00:00:00Z' })];
      const page1 = [
        agent('b', { updated: '2026-01-03T00:00:00Z' }),
        agent('c', { updated: '2026-01-02T00:00:00Z' }),
      ];
      const known = new Map([...page0, ...page1].map((a) => [a.id, a]));
      const fetchPage = vi.fn(async () => pagedResult(page1, { totalCount: 3, stats: undefined }));
      const { win } = createWindow({
        viewState: makeViewState({ pageSize: 1 }),
        fetchPage,
        getAgent: (id) => known.get(id),
      });
      win.setPaged(pagedResult(page0, { totalCount: 3, nextCursor: 'c1' }), '');
      await win.next();
      known.set('c', agent('c', { updated: '2026-01-05T00:00:00Z' })); // now above page1's old first ('b')
      win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });
      expect(win.updatesAvailable).toBe(true);
    });

    it('desc, page 0: a row falling below the bottom DOES raise the chip', () => {
      const items = [
        agent('a', { updated: '2026-01-03T00:00:00Z' }),
        agent('b', { updated: '2026-01-02T00:00:00Z' }),
        agent('c', { updated: '2026-01-01T00:00:00Z' }),
      ];
      const known = new Map(items.map((a) => [a.id, a]));
      const { win } = createWindow({
        viewState: makeViewState({ pageSize: 3 }),
        getAgent: (id) => known.get(id),
      });
      win.setPaged(pagedResult(items, { totalCount: 10, nextCursor: 'c' }), '');
      known.set('a', agent('a', { updated: '2025-01-01T00:00:00Z' })); // now older than everything
      win.applyChanges({ upserted: ['a'], deleted: [], unknown: new Map(), generation: 1 });
      expect(win.updatesAvailable).toBe(true);
    });
  });

  it('an off-page upsert of a state-known agent that fails the committed label is ignored (no chip, no addition)', () => {
    const page0 = [agent('a')];
    const nonMember = agent('x', { labels: { env: 'dev' }, updated: '2026-02-01T00:00:00Z' });
    const known = new Map([...page0, nonMember].map((a) => [a.id, a]));
    const { win } = createWindow({
      viewState: makeViewState(),
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 1 }), 'env=prod');

    win.applyChanges({ upserted: ['x'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(false);
    expect(win.memberIndex.has('x')).toBe(false);
    expect(win.stats.total).toBe(1);
  });

  it('a newly created off-page agent raises the chip only if it could land on THIS page (desc: page 0 only)', async () => {
    const page0 = [agent('a', { updated: '2026-01-03T00:00:00Z' })];
    const page1 = [agent('d', { updated: '2026-01-02T00:00:00Z' })];
    const known = new Map([...page0, ...page1].map((a) => [a.id, a]));
    const fetchPage = vi.fn(async () => pagedResult(page1, { totalCount: 3, stats: undefined }));
    const { win } = createWindow({
      viewState: makeViewState({ pageSize: 1 }),
      fetchPage,
      getAgent: (id) => known.get(id),
    });
    win.setPaged(pagedResult(page0, { totalCount: 3, nextCursor: 'c1' }), '');
    await win.next();
    expect(win.pageIndex).toBe(1);

    const created = agent('n', { updated: '2026-03-01T00:00:00Z' }); // newest overall: would land on page 0, not here
    known.set('n', created);
    win.applyChanges({ upserted: ['n'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(false); // page 1 can never receive a brand-new desc row
    expect(win.memberIndex.has('n')).toBe(true); // still added as a member (stats update live)
  });

  it('a newly created agent on page 0 (desc) DOES raise the chip', () => {
    const page0 = [agent('a', { updated: '2026-01-03T00:00:00Z' })];
    const { win } = createWindow({
      viewState: makeViewState({ pageSize: 1 }),
      getAgent: (id) => (id === 'n' ? agent('n', { updated: '2026-02-01T00:00:00Z' }) : undefined),
    });
    win.setPaged(pagedResult(page0, { totalCount: 1 }), '');
    win.applyChanges({ upserted: ['n'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.updatesAvailable).toBe(true);
  });

  it('rangeStart tracks the real running offset, not pageIndex * pageSize, across a short page', async () => {
    const page0 = [agent('a'), agent('b'), agent('c')]; // a short page0: 3 rows at pageSize 3 (full)
    const page1 = [agent('d'), agent('e')]; // page1 is SHORT: only 2 rows (one race-dropped)
    const fetchPage = vi.fn(async (params: { cursor?: string }) =>
      !params.cursor
        ? pagedResult(page0, { nextCursor: 'c1', totalCount: 10 })
        : pagedResult(page1, { nextCursor: 'c2', totalCount: 10 })
    );
    const { win } = createWindow({ viewState: makeViewState({ pageSize: 3 }), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 3, wantStats: true }), '');
    expect(win.rangeStart).toBe(0);
    await win.next();
    expect(win.rangeStart).toBe(3); // rows before page 1: exactly page0's 3 rows
    await win.next(); // page 2 starts after page1's actual (short) 2 rows, not an assumed 3
    expect(fetchPage).toHaveBeenLastCalledWith({ cursor: 'c2', limit: 3, wantStats: false });
    expect(win.rangeStart).toBe(5); // 3 + 2, not 3 + 3
  });

  it('display is memoized on (held identity, view state identity)', () => {
    let agents = [agent('b'), agent('a')];
    const { win } = createWindow({ viewState: makeViewState(), getHeldAgents: () => agents });
    win.setSmall();
    const d1 = win.display;
    const d2 = win.display;
    expect(d1).toBe(d2); // same reference: not recomputed

    win.setViewState({ phaseFilter: 'running' });
    const d3 = win.display;
    expect(d3).not.toBe(d1); // view state changed: recomputed
    const d4 = win.display;
    expect(d4).toBe(d3); // stable again until something changes

    agents = [agent('b'), agent('a')]; // host reassigns this.agents (new array, same content)
    const d5 = win.display;
    expect(d5).not.toBe(d3); // held identity changed: recomputed even though content is equal
  });
});

describe('AgentListWindow — cursor invalidation after a failed view-change', () => {
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

  it('invalidateCursors() clears hasNext/hasPrev while paged, until the next setPaged', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    const fetchPage = vi.fn(async (params: { cursor?: string }) =>
      !params.cursor
        ? pagedResult(page0, { nextCursor: 'c1', totalCount: 4 })
        : pagedResult(page1, { totalCount: 4 })
    );
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    expect(win.hasNext).toBe(true);
    expect(win.hasPrev).toBe(false);

    // Simulate a failed view-change request (e.g. a phase change that 500s)
    // while still on a page fetched under the old params.
    win.invalidateCursors();
    expect(win.hasNext).toBe(false); // would otherwise replay a cursor minted under the old params
    expect(win.hasPrev).toBe(false);

    // Next is now a no-op: fetchPage is not called again, and the window
    // stays exactly where it was (no error, same page, same rows).
    const callsBefore = fetchPage.mock.calls.length;
    await win.next();
    expect(fetchPage.mock.calls.length).toBe(callsBefore);
    expect(win.pageIndex).toBe(0);
    expect(win.error).toBeNull();
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);

    // The next successful setPaged (e.g. retrying the view-change) restores
    // navigation.
    win.setPaged(pagedResult(page1, { totalCount: 4 }), '');
    expect(win.hasNext).toBe(false); // page1 happens to be the last page here
    expect(win.hasPrev).toBe(false); // setPaged always re-adopts at page 0
  });

  it('invalidateCursors() is a no-op in the small state', () => {
    const { win, setHeld } = createWindow({ viewState: makeViewState() });
    setHeld([agent('a'), agent('b')]);
    expect(win.state).toBe('small');
    win.invalidateCursors();
    expect(win.state).toBe('small');
    expect(win.hasNext).toBe(false); // unaffected — governed by display.length, not cursors
  });
});

describe('AgentListWindow — refreshing a stranded page after an invalidation', () => {
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

  it('invalidateCursors() also clears hasPrev on a page other than 0, and prev() makes 0 fetches', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    const fetchPage = vi.fn(async (params: { cursor?: string }) =>
      !params.cursor
        ? pagedResult(page0, { nextCursor: 'c1', totalCount: 4 })
        : pagedResult(page1, { totalCount: 4 })
    );
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    await win.next();
    expect(win.pageIndex).toBe(1);
    expect(win.hasPrev).toBe(true); // on page 1, Prev is normally available

    win.invalidateCursors();
    expect(win.hasPrev).toBe(false); // the stored page-0 cursor slot (undefined) is fine, but
    // this is deliberately conservative: nothing distinguishes "this
    // particular cursor is safe" from "the stack might be stale" once
    // invalidated, so Prev is refused too, not just Next.

    const callsBefore = fetchPage.mock.calls.length;
    await win.prev();
    expect(fetchPage.mock.calls.length).toBe(callsBefore); // 0 fetches
    expect(win.pageIndex).toBe(1); // unchanged
  });

  it('refresh() while invalidated refetches page 0 instead of the current (stale-cursor) page, and restores navigation', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    const page0Again = [agent('e', { phase: 'stopped' }), agent('f', { phase: 'stopped' })];
    const page1Again = [agent('g', { phase: 'stopped' }), agent('h', { phase: 'stopped' })];
    const fetchPage = vi.fn(async (params: { cursor?: string }) => {
      if (!params.cursor) {
        // The second cursor-free call simulates the server now answering
        // under the new (post-view-change) params.
        return fetchPage.mock.calls.length <= 1
          ? pagedResult(page0, { nextCursor: 'c1', totalCount: 4 })
          : pagedResult(page0Again, { nextCursor: 'c1-new', totalCount: 4 });
      }
      if (params.cursor === 'c1') return pagedResult(page1, { totalCount: 4 });
      // The fresh cursor minted by the page-0-again response: distinct from
      // 'c1' (page 1's original, now-stale cursor), so a call with it
      // proves `next()` is using the newly-minted one, not the old one.
      if (params.cursor === 'c1-new') return pagedResult(page1Again, { totalCount: 4 });
      throw new Error(`unexpected cursor: ${params.cursor}`);
    });
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(await fetchPage({ cursor: undefined, limit: 2, wantStats: true }), '');
    await win.next();
    expect(win.pageIndex).toBe(1);

    // A view-change trigger fails while on page 1 (e.g. a phase change that
    // 500s): the window invalidates, exactly like the scenario above.
    win.invalidateCursors();
    expect(win.hasPrev).toBe(false);
    expect(win.hasNext).toBe(false);

    // The resync chip (an SSE signal) can still fire independently of
    // navigation.
    win.markResync();
    expect(win.updatesAvailable).toBe(true);

    // Clicking the chip calls refresh(). Because the stack is invalidated,
    // this must NOT replay page 1's stale cursor — it refetches page 0
    // instead (whose cursor is always `undefined`, so it cannot mismatch).
    const callsBefore = fetchPage.mock.calls.length;
    await win.refresh();
    expect(fetchPage).toHaveBeenNthCalledWith(callsBefore + 1, {
      cursor: undefined,
      limit: 2,
      wantStats: true,
    });
    expect(win.pageIndex).toBe(0);
    expect(win.items.map((a) => a.id)).toEqual(['e', 'f']);
    expect(win.error).toBeNull();

    // A successful page-0 fetch mints a fresh cursor stack, so navigation
    // is restored without waiting for a brand-new setPaged.
    expect(win.hasNext).toBe(true); // page0Again has a (fresh) nextCursor
    expect(win.hasPrev).toBe(false); // page 0
    expect(win.updatesAvailable).toBe(false);

    // Prove it's actually the *fresh* cursor, minted under the new params,
    // not the stale one from before the invalidation.
    await win.next();
    expect(fetchPage).toHaveBeenLastCalledWith({ cursor: 'c1-new', limit: 2, wantStats: false });
    expect(win.items.map((a) => a.id)).toEqual(['g', 'h']);
  });
});

describe('AgentListWindow — cursor invalidation vs. a concurrent window fetch', () => {
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

  it('a page-0 fetch already in flight when the stack is invalidated must not re-validate it with a cursor minted under the old params', async () => {
    const page0 = [agent('a'), agent('b')];
    const page1 = [agent('c'), agent('d')];
    let resolvePending!: (r: PagedPageResult) => void;
    const pending = new Promise<PagedPageResult>((resolve) => {
      resolvePending = resolve;
    });
    const fetchPage = vi.fn((params: { cursor?: string }) => {
      if (!params.cursor) return pending; // the in-flight page-0 fetch below
      return Promise.resolve(pagedResult(page1, { nextCursor: 'c1', totalCount: 4 }));
    });
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(pagedResult(page0, { nextCursor: 'c1', totalCount: 4 }), '');
    await win.next();
    expect(win.pageIndex).toBe(1);

    // Start a page-0 fetch under the current (old) params; leave it pending.
    const prevDone = win.prev();

    // Meanwhile, a view-change elsewhere fails and invalidates the stack
    // (the same call the host makes on a failed request while paged).
    win.invalidateCursors();
    expect(win.hasNext).toBe(false);
    // Dropping the in-flight fetch must also clear the loading flag: its
    // finally block is skipped for a superseded generation, so nothing else
    // would, and the pager and refresh chip would stay disabled.
    expect(win.loading).toBe(false);

    // The in-flight page-0 fetch — requested before the invalidation, and
    // so carrying a cursor bound to the stale, pre-invalidation params —
    // now lands.
    resolvePending(pagedResult(page0, { nextCursor: 'c1-stale', totalCount: 4 }));
    await prevDone;
    expect(win.loading).toBe(false);

    // Its result must not re-validate the stack: a late response from a
    // fetch started under now-obsolete params is exactly what the
    // generation bump in invalidateCursors() is for.
    expect(win.hasNext).toBe(false);
  });
});
