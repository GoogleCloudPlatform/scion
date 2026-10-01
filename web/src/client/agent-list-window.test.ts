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
    ...partial,
  };
}

/**
 * Builds a window plus a `setHeld` helper, since the small state reads the
 * held agents through a live callback rather than a copy (round 1 review
 * B1) — `getProjectId` and `getHeldAgents` default to fixed/closed-over
 * values so most tests don't need to care about them.
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
  it('display/items match agent-sort over the held set for every filter (A5 parity surface)', () => {
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

  it('re-derives from the held array by reference — no copy, no re-adoption step (round 1 review B1)', () => {
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
    expect(win.stats).toEqual({ total: 1, running: 0 });
  });

  it('setSmall() never resets pageIndex (only setViewState does, round 1 review B1)', () => {
    const agents = [agent('a'), agent('b'), agent('c'), agent('d'), agent('e')];
    const { win, setHeld } = createWindow({ viewState: makeViewState({ pageSize: 2 }) });
    setHeld(agents);
    void win.next();
    expect(win.pageIndex).toBe(1);
    win.setSmall(); // a later trigger re-affirms small state
    expect(win.pageIndex).toBe(1); // unchanged
  });

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
    expect(win.stats).toEqual({ total: 2, running: 1 });
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

  it('R2-B2: an off-page upsert of an already-known member updates the member index and raises the chip, with no page-row change', () => {
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
    // page's first key, so on page 0 it counts as "entering range" (design
    // §6.2) and the chip shows regardless of the phase change itself.
    known.set('c', agent('c', { phase: 'stopped' }));
    win.applyChanges({ upserted: ['c'], deleted: [], unknown: new Map(), generation: 1 });

    expect(win.updatesAvailable).toBe(true);
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']); // on-page rows unchanged
    expect(win.memberIndex.getPhase('c')).toBe('stopped');
    expect(win.stats).toEqual({ total: 3, running: 2 });
    expect(fetchPage).not.toHaveBeenCalled(); // no request (design §6.2)
  });

  it('N1: an off-page member change affecting counts only (no filter, key stays outside the page range) raises no chip', () => {
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
    // event — see the R2-B2 and B6 tests).
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
    expect(win.stats).toEqual({ total: 2, running: 1 });
    expect(win.updatesAvailable).toBe(false); // counts-only: no chip (design §6.2)
  });

  it('N1: an off-page member that newly passes the active phase filter raises the chip', () => {
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

    expect(win.updatesAvailable).toBe(true); // newly passes the filter (design §6.2)
  });

  it('B6: an off-page upsert for a non-member is added only if it passes the project + committed-label add rule', () => {
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

    // No active phase filter: a phase-only change is counts-only (design §6.2 N1).
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

  it('markResync raises the chip and issues no request (reconnect, design §6.2/§7)', () => {
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

  it("a sort/phase/page-size change never calls fetchPage by itself (round 1 review B3 — project-detail.ts's syncAgentsForViewState owns that decision)", () => {
    const fetchPage = vi.fn();
    const { win } = createWindow({ viewState: makeViewState(), fetchPage });
    win.setPaged(pagedResult([agent('a')], { totalCount: 1 }), '');
    win.setViewState({ sortField: 'name' });
    win.setViewState({ phaseFilter: 'running' });
    win.setViewState({ pageSize: 50 });
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('applyChanges is a no-op in the small state (design §11 P1c: small uses onAgentsUpdated instead)', () => {
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
