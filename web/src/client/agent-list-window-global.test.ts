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
 * The window options the global agents page uses: the mode filter, the
 * scope add rule, the count-only member index above 2,000 agents, and the
 * chip for a create outside the add rule.
 */

import { describe, it, expect, vi } from 'vitest';
import type { Agent, AgentPhase } from '../shared/types.js';
import { getAgentDisplayStatus } from '../shared/types.js';
import { AgentListWindow } from './agent-list-window.js';
import type { AgentListViewState, PagedPageParams, PagedPageResult } from './agent-list-window.js';
import { AgentMemberIndex } from './agent-member-index.js';
import type { AgentSortField, SortDir } from '../shared/agent-sort.js';

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

function setup(
  vs: Partial<AgentListViewState> = {},
  opts: {
    isAddable?: (a: Agent) => boolean;
    agents?: Map<string, Agent>;
    fetchPage?: (p: PagedPageParams) => Promise<PagedPageResult>;
  } = {}
) {
  let held: Agent[] = [];
  const agents = opts.agents ?? new Map<string, Agent>();
  const fetchPage = vi.fn(
    opts.fetchPage ?? (async (): Promise<PagedPageResult> => ({ agents: [], totalCount: 0 }))
  );
  const win = new AgentListWindow({
    viewState: viewState(vs),
    fetchPage,
    getAgent: (id) => agents.get(id),
    getProjectId: () => '',
    getHeldAgents: () => held,
    ...(opts.isAddable ? { isAddable: opts.isAddable } : {}),
  });
  return {
    win,
    fetchPage,
    agents,
    setHeld: (a: Agent[]) => {
      held = a;
    },
  };
}

function flushOf(upserted: string[], deleted: string[] = []) {
  return { upserted, deleted, unknown: new Map(), generation: 1 };
}

/** The global page's display filter and sort before the window rendered it, kept as the reference. */
function referenceDisplay(
  agents: Agent[],
  f: {
    phase: AgentPhase | '';
    mode: string;
    label: string;
    sortField: AgentSortField;
    sortDir: SortDir;
  }
): Agent[] {
  let list = agents;
  if (f.phase) list = list.filter((a) => a.phase === f.phase);
  if (f.mode) {
    if (f.mode === 'can_message') list = list.filter((a) => a._messageability?.canMessage === true);
    else if (f.mode === 'cannot_message')
      list = list.filter((a) => a._messageability?.canMessage === false);
    else list = list.filter((a) => (a.messageMode || 'project') === f.mode);
  }
  if (f.label.trim()) {
    const parts = f.label.trim().split('=');
    const key = parts[0];
    const value = parts.slice(1).join('=');
    list = list.filter((a) => {
      if (!a.labels) return false;
      if (value) return a.labels[key] === value;
      return key in a.labels;
    });
  }
  const sorted = [...list];
  sorted.sort((a, b) => {
    let cmp = 0;
    switch (f.sortField) {
      case 'name':
        cmp = (a.name || '').localeCompare(b.name || '');
        break;
      case 'status':
        cmp = getAgentDisplayStatus(a).localeCompare(getAgentDisplayStatus(b));
        break;
      case 'created':
        cmp = (a.created || '').localeCompare(b.created || '');
        break;
      case 'updated':
        cmp = (
          a.lastActivityEvent && !a.lastActivityEvent.startsWith('0001')
            ? a.lastActivityEvent
            : a.updated || ''
        ).localeCompare(
          b.lastActivityEvent && !b.lastActivityEvent.startsWith('0001')
            ? b.lastActivityEvent
            : b.updated || ''
        );
        break;
    }
    return f.sortDir === 'asc' ? cmp : -cmp;
  });
  return sorted;
}

describe('AgentListWindow — mode filter', () => {
  it('a mode filter makes every view state complete-needing', () => {
    const { win } = setup({ modeFilter: 'branch' });
    expect(win.isSortedEligible('')).toBe(false);
    expect(win.planRequest('page-load', '')).toBe('drain');
    win.setViewState({ modeFilter: '' });
    expect(win.isSortedEligible('')).toBe(true);
    expect(win.planRequest('page-load', '')).toBe('fit');
  });

  it('while paged, setting a mode filter plans a drain; clearing it in capped returns to a fit request', () => {
    const { win, setHeld } = setup();
    win.setPaged({ agents: [agent('a')], totalCount: 60, nextCursor: 'c1' }, '');
    win.setViewState({ modeFilter: 'can_message' });
    expect(win.planRequest('view-change', '')).toBe('drain');
    setHeld([agent('a')]);
    win.adoptDrain({ complete: false, capped: true, error: null, requests: 4 });
    win.setViewState({ modeFilter: '' });
    expect(win.planRequest('view-change', '')).toBe('fit');
  });

  it('the small-state display equals the reference for every sort, phase, bare-key label and mode filter', () => {
    const phases: AgentPhase[] = ['running', 'stopped', 'error', 'suspended'];
    const modes = ['project', 'branch', 'lineage', 'none', undefined];
    const held = Array.from({ length: 40 }, (_, i) =>
      agent(`a${String(i).padStart(2, '0')}`, {
        name: `agent-${(i * 7) % 40}`,
        phase: phases[i % phases.length],
        messageMode: modes[i % modes.length] as Agent['messageMode'],
        created: `2026-01-${String((i % 28) + 1).padStart(2, '0')}T00:00:00Z`,
        updated: `2026-02-${String(((i * 3) % 28) + 1).padStart(2, '0')}T00:00:00Z`,
        lastActivityEvent: i % 3 === 0 ? `2026-03-0${(i % 9) + 1}T00:00:00Z` : undefined,
        labels: i % 2 === 0 ? { env: i % 4 === 0 ? 'prod' : 'dev' } : { team: 'x' },
        _messageability: i % 5 === 0 ? undefined : { canMessage: i % 2 === 0 },
      } as Partial<Agent>)
    );
    const { win, setHeld } = setup({ pageSize: 100 });
    setHeld(held);
    win.setSmall();
    const sortFields: AgentSortField[] = ['name', 'status', 'created', 'updated'];
    for (const sortField of sortFields) {
      for (const sortDir of ['asc', 'desc'] as SortDir[]) {
        for (const phase of ['', 'running', 'stopped'] as Array<AgentPhase | ''>) {
          for (const mode of ['', 'branch', 'project', 'can_message', 'cannot_message']) {
            for (const label of ['', 'env', 'env=prod']) {
              win.setViewState({ sortField, sortDir, phaseFilter: phase, modeFilter: mode, label });
              const expected = referenceDisplay(held, { phase, mode, label, sortField, sortDir });
              expect(win.display.map((a) => a.id)).toEqual(expected.map((a) => a.id));
            }
          }
        }
      }
    }
  });
});

describe('AgentListWindow — add rule override', () => {
  it('replaces the project match: a create joins only when the rule passes, and the committed k=v label still applies', () => {
    let scopeAll = true;
    const { win, agents } = setup({}, { isAddable: () => scopeAll });
    win.setPaged(
      {
        agents: [agent('a', { updated: '2026-01-05T00:00:00Z' })],
        totalCount: 1,
        stats: { total: 1, running: 1, agents: [['a', 'running']] },
      },
      'env=prod'
    );
    agents.set('n1', agent('n1', { projectId: 'other', labels: { env: 'prod' } }));
    agents.set('n2', agent('n2', { labels: { env: 'dev' } }));
    win.applyChanges(flushOf(['n1', 'n2']));
    expect(win.memberIndex.has('n1')).toBe(true); // any project
    expect(win.memberIndex.has('n2')).toBe(false); // outside the committed label
    expect(win.updatesAvailable).toBe(true);

    scopeAll = false;
    const { win: win2, agents: agents2 } = setup({}, { isAddable: () => scopeAll });
    win2.setPaged({ agents: [], totalCount: 0, stats: { total: 0, running: 0, agents: [] } }, '');
    agents2.set('n3', agent('n3'));
    win2.applyChanges(flushOf(['n3']));
    expect(win2.memberIndex.has('n3')).toBe(false);
    expect(win2.updatesAvailable).toBe(false);
  });

  it('markMembershipChanged raises the chip while paged and is a no-op in the local states', () => {
    const { win, setHeld } = setup();
    setHeld([agent('a')]);
    win.setSmall();
    win.markMembershipChanged();
    expect(win.updatesAvailable).toBe(false);
    expect(win.stale).toBe(false);
    win.setPaged({ agents: [agent('a')], totalCount: 60, nextCursor: 'c1' }, '');
    win.markMembershipChanged();
    expect(win.updatesAvailable).toBe(true);
  });
});

describe('AgentListWindow — count-only member index', () => {
  function countOnlyWindow() {
    const page = [
      agent('a', { updated: '2026-01-05T00:00:00Z' }),
      agent('b', { updated: '2026-01-04T00:00:00Z' }),
    ];
    let stats: PagedPageResult['stats'] = { total: 2001, running: 7 };
    const ctx = setup(
      {},
      {
        fetchPage: async (p) => ({
          agents: page,
          totalCount: 2001,
          nextCursor: 'c',
          ...(p.wantStats ? { stats } : {}),
        }),
      }
    );
    ctx.win.setPaged({ agents: page, totalCount: 2001, nextCursor: 'c1', stats }, '');
    return {
      ...ctx,
      setStats: (s: PagedPageResult['stats']) => {
        stats = s;
      },
    };
  }

  it('stats without IDs give a snapshot that a delete, a create and a phase change leave unchanged, each raising the chip', () => {
    const { win, agents } = countOnlyWindow();
    expect(win.memberIndex.countOnly).toBe(true);
    expect(win.stats).toEqual({ total: 2001, running: 7, incomplete: false });

    win.applyChanges(flushOf([], ['x1']));
    expect(win.stats.total).toBe(2001);
    expect(win.updatesAvailable).toBe(true);

    for (const change of [
      () => {
        agents.set('n1', agent('n1'));
        win.applyChanges(flushOf(['n1']));
      },
      () => {
        win.applyChanges({
          upserted: [],
          deleted: [],
          unknown: new Map([['x2', { phase: 'stopped' }]]),
          generation: 1,
        } as never);
      },
    ]) {
      (win as unknown as { _updatesAvailable: boolean })._updatesAvailable = false;
      change();
      expect(win.stats).toEqual({ total: 2001, running: 7, incomplete: false });
      expect(win.updatesAvailable).toBe(true);
    }
  });

  it('an on-page row is still replaced live in count-only mode', () => {
    const { win, agents } = countOnlyWindow();
    agents.set('b', agent('b', { updated: '2026-01-04T00:00:00Z', phase: 'stopped' }));
    win.applyChanges(flushOf(['b']));
    expect(win.items.find((a) => a.id === 'b')?.phase).toBe('stopped');
    expect(win.stats.running).toBe(7);
  });

  it('the chip refresh sends stats=1 on any page and updates the counts', async () => {
    const { win, fetchPage, setStats } = countOnlyWindow();
    await win.next();
    expect(win.pageIndex).toBe(1);
    expect(fetchPage.mock.calls.at(-1)?.[0].wantStats).toBe(false);
    setStats({ total: 2003, running: 9 });
    await win.refresh();
    expect(fetchPage.mock.calls.at(-1)?.[0].wantStats).toBe(true);
    expect(win.stats).toEqual({ total: 2003, running: 9, incomplete: false });
    expect(win.updatesAvailable).toBe(false);
  });

  it('a later response with IDs leaves count-only mode', () => {
    const { win } = countOnlyWindow();
    win.setPaged(
      {
        agents: [agent('a')],
        totalCount: 1,
        stats: { total: 1, running: 1, agents: [['a', 'running']] },
      },
      ''
    );
    expect(win.memberIndex.countOnly).toBe(false);
    expect(win.stats.total).toBe(1);
  });
});

describe('AgentMemberIndex — count-only', () => {
  it('seedCounts holds a snapshot that set and delete do not change; seed leaves count-only mode', () => {
    const idx = new AgentMemberIndex();
    idx.seed([['a', 'running']]);
    idx.seedCounts(2500, 40);
    expect(idx.countOnly).toBe(true);
    expect(idx.size).toBe(0);
    idx.set('b', 'running');
    idx.delete('a');
    expect(idx.has('b')).toBe(false);
    expect(idx.stats).toEqual({ total: 2500, running: 40 });
    idx.seed([['c', 'stopped']]);
    expect(idx.countOnly).toBe(false);
    expect(idx.stats).toEqual({ total: 1, running: 0 });
  });
});
