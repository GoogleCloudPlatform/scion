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
 * The paged window's frozen walk order (ptone/scion#3744): page 0's stats
 * population freezes the order, later pages are fetched by id, so an agent
 * whose sort key changes mid-walk is neither skipped nor repeated.
 */

import { describe, it, expect } from 'vitest';
import type { Agent } from '../shared/types.js';
import { AgentListWindow } from './agent-list-window.js';
import type { AgentListViewState, PagedPageParams, PagedPageResult } from './agent-list-window.js';

function agent(id: string, updated: number, phase = 'running'): Agent {
  return {
    id,
    name: id,
    projectId: 'p-1',
    template: 't',
    phase,
    created: '2026-01-01T00:00:00Z',
    updated: new Date(Date.UTC(2026, 0, 1, 0, 0, updated)).toISOString(),
    messageMode: 'project',
  } as Agent;
}

function viewState(partial: Partial<AgentListViewState> = {}): AgentListViewState {
  return {
    phaseFilter: '',
    label: '',
    sortField: 'updated',
    sortDir: 'desc',
    pageSize: 3,
    view: 'list',
    ...partial,
  };
}

/**
 * A fake sorted endpoint over a mutable agent set, updated desc, paging
 * either by a positional cursor over the CURRENT order (the server's
 * keyset behaviour: a key change moves rows across the cursor) or by ids.
 */
class FakeServer {
  agents: Agent[];
  requests: PagedPageParams[] = [];
  omitPopulation = false;
  constructor(agents: Agent[]) {
    this.agents = agents;
  }
  private sorted(phase: string): Agent[] {
    return [...this.agents]
      .filter((a) => !phase || a.phase === phase)
      .sort((a, b) => b.updated.localeCompare(a.updated) || b.id.localeCompare(a.id));
  }
  page(params: PagedPageParams, phase = ''): PagedPageResult {
    this.requests.push(params);
    const all = this.sorted('');
    const phased = this.sorted(phase);
    const stats = params.wantStats
      ? {
          total: all.length,
          running: all.filter((a) => a.phase === 'running').length,
          ...(this.omitPopulation
            ? {}
            : { agents: all.map((a) => [a.id, a.phase] as [string, string]) }),
        }
      : undefined;
    if (params.ids) {
      const wanted = new Set(params.ids);
      const rows = phased.filter((a) => wanted.has(a.id));
      return { agents: rows, totalCount: rows.length, stats };
    }
    // Keyset-like: continue after the cursor row's CURRENT position.
    let start = 0;
    if (params.cursor) {
      const at = phased.findIndex((a) => a.id === params.cursor);
      start = at < 0 ? phased.length : at + 1;
    }
    const rows = phased.slice(start, start + params.limit);
    const next = start + params.limit < phased.length ? rows[rows.length - 1]?.id : undefined;
    return { agents: rows, totalCount: phased.length, nextCursor: next, stats };
  }
  bump(id: string, at: number): void {
    this.agents = this.agents.map((a) =>
      a.id === id ? { ...a, updated: new Date(Date.UTC(2026, 0, 2, 0, 0, at)).toISOString() } : a
    );
  }
  remove(id: string): void {
    this.agents = this.agents.filter((a) => a.id !== id);
  }
}

function setup(server: FakeServer, vs: Partial<AgentListViewState> = {}): AgentListWindow {
  const state = viewState(vs);
  const win = new AgentListWindow({
    viewState: state,
    fetchPage: (params) => Promise.resolve(server.page(params, state.phaseFilter)),
    getAgent: () => undefined,
    getProjectId: () => 'p-1',
    getHeldAgents: () => [],
  });
  return win;
}

/** Walks every page from page 0, running `between` after each page. */
async function walk(win: AgentListWindow, server: FakeServer, between?: (page: number) => void) {
  win.setPaged(
    server.page({ limit: 3, wantStats: true, signal: new AbortController().signal }),
    ''
  );
  const seen: string[] = win.items.map((a) => a.id);
  between?.(0);
  let page = 0;
  while (win.hasNext) {
    await win.next();
    page++;
    expect(win.pageIndex).toBe(page);
    seen.push(...win.items.map((a) => a.id));
    between?.(page);
    if (page > 20) throw new Error('walk did not end');
  }
  return seen;
}

const ten = () => Array.from({ length: 10 }, (_, i) => agent(`a${i}`, 10 - i));

describe('AgentListWindow — frozen walk order', () => {
  it('a cursor walk over a mutable key skips agents (the bug this fixes)', async () => {
    const server = new FakeServer(ten());
    server.omitPopulation = true; // no population: the window follows cursors
    const win = setup(server);
    const seen = await walk(win, server, (page) => {
      if (page === 0) {
        server.bump('a7', 1);
        server.bump('a8', 2);
      }
    });
    expect(new Set(seen).size).toBe(seen.length);
    expect(seen).not.toContain('a7');
    expect(seen).not.toContain('a8');
  });

  it('returns every agent exactly once while sort keys change mid-walk, fetching later pages by id', async () => {
    const server = new FakeServer(ten());
    const win = setup(server);
    const seen = await walk(win, server, (page) => {
      if (page === 0) {
        server.bump('a7', 1); // not yet shown: moves to the front
        server.bump('a0', 2); // already shown: moves to the front again
      }
      if (page === 1) server.bump('a9', 3);
    });
    expect(seen).toEqual(['a0', 'a1', 'a2', 'a3', 'a4', 'a5', 'a6', 'a7', 'a8', 'a9']);
    const later = server.requests.slice(1);
    expect(later.length).toBe(3);
    for (const r of later) {
      expect(r.cursor).toBeUndefined();
      expect(r.ids?.length).toBeGreaterThan(0);
      expect(r.ids?.length).toBeLessThanOrEqual(3);
    }
    expect(win.total).toBe(10);
  });

  it('a deleted agent drops out of its page with no repeat or gap for the rest', async () => {
    const server = new FakeServer(ten());
    const win = setup(server);
    const seen = await walk(win, server, (page) => {
      if (page === 0) server.remove('a4');
    });
    expect(seen).toEqual(['a0', 'a1', 'a2', 'a3', 'a5', 'a6', 'a7', 'a8', 'a9']);
  });

  it('applies the phase filter to the frozen order', async () => {
    const agents = ten().map((a, i) => ({ ...a, phase: i % 2 === 0 ? 'running' : 'stopped' }));
    const server = new FakeServer(agents);
    const win = setup(server, { phaseFilter: 'running' });
    win.setPaged(
      server.page({ limit: 3, wantStats: true, signal: new AbortController().signal }, 'running'),
      ''
    );
    const seen = win.items.map((a) => a.id);
    server.bump('a8', 1);
    while (win.hasNext) {
      await win.next();
      seen.push(...win.items.map((a) => a.id));
    }
    expect(seen).toEqual(['a0', 'a2', 'a4', 'a6', 'a8']);
    expect(win.total).toBe(5);
  });

  it('falls back to cursors when the population does not match page 0', async () => {
    const server = new FakeServer(ten());
    const win = setup(server);
    const first = server.page({ limit: 3, wantStats: true, signal: new AbortController().signal });
    // A page row dropped (for example deleted live) so the head disagrees.
    win.setPaged({ ...first, agents: first.agents.slice(1) }, '');
    await win.next();
    expect(server.requests.at(-1)?.cursor).toBeDefined();
    expect(server.requests.at(-1)?.ids).toBeUndefined();
  });

  it('the refresh chip on a later page starts a fresh walk from page 0', async () => {
    const server = new FakeServer(ten());
    const win = setup(server);
    win.setPaged(
      server.page({ limit: 3, wantStats: true, signal: new AbortController().signal }),
      ''
    );
    await win.next();
    expect(win.pageIndex).toBe(1);
    server.bump('a9', 5);
    await win.refresh();
    expect(win.pageIndex).toBe(0);
    expect(server.requests.at(-1)?.wantStats).toBe(true);
    expect(win.items.map((a) => a.id)).toEqual(['a9', 'a0', 'a1']);
    await win.next();
    expect(server.requests.at(-1)?.ids).toEqual(['a2', 'a3', 'a4']);
  });
});
