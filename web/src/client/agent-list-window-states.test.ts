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
import {
  AgentListWindow,
  cappedTotalText,
  failedTotalText,
  incompleteStatNote,
} from './agent-list-window.js';
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

  /** Puts a fresh window into `state` on page 1 (page size 1, two rows). */
  async function enterAtPage1(
    win: AgentListWindow,
    setHeld: (a: Agent[]) => void,
    fetchPage: ReturnType<typeof vi.fn>,
    state: WindowState
  ): Promise<void> {
    if (state === 'paged') {
      fetchPage.mockResolvedValue(pagedResult([agent('b')], { nextCursor: 'c2', totalCount: 60 }));
      win.setPaged(pagedResult([agent('a')], { nextCursor: 'c1', totalCount: 60 }), '');
    } else {
      setHeld([agent('a'), agent('b')]);
      if (state === 'small') win.setSmall();
      else if (state === 'held') {
        win.adoptDrain({ complete: true, capped: false, error: null, requests: 2 });
      } else {
        win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
      }
    }
    await win.next();
    expect(win.state).toBe(state);
    expect(win.pageIndex).toBe(1);
  }

  for (const from of STATES) {
    for (const entry of ENTRIES) {
      // Leaving paged, or entering it, swaps in a different data set and
      // lands on page 0. A local-to-local adoption keeps the page (a later
      // trigger while already local must not throw the user back to page 0;
      // only a view-state change does).
      const resets = from === 'paged' || entry.state === 'paged';
      it(`${from} on page 1 -> ${entry.name} -> ${entry.state} on page ${resets ? 0 : 1}`, async () => {
        const { win, setHeld, fetchPage } = setup({ pageSize: 1 });
        await enterAtPage1(win, setHeld, fetchPage, from);
        entry.apply(win, setHeld);
        if (entry.state === 'small' || entry.state === 'held' || entry.state === 'capped') {
          // Two rows at page size 1, so page 1 exists in the adopted set too.
          setHeld([agent('a'), agent('b')]);
        }
        expect(win.state).toBe(entry.state);
        expect(win.pageIndex).toBe(resets ? 0 : 1);
        expect(win.rangeStart).toBe(resets ? 0 : 1);
        if (entry.state === 'paged') {
          // The adopted page is page 0, with no cursor behind it.
          expect(win.hasPrev).toBe(false);
          expect(win.items.map((a) => a.id)).toEqual(['a']);
        }
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

  it('the incomplete stat note follows the reason: capped names the 2,000 checked, failed does not', () => {
    expect(failedTotalText(1500)).toBe('Incomplete: loaded 1,500');
    expect(incompleteStatNote('capped', 'total')).toBe('loaded (newest 2,000 checked), more exist');
    expect(incompleteStatNote('capped', 'running')).toBe(
      'among loaded (newest 2,000 checked), more exist'
    );
    expect(incompleteStatNote('failed', 'total')).toBe('loaded, incomplete');
    expect(incompleteStatNote('failed', 'running')).toBe('among loaded, incomplete');
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
  /**
   * Requests each plan really sends: a fit request or a page refetch is one
   * request; a drain is its legacy first page plus up to three continuation
   * pages (the four-request cap); none is zero.
   */
  const REQUESTS: Record<AgentListRequestPlan, { min: number; max: number }> = {
    fit: { min: 1, max: 1 },
    page: { min: 1, max: 1 },
    drain: { min: 1, max: 4 },
    none: { min: 0, max: 0 },
  };
  const cases: Array<{
    state: WindowState;
    eligible: AgentListRequestPlan;
    ineligible: AgentListRequestPlan;
  }> = [
    { state: 'small', eligible: 'fit', ineligible: 'drain' },
    { state: 'paged', eligible: 'fit', ineligible: 'drain' },
    { state: 'held', eligible: 'none', ineligible: 'none' },
    { state: 'capped', eligible: 'none', ineligible: 'none' },
  ];
  for (const c of cases) {
    const describeCost = (plan: AgentListRequestPlan) => {
      const { min, max } = REQUESTS[plan];
      return min === max ? `${min}` : `${min} to ${max}`;
    };
    it(`${c.state}: updated sort plans ${c.eligible} (${describeCost(c.eligible)} requests), name sort plans ${c.ineligible} (${describeCost(c.ineligible)} requests)`, () => {
      const { win, setHeld } = setup();
      enter(win, setHeld, c.state);
      win.setViewState({ sortField: 'updated' });
      expect(win.planRequest('lifecycle-refresh', '')).toBe(c.eligible);
      win.setViewState({ sortField: 'name' });
      expect(win.planRequest('lifecycle-refresh', '')).toBe(c.ineligible);
    });
  }

  it('the request cost of each plan matches the requests the hosts send for it', () => {
    expect(REQUESTS.none).toEqual({ min: 0, max: 0 });
    expect(REQUESTS.fit).toEqual({ min: 1, max: 1 });
    expect(REQUESTS.page).toEqual({ min: 1, max: 1 });
    // The drain cap: a legacy first page plus three continuation pages.
    expect(REQUESTS.drain).toEqual({ min: 1, max: 4 });
  });
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

describe('AgentListWindow states — undecidable membership change', () => {
  const EXPECTED: Record<WindowState, { chip: boolean; stale: boolean; banner: string | null }> = {
    small: { chip: false, stale: false, banner: null },
    paged: { chip: true, stale: false, banner: null },
    held: { chip: false, stale: true, banner: 'stale' },
    // The capped banner still wins over the stale one.
    capped: { chip: false, stale: true, banner: 'capped' },
  };
  for (const state of STATES) {
    const e = EXPECTED[state];
    it(`${state}: markMembershipChanged ${e.chip ? 'raises the chip' : e.stale ? 'raises the stale flag' : 'changes nothing'} and sends nothing`, () => {
      const { win, setHeld, fetchPage } = setup();
      enter(win, setHeld, state);
      const changes = vi.fn();
      win.addEventListener('change', changes);
      win.markMembershipChanged();
      expect(win.updatesAvailable).toBe(e.chip);
      expect(win.stale).toBe(e.stale);
      expect(win.banner?.kind ?? null).toBe(e.banner);
      expect(changes).toHaveBeenCalledTimes(state === 'small' ? 0 : 1);
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

describe('AgentListWindow states — off-page activity from an unknown delta', () => {
  /** Paged under the updated sort with members a, b on the page and c, d off it. */
  function pagedWithOffPageMembers(sortDir: 'asc' | 'desc' = 'desc') {
    const rows =
      sortDir === 'desc'
        ? [
            agent('a', { updated: '2026-01-04T00:00:00Z' }),
            agent('b', { updated: '2026-01-03T00:00:00Z' }),
          ]
        : [
            agent('a', { updated: '2026-01-01T00:00:00Z' }),
            agent('b', { updated: '2026-01-02T00:00:00Z' }),
          ];
    const fetchPage = vi.fn(async () => pagedResult([]));
    const win = new AgentListWindow({
      viewState: viewState({ sortDir }),
      fetchPage,
      getAgent: () => undefined,
      getProjectId: () => 'p-1',
      getHeldAgents: () => [],
    });
    win.setPaged(
      pagedResult(rows, {
        totalCount: 4,
        nextCursor: 'c',
        stats: {
          total: 4,
          running: 4,
          agents: [
            ['a', 'running'],
            ['b', 'running'],
            ['c', 'running'],
            ['d', 'running'],
          ],
        },
      }),
      ''
    );
    return { win, fetchPage };
  }

  const activity = (id: string, lastActivityEvent: string) => ({
    upserted: [],
    deleted: [],
    unknown: new Map([[id, { lastActivityEvent }]]),
    generation: 1,
  });

  it('an off-page activity bump from an unknown delta raises the chip on page 0', () => {
    const { win, fetchPage } = pagedWithOffPageMembers();
    const onChange = vi.fn();
    win.addEventListener('change', onChange);
    win.applyChanges(activity('c', '2026-01-05T00:00:00Z'));
    expect(win.updatesAvailable).toBe(true);
    expect(onChange).toHaveBeenCalled();
    // The page itself is untouched until the chip is clicked.
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('an ascending sort raises the chip for an off-page activity time at or before the first row', () => {
    const { win, fetchPage } = pagedWithOffPageMembers('asc');
    win.applyChanges(activity('c', '2025-12-31T00:00:00Z'));
    expect(win.updatesAvailable).toBe(true);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('an off-page activity time that stays below the first row of page 0 raises no chip', () => {
    const { win, fetchPage } = pagedWithOffPageMembers();
    win.applyChanges(activity('c', '2026-01-02T00:00:00Z'));
    expect(win.updatesAvailable).toBe(false);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('an activity delta for an agent that is neither on the page nor a member is ignored', () => {
    const { win } = pagedWithOffPageMembers();
    win.applyChanges(activity('stranger', '2026-01-05T00:00:00Z'));
    expect(win.updatesAvailable).toBe(false);
    expect(win.stats.total).toBe(4);
  });
});

describe('AgentListWindow states — small-state live reorder across a local page boundary', () => {
  it('a row bumped from page 2 to page 1 moves pages with no page reset and no request', async () => {
    const { win, setHeld, fetchPage } = setup({ pageSize: 2 });
    const rows = [
      agent('a', { updated: '2026-01-05T00:00:00Z' }),
      agent('b', { updated: '2026-01-04T00:00:00Z' }),
      agent('c', { updated: '2026-01-03T00:00:00Z' }),
      agent('d', { updated: '2026-01-02T00:00:00Z' }),
    ];
    setHeld(rows);
    win.setSmall();
    await win.next();
    expect(win.pageIndex).toBe(1);
    expect(win.items.map((a) => a.id)).toEqual(['c', 'd']);

    // A live update makes d the most recently active agent: the host
    // replaces H, and the window reads it fresh.
    setHeld([...rows.slice(0, 3), agent('d', { updated: '2026-01-09T00:00:00Z' })]);
    expect(win.pageIndex).toBe(1);
    expect(win.items.map((a) => a.id)).toEqual(['b', 'c']);
    await win.prev();
    expect(win.items.map((a) => a.id)).toEqual(['d', 'a']);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('a row that falls from page 1 to page 2 leaves page 1 and appears on page 2', async () => {
    const { win, setHeld, fetchPage } = setup({ pageSize: 2 });
    const rows = [
      agent('a', { updated: '2026-01-05T00:00:00Z' }),
      agent('b', { updated: '2026-01-04T00:00:00Z' }),
      agent('c', { updated: '2026-01-03T00:00:00Z' }),
      agent('d', { updated: '2026-01-02T00:00:00Z' }),
    ];
    setHeld(rows);
    win.setSmall();
    expect(win.items.map((a) => a.id)).toEqual(['a', 'b']);
    // Ascending by updated time instead: a, now the oldest, sorts last.
    win.setViewState({ sortDir: 'asc' });
    expect(win.items.map((a) => a.id)).toEqual(['d', 'c']);
    setHeld([agent('d', { updated: '2026-01-08T00:00:00Z' }), ...rows.slice(0, 3)]);
    expect(win.pageIndex).toBe(0);
    expect(win.items.map((a) => a.id)).toEqual(['c', 'b']);
    await win.next();
    expect(win.items.map((a) => a.id)).toEqual(['a', 'd']);
    expect(fetchPage).not.toHaveBeenCalled();
  });
});

describe('AgentListWindow states — optimistic local update', () => {
  it('paged: replaces the on-page row and the member phase, with no chip and no request', () => {
    const { win, fetchPage } = setup();
    win.setPaged(
      pagedResult([agent('a', { updated: '2026-01-02T00:00:00Z' }), agent('b')], {
        totalCount: 2,
      }),
      ''
    );
    win.applyLocalUpdate([
      agent('a', { phase: 'stopping', updated: '2026-01-02T00:00:00Z' }),
      agent('zz', { phase: 'stopping' }),
    ]);
    expect(win.items.map((a) => [a.id, a.phase])).toEqual([
      ['a', 'stopping'],
      ['b', 'running'],
    ]);
    expect(win.memberIndex.getPhase('a')).toBe('stopping');
    expect(win.memberIndex.has('zz')).toBe(false);
    expect(win.updatesAvailable).toBe(false);
    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('paged: re-sorts the page locally after replacing a row, as a live on-page upsert does', () => {
    const { win, fetchPage } = setup();
    win.setPaged(
      pagedResult(
        [
          agent('a', { updated: '2026-01-03T00:00:00Z' }),
          agent('b', { updated: '2026-01-02T00:00:00Z' }),
          agent('c', { updated: '2026-01-01T00:00:00Z' }),
        ],
        { totalCount: 3 }
      ),
      ''
    );
    const onChange = vi.fn();
    win.addEventListener('change', onChange);
    // The patched row's activity time moves it to the top of the desc page.
    win.applyLocalUpdate([agent('c', { phase: 'stopping', updated: '2026-01-04T00:00:00Z' })]);
    expect(win.items.map((a) => a.id)).toEqual(['c', 'a', 'b']);
    expect(win.items[0].phase).toBe('stopping');
    expect(onChange).toHaveBeenCalledTimes(1);
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

describe('AgentListWindow states — live changes during a paged request', () => {
  function setupWithStore(
    store: Map<string, Agent>,
    fetchPage = vi.fn(async () => pagedResult([]))
  ) {
    const win = new AgentListWindow({
      viewState: viewState(),
      fetchPage,
      getAgent: (id) => store.get(id),
      getProjectId: () => 'p-1',
      getHeldAgents: () => [],
    });
    return { win, fetchPage };
  }

  it('setPaged replays a live create the response predates into the member index, with the chip on page 0', () => {
    const created = agent('new', { updated: '2026-02-01T00:00:00Z' });
    const store = new Map([[created.id, created]]);
    const { win } = setupWithStore(store);
    win.setPaged(
      pagedResult([agent('a'), agent('b')], { totalCount: 2, liveChanged: ['new'] }),
      ''
    );
    expect(win.memberIndex.has('new')).toBe(true);
    expect(win.stats.total).toBe(3);
    expect(win.updatesAvailable).toBe(true);
  });

  it('setPaged ignores a replayed create outside the add rule', () => {
    const foreign = agent('other', { projectId: 'p-2' });
    const store = new Map([[foreign.id, foreign]]);
    const { win } = setupWithStore(store);
    win.setPaged(pagedResult([agent('a')], { totalCount: 1, liveChanged: ['other'] }), '');
    expect(win.memberIndex.has('other')).toBe(false);
    expect(win.updatesAvailable).toBe(false);
  });

  it('a page fetch replays a live create into the member index after its stats seed', async () => {
    const created = agent('new', { updated: '2026-02-01T00:00:00Z' });
    const store = new Map([[created.id, created]]);
    const fetchPage = vi.fn(async () =>
      pagedResult([agent('a')], { totalCount: 1, liveChanged: ['new'] })
    );
    const { win } = setupWithStore(store, fetchPage);
    win.setPaged(pagedResult([agent('a')], { totalCount: 1 }), '');
    await win.refresh();
    expect(fetchPage).toHaveBeenCalledTimes(1);
    expect(win.memberIndex.has('new')).toBe(true);
    expect(win.updatesAvailable).toBe(true);
  });
});

/** A window whose page fetches are held until the test resolves them. */
function setupHeldFetch(vs: Partial<AgentListViewState> = {}) {
  let held: Agent[] = [];
  const calls: Array<{ signal: AbortSignal; resolve: (r: PagedPageResult) => void }> = [];
  const fetchPage = vi.fn(
    (params: { signal?: AbortSignal }) =>
      new Promise<PagedPageResult>((resolve) => {
        calls.push({ signal: params.signal!, resolve });
      })
  );
  const win = new AgentListWindow({
    viewState: viewState(vs),
    fetchPage,
    getAgent: () => undefined,
    getProjectId: () => 'p-1',
    getHeldAgents: () => held,
  });
  win.setPaged(pagedResult([agent('a')], { nextCursor: 'c1', totalCount: 60 }), '');
  const setHeld = (agents: Agent[]): void => {
    held = agents;
  };
  return { win, calls, setHeld };
}

describe('AgentListWindow — page fetch abort', () => {
  const STOPS: Array<{
    name: string;
    stop: (win: AgentListWindow, setHeld: (a: Agent[]) => void) => void;
    state: WindowState;
  }> = [
    {
      name: 'beginSortedRequest',
      stop: (win) => void win.beginSortedRequest('view-change', ''),
      state: 'paged',
    },
    { name: 'cancelPageFetch', stop: (win) => win.cancelPageFetch(), state: 'paged' },
    {
      name: 'setSmall',
      stop: (win, setHeld) => {
        setHeld([agent('s')]);
        win.setSmall();
      },
      state: 'small',
    },
    {
      name: 'setPaged',
      stop: (win) => win.setPaged(pagedResult([agent('p')], { totalCount: 1 }), ''),
      state: 'paged',
    },
  ];

  for (const { name, stop, state } of STOPS) {
    it(`Next then ${name} aborts the page fetch and drops its late result`, async () => {
      const { win, calls, setHeld } = setupHeldFetch();
      const pending = win.next();
      expect(calls).toHaveLength(1);
      expect(win.loading).toBe(true);
      const before = win.items.map((a) => a.id);
      stop(win, setHeld);
      expect(calls[0].signal.aborted).toBe(true);
      expect(win.loading).toBe(false);
      const afterStop = win.items.map((a) => a.id);
      if (name !== 'setSmall' && name !== 'setPaged') expect(afterStop).toEqual(before);
      calls[0].resolve(pagedResult([agent('late')], { nextCursor: 'c2', totalCount: 60 }));
      await pending;
      expect(win.state).toBe(state);
      expect(win.pageIndex).toBe(0);
      expect(win.items.map((a) => a.id)).toEqual(afterStop);
    });
  }

  it('a second Next aborts the first, and only the second result is shown', async () => {
    const { win, calls } = setupHeldFetch();
    const first = win.next();
    const second = win.next();
    expect(calls).toHaveLength(2);
    expect(calls[0].signal.aborted).toBe(true);
    expect(calls[1].signal.aborted).toBe(false);
    calls[1].resolve(pagedResult([agent('second')], { totalCount: 60 }));
    calls[0].resolve(pagedResult([agent('first')], { totalCount: 60 }));
    await Promise.all([first, second]);
    expect(win.pageIndex).toBe(1);
    expect(win.items.map((a) => a.id)).toEqual(['second']);
  });
});

describe('AgentListWindow — sorted request tickets', () => {
  it('supersededRequest is null with no ticket, and for a grid and list switch', () => {
    const { win } = setup();
    expect(win.supersededRequest('')).toBeNull();
    win.beginSortedRequest('page-load', '');
    expect(win.supersededRequest('')).toBeNull();
    win.setViewState({ view: 'grid' });
    expect(win.supersededRequest('')).toBeNull();
  });

  const CHANGES: Array<{ name: string; change: Partial<AgentListViewState>; label?: string }> = [
    { name: 'sort', change: { sortField: 'created' } },
    { name: 'dir', change: { sortDir: 'asc' } },
    { name: 'phase', change: { phaseFilter: 'stopped' } },
    { name: 'page size', change: { pageSize: 50 } },
    { name: 'label', change: {}, label: 'env=prod' },
    { name: 'eligibility (tree view)', change: { view: 'tree' } },
    { name: 'eligibility (name sort)', change: { sortField: 'name' } },
  ];
  for (const { name, change, label } of CHANGES) {
    it(`a ${name} change supersedes the ticket's trigger`, () => {
      const { win } = setup();
      win.beginSortedRequest('label-commit', '');
      win.setViewState(change);
      expect(win.supersededRequest(label ?? '')).toBe('label-commit');
    });
  }

  it('supersededRequest is null after endSortedRequest', () => {
    const { win } = setup();
    const ticket = win.beginSortedRequest('page-load', '');
    win.endSortedRequest(ticket);
    win.setViewState({ sortDir: 'asc' });
    expect(win.supersededRequest('')).toBeNull();

    win.beginSortedRequest('page-load', '');
    win.endSortedRequest();
    expect(win.supersededRequest('env=prod')).toBeNull();
  });

  it("a stale ticket's end does not clear a newer ticket", () => {
    const { win } = setup();
    const older = win.beginSortedRequest('page-load', '');
    win.beginSortedRequest('view-change', '');
    win.endSortedRequest(older);
    win.setViewState({ sortDir: 'asc' });
    expect(win.supersededRequest('')).toBe('view-change');
  });

  it('setPaged under a ticket key that differs from the current view state plans a fit on a view change', () => {
    const { win } = setup();
    const ticket = win.beginSortedRequest('page-load', '');
    win.setViewState({ sortDir: 'asc' });
    win.setPaged(pagedResult([agent('a')], { nextCursor: 'c1', totalCount: 60 }), '', ticket.key);
    expect(win.planRequest('view-change', '')).toBe('fit');
    // The same page recorded under the current view state needs nothing.
    win.setPaged(pagedResult([agent('a')], { nextCursor: 'c1', totalCount: 60 }), '');
    expect(win.planRequest('view-change', '')).toBe('none');
  });
});
