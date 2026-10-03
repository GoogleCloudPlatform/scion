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

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { Agent } from '../shared/types.js';
import { StateManager } from './state.js';
import {
  AgentDrainRunner,
  DRAIN_MAX_REQUESTS,
  DRAIN_PAGE_LIMIT,
  drainAgents,
  type AgentDrainFetch,
} from './agent-drain.js';

class FakeEventSource extends EventTarget {
  readyState = 0;
  close(): void {
    this.readyState = 2;
  }
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function makeAgent(i: number, overrides: Partial<Agent> = {}): Agent {
  const t = new Date(Date.UTC(2026, 0, 1) + i * 1000).toISOString();
  return {
    id: `a-${String(i).padStart(5, '0')}`,
    name: `agent-${i}`,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    created: t,
    updated: t,
    ...overrides,
  } as Agent;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface FakeServerOptions {
  /** Candidate rows, in created-desc (walk) order. */
  rows: Agent[];
  /** Rows the caller may read; others are filtered out of pages (the cursor still advances past them). */
  readable?: (a: Agent) => boolean;
  /** Return a Response (or throw) to override a given attempt (0-based over all fetches). */
  override?: (attempt: number, url: URL) => Response | Error | undefined;
  /** Called before each page is served, so a test can interleave live events. */
  beforeServe?: (pageIndex: number) => void;
}

/** A minimal legacy list endpoint: limit-sized pages over `rows`, cursor = row offset. */
function fakeServer(opts: FakeServerOptions) {
  const urls: URL[] = [];
  let attempts = 0;
  let served = 0;
  const fn: AgentDrainFetch = (url, init) => {
    const u = new URL(url, 'http://localhost');
    urls.push(u);
    const attempt = attempts++;
    if (init.signal?.aborted) {
      const err = new Error('aborted');
      err.name = 'AbortError';
      return Promise.reject(err);
    }
    const o = opts.override?.(attempt, u);
    if (o instanceof Error) return Promise.reject(o);
    if (o) return Promise.resolve(o);
    opts.beforeServe?.(served++);
    const limit = Number(u.searchParams.get('limit'));
    const start = Number(u.searchParams.get('cursor') ?? '0');
    const slice = opts.rows.slice(start, start + limit);
    const end = start + slice.length;
    const agents = slice.filter(opts.readable ?? (() => true));
    return Promise.resolve(
      json({
        agents,
        ...(end < opts.rows.length ? { nextCursor: String(end) } : {}),
        _capabilities: { actions: ['create'] },
      })
    );
  };
  return { fn, urls };
}

function rowsDesc(n: number): Agent[] {
  const out: Agent[] = [];
  for (let i = n - 1; i >= 0; i--) out.push(makeAgent(i));
  return out;
}

describe('drainAgents', () => {
  it('1,201 agents over 3 pages give every node, with lineage across a page boundary intact', async () => {
    const rows = rowsDesc(1201);
    // The child sits on page 2, its parent and grandparent on page 1.
    rows[600] = { ...rows[600], ancestry: [rows[3].id, rows[10].id] } as Agent;
    const server = fakeServer({ rows });
    const result = await drainAgents('/api/v1/projects/p1/agents', { fetchFn: server.fn });

    expect(server.urls).toHaveLength(3);
    expect(result.complete).toBe(true);
    expect(result.capped).toBe(false);
    expect(result.error).toBeNull();
    expect(result.requests).toBe(3);
    expect(result.agents.map((a) => a.id)).toEqual(rows.map((a) => a.id));
    const ids = new Set(result.agents.map((a) => a.id));
    const child = result.agents.find((a) => a.id === rows[600].id);
    expect(child?.ancestry).toEqual([rows[3].id, rows[10].id]);
    for (const anc of child?.ancestry ?? []) expect(ids.has(anc)).toBe(true);
    expect(result.capabilities).toEqual({ actions: ['create'] });
  });

  it('sends limit=500, follows nextCursor, keeps filters, and sends view only for compact', async () => {
    const server = fakeServer({ rows: rowsDesc(501) });
    await drainAgents('/api/v1/projects/p1/agents?label=team%3Dx', { fetchFn: server.fn });
    expect(server.urls.map((u) => u.searchParams.get('limit'))).toEqual(['500', '500']);
    expect(server.urls[0].searchParams.has('cursor')).toBe(false);
    expect(server.urls[1].searchParams.get('cursor')).toBe('500');
    expect(server.urls.every((u) => u.searchParams.get('label') === 'team=x')).toBe(true);
    expect(server.urls.every((u) => !u.searchParams.has('view'))).toBe(true);
    expect(server.urls.every((u) => !u.searchParams.has('sort'))).toBe(true);

    const compact = fakeServer({ rows: rowsDesc(10) });
    await drainAgents('/api/v1/agents', { fetchFn: compact.fn, view: 'compact' });
    expect(compact.urls[0].searchParams.get('view')).toBe('compact');
  });

  it('2,001 agents stop after 4 requests, capped, with a nextCursor left; ancestors beyond the cap are absent', async () => {
    const rows = rowsDesc(2001);
    // A loaded agent whose parent is the one row past the cap.
    rows[5] = { ...rows[5], ancestry: [rows[2000].id] } as Agent;
    const server = fakeServer({ rows });
    const result = await drainAgents('/x', { fetchFn: server.fn });
    expect(server.urls).toHaveLength(DRAIN_MAX_REQUESTS);
    expect(result.capped).toBe(true);
    expect(result.complete).toBe(false);
    expect(result.error).toBeNull();
    expect(result.agents).toHaveLength(DRAIN_MAX_REQUESTS * DRAIN_PAGE_LIMIT);
    const ids = new Set(result.agents.map((a) => a.id));
    expect(ids.has(rows[2000].id)).toBe(false);
    expect(result.agents.find((a) => a.id === rows[5].id)?.ancestry).toEqual([rows[2000].id]);
  });

  it('exactly 2,000 agents complete in 4 requests (the cap is not reached without a nextCursor)', async () => {
    const server = fakeServer({ rows: rowsDesc(2000) });
    const result = await drainAgents('/x', { fetchFn: server.fn });
    expect(server.urls).toHaveLength(4);
    expect(result.complete).toBe(true);
    expect(result.capped).toBe(false);
  });

  it('a read-filtered page with zero items and a nextCursor does not end the drain, and counts toward the cap', async () => {
    const rows = rowsDesc(2001);
    // Only the first 200 rows of page 2 and nothing of page 1 are readable.
    const readable = new Set(rows.slice(500, 700).map((a) => a.id));
    const server = fakeServer({ rows, readable: (a) => readable.has(a.id) });
    const result = await drainAgents('/x', { fetchFn: server.fn });
    expect(server.urls).toHaveLength(4);
    expect(result.requests).toBe(4);
    expect(result.capped).toBe(true);
    expect(result.complete).toBe(false);
    expect(result.agents).toHaveLength(200);

    // Zero items on every page but the last still walks to the end.
    const sparse = fakeServer({ rows: rowsDesc(1500), readable: (a) => a.id === 'a-00000' });
    const r2 = await drainAgents('/x', { fetchFn: sparse.fn });
    expect(sparse.urls).toHaveLength(3);
    expect(r2.complete).toBe(true);
    expect(r2.agents.map((a) => a.id)).toEqual(['a-00000']);
  });

  it('a page-2 failure that persists after two retries returns an incomplete result with page 1 kept', async () => {
    const server = fakeServer({
      rows: rowsDesc(1200),
      override: (attempt) => (attempt >= 1 ? json({ error: 'boom' }, 503) : undefined),
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(server.urls).toHaveLength(1 + 3); // page 1, then page 2 tried three times.
    expect(result.complete).toBe(false);
    expect(result.capped).toBe(false);
    expect(result.error).toEqual({ message: expect.stringContaining('503'), status: 503 });
    expect(result.agents).toHaveLength(500);
  });

  it('retries each page twice: a network error then a 500 then success completes', async () => {
    const server = fakeServer({
      rows: rowsDesc(600),
      override: (attempt) =>
        attempt === 1 ? new TypeError('network') : attempt === 2 ? json({}, 500) : undefined,
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(server.urls).toHaveLength(4);
    expect(result.complete).toBe(true);
    expect(result.requests).toBe(2);
    expect(result.agents).toHaveLength(600);
  });

  it('an unparseable body is retried; a body that never parses ends with an error, never a silent completion', async () => {
    const server = fakeServer({
      rows: rowsDesc(10),
      override: () => new Response('not json', { status: 200 }),
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(server.urls).toHaveLength(3);
    expect(result.complete).toBe(false);
    expect(result.error).not.toBeNull();
    expect(result.agents).toEqual([]);
  });

  it('a 400 (for example a rejected label) is not retried and reports its status', async () => {
    const server = fakeServer({
      rows: rowsDesc(10),
      override: () => json({ error: { message: 'bad label' } }, 400),
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(server.urls).toHaveLength(1);
    expect(result.complete).toBe(false);
    expect(result.error?.status).toBe(400);
  });

  it('a legacy array body is a complete set', async () => {
    const fn: AgentDrainFetch = () => Promise.resolve(json([makeAgent(1), makeAgent(2)]));
    const result = await drainAgents('/x', { fetchFn: fn });
    expect(result.complete).toBe(true);
    expect(result.agents).toHaveLength(2);
  });

  it('an aborted signal rejects with AbortError instead of resolving a partial result', async () => {
    const controller = new AbortController();
    const server = fakeServer({
      rows: rowsDesc(1200),
      beforeServe: (page) => {
        if (page === 1) controller.abort();
      },
    });
    await expect(
      drainAgents('/x', { fetchFn: server.fn, signal: controller.signal })
    ).rejects.toMatchObject({ name: 'AbortError' });
  });
});

/** Drive the state store's SSE path directly. */
function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

function flush(sm: StateManager): void {
  (sm as unknown as { flush(): void }).flush();
}

function connect(sm: StateManager): void {
  sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
}

function openEpochs(sm: StateManager): number {
  return (sm as unknown as { seedEpochs: Map<unknown, unknown> }).seedEpochs.size;
}

describe('AgentDrainRunner (seed-epoch protocol)', () => {
  it('seeds the drained set with an epoch token: a live update during the drain survives the seed', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const rows = rowsDesc(700);
    const target = rows[650];
    sm.seedAgents([target]);
    const server = fakeServer({
      rows,
      beforeServe: (page) => {
        if (page === 1) {
          emit(sm, `agent.${target.id}.status`, { phase: 'stopped' });
          flush(sm);
        }
      },
    });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });

    expect(result?.complete).toBe(true);
    expect(result?.stale).toBe(false);
    // The REST row (phase running) did not overwrite the live phase.
    expect(sm.getAgent(target.id)?.phase).toBe('stopped');
    expect(result?.agents.find((a) => a.id === target.id)?.phase).toBe('stopped');
    expect(openEpochs(sm)).toBe(0);
  });

  it('a live create during the drain is added; a live delete during the drain is removed', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const rows = rowsDesc(900);
    const doomed = rows[700];
    const server = fakeServer({
      rows,
      beforeServe: (page) => {
        if (page === 0) {
          emit(sm, 'agent.new-1.created', {
            id: 'new-1',
            name: 'new',
            projectId: 'p1',
            phase: 'running',
          });
          emit(sm, 'agent.other-1.created', {
            id: 'other-1',
            name: 'other',
            projectId: 'p2',
            phase: 'running',
          });
          emit(sm, `agent.${doomed.id}.deleted`, {});
          flush(sm);
        }
      },
    });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({
      url: '/x',
      view: 'full',
      isMember: (a) => a.projectId === 'p1',
    });
    const ids = new Set(result?.agents.map((a) => a.id));
    expect(ids.has('new-1')).toBe(true);
    expect(ids.has('other-1')).toBe(false);
    expect(ids.has(doomed.id)).toBe(false);
    expect(sm.getAgent(doomed.id)).toBeUndefined();
    expect(result?.agents).toHaveLength(900); // 900 - 1 deleted + 1 created
    expect(result?.stale).toBe(false);
  });

  it('an undecidable live create (no membership rule) is never added, marks the result stale, and issues no request', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    connect(sm);
    const server = fakeServer({
      rows: rowsDesc(20),
      beforeServe: () => {
        emit(sm, 'agent.new-1.created', { id: 'new-1', name: 'n', projectId: 'p9' });
        flush(sm);
      },
    });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({ url: '/api/v1/agents?scope=mine', view: 'compact' });
    expect(result?.agents.some((a) => a.id === 'new-1')).toBe(false);
    expect(result?.stale).toBe(true);
    expect(server.urls).toHaveLength(1);
  });

  it('a compact drain seeds partially, so full fields already in state survive', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    connect(sm);
    const full = makeAgent(1, { taskSummary: 'keep me' } as Partial<Agent>);
    sm.seedAgents([full]);
    const compactRow = { id: full.id, name: full.name, phase: 'running' } as Agent;
    const fn: AgentDrainFetch = () => Promise.resolve(json({ agents: [compactRow] }));
    const runner = new AgentDrainRunner({ state: sm, fetchFn: fn });
    const result = await runner.run({ url: '/x', view: 'compact', isMember: () => true });
    expect((sm.getAgent(full.id) as Agent & { taskSummary?: string }).taskSummary).toBe('keep me');
    expect((result?.agents[0] as Agent & { taskSummary?: string }).taskSummary).toBe('keep me');
  });

  it('waits for the live connection before opening the epoch', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    const server = fakeServer({ rows: rowsDesc(3) });
    const runner = new AgentDrainRunner({
      state: sm,
      fetchFn: server.fn,
      connectTimeoutMs: 60_000,
    });
    const pending = runner.run({ url: '/x', view: 'full', isMember: () => true });
    await Promise.resolve();
    await Promise.resolve();
    expect(server.urls).toHaveLength(0);
    expect(openEpochs(sm)).toBe(0);
    connect(sm);
    const result = await pending;
    expect(server.urls).toHaveLength(1);
    expect(result?.stale).toBe(false);
  });

  it('drains anyway after the connect timeout and marks the result stale', async () => {
    vi.useFakeTimers();
    try {
      const sm = new StateManager();
      sm.setScope({ type: 'project', projectId: 'p1' });
      const server = fakeServer({ rows: rowsDesc(3) });
      const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
      const pending = runner.run({ url: '/x', view: 'full', isMember: () => true });
      await vi.advanceTimersByTimeAsync(3000);
      const result = await pending;
      expect(server.urls).toHaveLength(1);
      expect(result?.complete).toBe(true);
      expect(result?.stale).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a scope switch mid-drain discards the result and seeds nothing', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const server = fakeServer({
      rows: rowsDesc(800),
      beforeServe: (page) => {
        if (page === 1) sm.setScope({ type: 'project', projectId: 'p2' });
      },
    });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result).toBeNull();
    expect(sm.getAgents()).toHaveLength(0);
    expect(openEpochs(sm)).toBe(0);
  });

  it('a second run aborts the first, which resolves null; the second completes', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    let release: (() => void) | null = null;
    const gate = new Promise<void>((r) => (release = r));
    const slowRows = rowsDesc(1200);
    let calls = 0;
    const fn: AgentDrainFetch = async (url, init) => {
      calls++;
      if (calls === 1) {
        await gate;
        if (init.signal?.aborted) {
          const e = new Error('aborted');
          e.name = 'AbortError';
          throw e;
        }
      }
      const u = new URL(url, 'http://localhost');
      const start = Number(u.searchParams.get('cursor') ?? '0');
      return json({ agents: slowRows.slice(start, start + 500) });
    };
    const runner = new AgentDrainRunner({ state: sm, fetchFn: fn });
    const first = runner.run({ url: '/x', view: 'full', isMember: () => true });
    await Promise.resolve();
    await Promise.resolve();
    const second = runner.run({ url: '/y', view: 'full', isMember: () => true });
    release!();
    expect(await first).toBeNull();
    const r2 = await second;
    expect(r2?.complete).toBe(true);
    expect(openEpochs(sm)).toBe(0);
  });

  it('abort() during a drain resolves null, closes the epoch and issues no further request', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    let runner: AgentDrainRunner | null = null;
    const server = fakeServer({
      rows: rowsDesc(1500),
      beforeServe: (page) => {
        if (page === 0) runner?.abort();
      },
    });
    runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result).toBeNull();
    expect(server.urls).toHaveLength(1);
    expect(openEpochs(sm)).toBe(0);
    expect(runner.running).toBe(false);
  });

  it('a failed drain still closes the epoch and reports the error with what loaded', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const server = fakeServer({
      rows: rowsDesc(800),
      override: (attempt) => (attempt >= 1 ? json({}, 502) : undefined),
    });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn, retryDelayMs: 0 });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result?.complete).toBe(false);
    expect(result?.error?.status).toBe(502);
    expect(result?.agents).toHaveLength(500);
    expect(openEpochs(sm)).toBe(0);
  });

  it('a capped drain is reported capped through the protocol, never complete', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const server = fakeServer({ rows: rowsDesc(2001) });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result?.capped).toBe(true);
    expect(result?.complete).toBe(false);
    expect(server.urls).toHaveLength(4);
  });
});
