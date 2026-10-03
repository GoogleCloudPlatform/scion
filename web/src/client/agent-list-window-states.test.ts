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
 * The four window states (small, paged, held, capped): the request planner
 * table, every state transition, sorted-mode refusal per committed label,
 * the capped total, and the stale banner.
 */

import { describe, it, expect, vi } from 'vitest';
import type { Agent } from '../shared/types.js';
import { AgentListWindow, cappedTotalText } from './agent-list-window.js';
import type {
  AgentListRequestPlan,
  AgentListTrigger,
  AgentListViewState,
  PagedPageResult,
  WindowState,
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

function viewState(partial: Partial<AgentListViewState> = {}): AgentListViewState {
  return {
    phaseFilter: '',
    label: '',
    sortField: 'updated',
    sortDir: 'desc',
    pageSize: 25,
    view: 'list',
    ...partial,
  };
}

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

function setup(vs: Partial<AgentListViewState> = {}) {
  let held: Agent[] = [];
  const fetchPage = vi.fn(async () => pagedResult([]));
  const win = new AgentListWindow({
    viewState: viewState(vs),
    fetchPage,
    getAgent: () => undefined,
    getProjectId: () => 'p-1',
    getHeldAgents: () => held,
  });
  const setHeld = (agents: Agent[]): void => {
    held = agents;
  };
  return { win, fetchPage, setHeld };
}

/** Puts a fresh window into `state`. */
function enter(win: AgentListWindow, setHeld: (a: Agent[]) => void, state: WindowState): void {
  switch (state) {
    case 'small':
      setHeld([agent('a')]);
      win.setSmall();
      break;
    case 'paged':
      win.setPaged(pagedResult([agent('a')], { nextCursor: 'c1', totalCount: 60 }), '');
      break;
    case 'held':
      setHeld([agent('a'), agent('b')]);
      win.adoptDrain({ complete: true, capped: false, error: null, requests: 2 });
      break;
    case 'capped':
      setHeld([agent('a'), agent('b')]);
      win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
      break;
  }
  expect(win.state).toBe(state);
}

const STATES: WindowState[] = ['small', 'paged', 'held', 'capped'];

/**
 * Expected plan per trigger and state, for an eligible view state (grid or
 * list, updated or created sort, empty or k=v label) and an ineligible one.
 */
const PLAN_TABLE: Array<{
  trigger: AgentListTrigger;
  eligible: Record<WindowState, AgentListRequestPlan>;
  ineligible: Record<WindowState, AgentListRequestPlan>;
}> = [
  {
    trigger: 'page-load',
    eligible: { small: 'fit', paged: 'fit', held: 'fit', capped: 'fit' },
    ineligible: { small: 'drain', paged: 'drain', held: 'drain', capped: 'drain' },
  },
  {
    trigger: 'label-commit',
    eligible: { small: 'fit', paged: 'fit', held: 'fit', capped: 'fit' },
    ineligible: { small: 'drain', paged: 'drain', held: 'drain', capped: 'drain' },
  },
  {
    trigger: 'lifecycle-refresh',
    eligible: { small: 'fit', paged: 'fit', held: 'none', capped: 'none' },
    ineligible: { small: 'drain', paged: 'drain', held: 'none', capped: 'none' },
  },
  {
    trigger: 'view-change',
    eligible: { small: 'none', paged: 'fit', held: 'none', capped: 'fit' },
    ineligible: { small: 'none', paged: 'drain', held: 'none', capped: 'none' },
  },
  {
    trigger: 'chip',
    eligible: { small: 'fit', paged: 'page', held: 'drain', capped: 'drain' },
    ineligible: { small: 'drain', paged: 'page', held: 'drain', capped: 'drain' },
  },
];

describe('AgentListWindow states — request planner table', () => {
  for (const row of PLAN_TABLE) {
    for (const state of STATES) {
      it(`${row.trigger} in ${state}: eligible -> ${row.eligible[state]}, ineligible -> ${row.ineligible[state]}`, () => {
        const { win, setHeld } = setup();
        enter(win, setHeld, state);
        // A view change that alters a server parameter (here the dir).
        if (row.trigger === 'view-change') win.setViewState({ sortDir: 'asc' });
        expect(win.planRequest(row.trigger, '')).toBe(row.eligible[state]);
        win.setViewState({ sortField: 'name' });
        expect(win.planRequest(row.trigger, '')).toBe(row.ineligible[state]);
      });
    }
  }
});

describe('AgentListWindow states — paged view changes', () => {
  const cases: Array<{
    name: string;
    vs: Partial<AgentListViewState>;
    plan: AgentListRequestPlan;
  }> = [
    { name: 'list -> grid', vs: { view: 'grid' }, plan: 'none' },
    { name: 'list -> grid -> list', vs: { view: 'list' }, plan: 'none' },
    { name: 'list -> tree', vs: { view: 'tree' }, plan: 'drain' },
    { name: 'dir flip', vs: { sortDir: 'asc' }, plan: 'fit' },
    { name: 'updated -> created', vs: { sortField: 'created' }, plan: 'fit' },
    { name: 'grid, updated -> created', vs: { view: 'grid', sortField: 'created' }, plan: 'fit' },
    { name: 'sort -> name', vs: { sortField: 'name' }, plan: 'drain' },
    { name: 'sort -> status', vs: { sortField: 'status' }, plan: 'drain' },
    { name: 'phase change', vs: { phaseFilter: 'running' }, plan: 'fit' },
    { name: 'page size change', vs: { pageSize: 50 }, plan: 'fit' },
    { name: 'label typing only', vs: { label: 'team=r' }, plan: 'none' },
  ];
  for (const c of cases) {
    it(`${c.name} -> ${c.plan}`, () => {
      const { win, setHeld } = setup();
      enter(win, setHeld, 'paged');
      win.setViewState(c.vs);
      expect(win.planRequest('view-change', '')).toBe(c.plan);
    });
  }

  it('a change and its reversal before any request sends nothing', () => {
    const { win, setHeld } = setup();
    enter(win, setHeld, 'paged');
    win.setViewState({ sortDir: 'asc' });
    win.setViewState({ sortDir: 'desc' });
    expect(win.planRequest('view-change', '')).toBe('none');
  });

  it('page navigation keeps the adopted parameters', async () => {
    const { win, setHeld, fetchPage } = setup();
    fetchPage.mockResolvedValue(pagedResult([agent('b')], { totalCount: 60 }));
    enter(win, setHeld, 'paged');
    await win.next();
    win.setViewState({ view: 'grid' });
    expect(win.planRequest('view-change', '')).toBe('none');
  });
});

describe('AgentListWindow states — sorted eligibility', () => {
  const cases: Array<{
    name: string;
    vs: Partial<AgentListViewState>;
    label: string;
    eligible: boolean;
  }> = [
    { name: 'list, updated, no label', vs: {}, label: '', eligible: true },
    { name: 'grid, updated, no label', vs: { view: 'grid' }, label: '', eligible: true },
    { name: 'list, created', vs: { sortField: 'created' }, label: '', eligible: true },
    {
      name: 'grid, created, asc',
      vs: { view: 'grid', sortField: 'created', sortDir: 'asc' },
      label: '',
      eligible: true,
    },
    { name: 'k=v label', vs: {}, label: 'team=red', eligible: true },
    { name: 'whitespace-only label', vs: {}, label: '   ', eligible: true },
    { name: 'bare-key label', vs: {}, label: 'team', eligible: false },
    { name: 'name sort', vs: { sortField: 'name' }, label: '', eligible: false },
    { name: 'status sort', vs: { sortField: 'status' }, label: '', eligible: false },
    { name: 'tree view', vs: { view: 'tree' }, label: '', eligible: false },
    {
      name: 'tree view, created',
      vs: { view: 'tree', sortField: 'created' },
      label: '',
      eligible: false,
    },
  ];
  for (const c of cases) {
    it(`${c.name} -> ${c.eligible ? 'sorted-eligible' : 'complete-needing'}`, () => {
      const { win } = setup(c.vs);
      expect(win.isSortedEligible(c.label)).toBe(c.eligible);
      expect(win.planRequest('page-load', c.label)).toBe(c.eligible ? 'fit' : 'drain');
    });
  }
});

describe('AgentListWindow states — transitions', () => {
  const ENTRIES: Array<{
    name: string;
    apply: (win: AgentListWindow, setHeld: (a: Agent[]) => void) => void;
    state: WindowState;
    reason: 'capped' | 'failed' | null;
  }> = [
    {
      name: 'setSmall',
      apply: (win, setHeld) => {
        setHeld([agent('a')]);
        win.setSmall();
      },
      state: 'small',
      reason: null,
    },
    {
      name: 'setPaged',
      apply: (win) => win.setPaged(pagedResult([agent('a')], { nextCursor: 'c' }), ''),
      state: 'paged',
      reason: null,
    },
    {
      name: 'a complete one-request drain',
      apply: (win, setHeld) => {
        setHeld([agent('a')]);
        win.adoptDrain({ complete: true, capped: false, error: null, requests: 1 });
      },
      state: 'small',
      reason: null,
    },
    {
      name: 'a complete multi-request drain',
      apply: (win, setHeld) => {
        setHeld([agent('a'), agent('b')]);
        win.adoptDrain({ complete: true, capped: false, error: null, requests: 3 });
      },
      state: 'held',
      reason: null,
    },
    {
      name: 'a drain that hit its request cap',
      apply: (win, setHeld) => {
        setHeld([agent('a')]);
        win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
      },
      state: 'capped',
      reason: 'capped',
    },
    {
      name: 'a drain whose later page failed',
      apply: (win, setHeld) => {
        setHeld([agent('a')]);
        win.adoptDrain({
          complete: false,
          capped: false,
          error: { message: 'HTTP 500', status: 500 },
          requests: 2,
        });
      },
      state: 'capped',
      reason: 'failed',
    },
  ];

  for (const from of STATES) {
    for (const entry of ENTRIES) {
      it(`${from} -> ${entry.name} -> ${entry.state}`, () => {
        const { win, setHeld } = setup({ pageSize: 1 });
        enter(win, setHeld, from);
        win.markResync(); // set the per-state signal, which every transition clears
        const onChange = vi.fn();
        win.addEventListener('change', onChange);
        entry.apply(win, setHeld);
        expect(win.state).toBe(entry.state);
        expect(win.incompleteReason).toBe(entry.reason);
        expect(win.isLocal).toBe(entry.state !== 'paged');
        expect(win.pageIndex).toBe(0);
        expect(win.stale).toBe(false);
        expect(win.updatesAvailable).toBe(false);
        expect(win.error).toBeNull();
        expect(win.loading).toBe(false);
        expect(win.stats.incomplete).toBe(entry.state === 'capped');
        expect(onChange).toHaveBeenCalled();
      });
    }
  }

  it('the local states keep pageIndex across live updates of H (no re-adoption)', async () => {
    const { win, setHeld } = setup({ pageSize: 1 });
    setHeld([agent('a'), agent('b'), agent('c')]);
    win.adoptDrain({ complete: true, capped: false, error: null, requests: 2 });
    await win.next();
    expect(win.pageIndex).toBe(1);
    setHeld([agent('a'), agent('b'), agent('c'), agent('d')]);
    expect(win.pageIndex).toBe(1);
    expect(win.total).toBe(4);
  });

  it('a stale drain raises the stale banner on adoption', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a'), agent('b')]);
    win.adoptDrain({ complete: true, capped: false, error: null, requests: 2, stale: true });
    expect(win.state).toBe('held');
    expect(win.stale).toBe(true);
    expect(win.banner).toEqual({ kind: 'stale', text: 'may be stale' });
  });

  it('never treats an incomplete drain as complete, even when it reports complete with an error', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a')]);
    win.adoptDrain({ complete: true, capped: false, error: { message: 'x' }, requests: 2 });
    expect(win.state).toBe('capped');
    expect(win.incompleteReason).toBe('failed');
  });
});

describe('AgentListWindow states — sorted-mode refusal per committed label', () => {
  it('a 422 for a label stops sorted requests for that label only; a new label gets one retry', () => {
    const { win, setHeld } = setup();
    expect(win.planRequest('page-load', 'team=red')).toBe('fit');

    // The server refuses sorted mode for team=red; the host drains instead.
    win.recordRefusal('team=red');
    expect(win.isSortedRefused('team=red')).toBe(true);
    expect(win.isSortedRefused(' team=red ')).toBe(true);
    expect(win.planRequest('label-commit', 'team=red')).toBe('drain');
    setHeld([agent('a'), agent('b')]);
    win.adoptDrain({ complete: true, capped: false, error: null, requests: 2 });
    for (const trigger of ['label-commit', 'page-load', 'chip'] as const) {
      expect(win.planRequest(trigger, 'team=red')).toBe('drain');
    }

    // A different committed label gets one sorted attempt of its own.
    expect(win.isSortedRefused('team=blue')).toBe(false);
    expect(win.planRequest('label-commit', 'team=blue')).toBe('fit');
    win.recordRefusal('team=blue');
    expect(win.planRequest('label-commit', 'team=blue')).toBe('drain');

    // The earlier label is eligible again once another label was refused.
    expect(win.planRequest('label-commit', 'team=red')).toBe('fit');
  });

  it('an empty label can be refused too, and clearing back to it after a different label retries once', () => {
    const { win } = setup();
    win.recordRefusal('');
    expect(win.planRequest('page-load', '')).toBe('drain');
    win.recordRefusal('a=b');
    expect(win.planRequest('page-load', '')).toBe('fit');
  });
});

describe('AgentListWindow states — capped', () => {
  it('2,001 candidates with 700 readable: "700 loaded (newest 2,000 checked), more exist"', () => {
    const { win, setHeld } = setup();
    const readable = Array.from({ length: 700 }, (_, i) => agent(`a${i}`));
    setHeld(readable);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    expect(win.total).toEqual({ loaded: 700, capped: true });
    expect(win.banner).toEqual({
      kind: 'capped',
      text: '700 loaded (newest 2,000 checked), more exist',
    });
    expect(win.stats).toEqual({ total: 700, running: 700, incomplete: true });
  });

  it('formats the loaded count with thousands separators', () => {
    expect(cappedTotalText(1999)).toBe('1,999 loaded (newest 2,000 checked), more exist');
    expect(cappedTotalText(0)).toBe('0 loaded (newest 2,000 checked), more exist');
  });

  it('a failed drain reports "Incomplete: loaded X" and a plain number total', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a'), agent('b'), agent('c')]);
    win.adoptDrain({ complete: false, capped: false, error: { message: 'x' }, requests: 2 });
    expect(win.banner).toEqual({ kind: 'failed', text: 'Incomplete: loaded 3' });
    expect(win.total).toBe(3);
  });

  it('the capped banner wins over the stale banner', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a')]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4, stale: true });
    expect(win.banner?.kind).toBe('capped');
  });

  it('sorts H locally on any view change and sends nothing while sorted mode is unavailable', () => {
    const { win, setHeld, fetchPage } = setup({ sortField: 'name', sortDir: 'asc' });
    setHeld([agent('b', { name: 'B' }), agent('a', { name: 'A' })]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
    win.setViewState({ sortDir: 'desc' });
    expect(win.planRequest('view-change', '')).toBe('none');
    expect(win.items.map((a) => a.id)).toEqual(['b', 'a']);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('returns to paged on a view change to a sorted-eligible state', () => {
    const { win, setHeld } = setup({ sortField: 'name' });
    setHeld([agent('a')]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    win.setViewState({ sortField: 'updated' });
    expect(win.planRequest('view-change', '')).toBe('fit');
    win.setPaged(pagedResult([agent('a')], { nextCursor: 'c', totalCount: 3000 }), '');
    expect(win.state).toBe('paged');
    expect(win.incompleteReason).toBeNull();
    expect(win.total).toBe(3000);
    expect(win.banner).toBeNull();
  });

  it('stays capped (local sort) on a view change to a sorted state when the label was refused', () => {
    const { win, setHeld } = setup({ sortField: 'name' });
    win.recordRefusal('team=red');
    setHeld([
      agent('a', { updated: '2026-01-01T00:00:00Z' }),
      agent('b', { updated: '2026-01-02T00:00:00Z' }),
    ]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    win.setViewState({ sortField: 'updated' });
    expect(win.planRequest('view-change', 'team=red')).toBe('none');
    expect(win.state).toBe('capped');
    expect(win.items.map((a) => a.id)).toEqual(['b', 'a']);
  });

  it('leaves capped only by a new first request: live H updates keep it capped', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a')]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    setHeld([agent('a'), agent('b')]);
    win.setViewState({ phaseFilter: 'running' });
    win.markResync();
    expect(win.state).toBe('capped');
    expect(win.total).toEqual({ loaded: 2, capped: true });
  });
});

describe('AgentListWindow states — held', () => {
  it('view changes in held are local: grid, list, tree, every sort, no request', () => {
    const { win, setHeld, fetchPage } = setup();
    setHeld([agent('b', { name: 'B' }), agent('a', { name: 'A' }), agent('c', { name: 'C' })]);
    win.adoptDrain({ complete: true, capped: false, error: null, requests: 3 });
    for (const vs of [
      { view: 'grid' as const },
      { view: 'tree' as const },
      { sortField: 'name' as const, sortDir: 'asc' as const },
      { sortField: 'created' as const },
      { view: 'list' as const, sortField: 'updated' as const },
    ]) {
      win.setViewState(vs);
      expect(win.planRequest('view-change', '')).toBe('none');
      expect(win.state).toBe('held');
    }
    win.setViewState({ sortField: 'name', sortDir: 'asc' });
    expect(win.display.map((a) => a.id)).toEqual(['a', 'b', 'c']);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('the total, stats and banner come from H', () => {
    const { win, setHeld } = setup({ pageSize: 2 });
    setHeld([agent('a'), agent('b', { phase: 'stopped' }), agent('c')]);
    win.adoptDrain({ complete: true, capped: false, error: null, requests: 2 });
    expect(win.total).toBe(3);
    expect(win.stats).toEqual({ total: 3, running: 2, incomplete: false });
    expect(win.banner).toBeNull();
    expect(win.items).toHaveLength(2);
    expect(win.hasNext).toBe(true);
  });
});

describe('AgentListWindow states — lifecycle refresh', () => {
  const cases: Array<{ state: WindowState; requests: number }> = [
    { state: 'small', requests: 1 },
    { state: 'paged', requests: 1 },
    { state: 'held', requests: 0 },
    { state: 'capped', requests: 0 },
  ];
  for (const c of cases) {
    it(`${c.state}: ${c.requests} request(s), for eligible and ineligible view states`, () => {
      const { win, setHeld } = setup();
      enter(win, setHeld, c.state);
      for (const sortField of ['updated', 'name'] as const) {
        win.setViewState({ sortField });
        const plan = win.planRequest('lifecycle-refresh', '');
        expect(plan === 'none' ? 0 : 1).toBe(c.requests);
      }
    });
  }
});

describe('AgentListWindow states — resync signal', () => {
  for (const state of STATES) {
    it(`${state}: markResync raises ${state === 'paged' ? 'the chip' : 'the stale banner'} and sends nothing`, () => {
      const { win, setHeld, fetchPage } = setup();
      enter(win, setHeld, state);
      win.markResync();
      expect(win.updatesAvailable).toBe(state === 'paged');
      expect(win.stale).toBe(state !== 'paged');
      if (state === 'small' || state === 'held') {
        expect(win.banner).toEqual({ kind: 'stale', text: 'may be stale' });
      }
      if (state === 'paged') expect(win.banner).toBeNull();
      expect(fetchPage).not.toHaveBeenCalled();
    });
  }
});

describe('AgentListWindow states — created sort while paged', () => {
  it('re-sorts an on-page update by created time, not by updated time', () => {
    const rows = [
      agent('new', { created: '2026-01-03T00:00:00Z', updated: '2026-01-03T00:00:00Z' }),
      agent('mid', { created: '2026-01-02T00:00:00Z', updated: '2026-01-02T00:00:00Z' }),
    ];
    const live = new Map(rows.map((a) => [a.id, a]));
    let held: Agent[] = [];
    const win = new AgentListWindow({
      viewState: viewState({ sortField: 'created' }),
      fetchPage: vi.fn(),
      getAgent: (id) => live.get(id),
      getProjectId: () => 'p-1',
      getHeldAgents: () => held,
    });
    held = [];
    win.setPaged(pagedResult(rows, { totalCount: 2 }), '');
    // A fresh activity on the older agent would move it first under the
    // updated sort; under the created sort it stays where it is.
    live.set('mid', { ...rows[1], updated: '2026-02-01T00:00:00Z' });
    win.applyChanges({ upserted: ['mid'], deleted: [], unknown: new Map(), generation: 1 });
    expect(win.items.map((a) => a.id)).toEqual(['new', 'mid']);
    expect(win.updatesAvailable).toBe(false);
  });

  it('an off-page member whose activity time enters the page range raises no chip under the created sort', () => {
    const rows = [
      agent('a', { created: '2026-01-03T00:00:00Z' }),
      agent('b', { created: '2026-01-02T00:00:00Z' }),
    ];
    const win = new AgentListWindow({
      viewState: viewState({ sortField: 'created' }),
      fetchPage: vi.fn(),
      getAgent: () => undefined,
      getProjectId: () => 'p-1',
      getHeldAgents: () => [],
    });
    win.setPaged(
      pagedResult(rows, {
        totalCount: 3,
        nextCursor: 'c',
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
    win.applyChanges({
      upserted: [],
      deleted: [],
      unknown: new Map([['c', { lastActivityEvent: '2026-01-02T12:00:00Z' }]]),
      generation: 1,
    });
    expect(win.updatesAvailable).toBe(false);
  });
});

describe('AgentListWindow states — optimistic local update', () => {
  it('paged: replaces the on-page row and the member phase, with no chip and no request', () => {
    const { win, fetchPage } = setup();
    win.setPaged(pagedResult([agent('a'), agent('b')], { totalCount: 2 }), '');
    win.applyLocalUpdate([agent('a', { phase: 'stopping' }), agent('zz', { phase: 'stopping' })]);
    expect(win.items.map((a) => [a.id, a.phase])).toEqual([
      ['a', 'stopping'],
      ['b', 'running'],
    ]);
    expect(win.memberIndex.getPhase('a')).toBe('stopping');
    expect(win.memberIndex.has('zz')).toBe(false);
    expect(win.updatesAvailable).toBe(false);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('is a no-op in the local states (the host merges into H)', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a')]);
    win.setSmall();
    const onChange = vi.fn();
    win.addEventListener('change', onChange);
    win.applyLocalUpdate([agent('a', { phase: 'stopping' })]);
    expect(onChange).not.toHaveBeenCalled();
    expect(win.items[0].phase).toBe('running');
  });
});
