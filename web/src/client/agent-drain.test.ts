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
import { AgentSeedEpoch } from './agent-seed-epoch.js';

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
  /** The `_capabilities` every served page carries. Defaults to `create`. */
  capabilities?: { actions: string[] };
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
        _capabilities: opts.capabilities ?? { actions: ['create'] },
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

  it('reports a first-page failure only when no page arrived', async () => {
    const failing = fakeServer({ rows: rowsDesc(10), override: () => json({}, 503) });
    const failed = await drainAgents('/x', { fetchFn: failing.fn, retryDelayMs: 0 });
    expect(failed.firstPageFailed).toBe(true);

    // Page 1 arrives with zero readable items, then page 2 keeps failing:
    // an incomplete result, not a first-page failure.
    const server = fakeServer({
      rows: rowsDesc(800),
      readable: () => false,
      override: (attempt) => (attempt >= 1 ? json({}, 502) : undefined),
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(result.agents).toHaveLength(0);
    expect(result.requests).toBe(1);
    expect(result.error?.status).toBe(502);
    expect(result.firstPageFailed).toBe(false);
  });

  it('continues from an already-fetched first page and its cursor, within the same request cap', async () => {
    const rows = rowsDesc(2600);
    // The drained pages carry other capabilities than the carried page.
    const server = fakeServer({ rows, capabilities: { actions: ['read'] } });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      firstPage: {
        agents: rows.slice(0, 500),
        nextCursor: '500',
        capabilities: { actions: ['create'] },
      },
    });
    // Page 1 is never requested again; pages 2-4 are, from its cursor.
    expect(server.urls.map((u) => u.searchParams.get('cursor'))).toEqual(['500', '1000', '1500']);
    expect(result.requests).toBe(DRAIN_MAX_REQUESTS);
    expect(result.capped).toBe(true);
    expect(result.agents).toHaveLength(2000);
    expect(result.agents[0].id).toBe(rows[0].id);
    expect(result.capabilities).toEqual({ actions: ['create'] });
    expect(result.firstPageFailed).toBe(false);
  });

  it('a short carried first page is discarded: the drain starts from the first page with no cursor and sends at most four pages', async () => {
    const rows = rowsDesc(2600);
    const server = fakeServer({
      rows,
      override: (_attempt, u) =>
        u.searchParams.has('cursor')
          ? undefined
          : json({
              agents: rows.slice(0, 500),
              nextCursor: '500',
              _capabilities: { actions: ['read'] },
            }),
    });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      firstPage: {
        agents: rows.slice(0, 25),
        nextCursor: '25',
        capabilities: { actions: ['create'] },
      },
    });
    expect(server.urls.map((u) => u.searchParams.get('cursor'))).toEqual([
      null,
      '500',
      '1000',
      '1500',
    ]);
    expect(server.urls.every((u) => u.searchParams.get('limit') === '500')).toBe(true);
    // The short answer plus four drain pages.
    expect(result.requests).toBe(DRAIN_MAX_REQUESTS + 1);
    expect(result.capped).toBe(true);
    expect(result.complete).toBe(false);
    // Exactly the newest 2,000 candidate rows, never the 25 on top of them.
    expect(result.agents).toHaveLength(DRAIN_MAX_REQUESTS * DRAIN_PAGE_LIMIT);
    expect(result.agents.map((a) => a.id)).toEqual(rows.slice(0, 2000).map((a) => a.id));
    // The capabilities come from the drain's own first page, not the discarded one.
    expect(result.capabilities).toEqual({ actions: ['read'] });
    expect(result.firstPageFailed).toBe(false);
  });

  it('a row only the discarded short page holds never reaches the result', async () => {
    const rows = rowsDesc(1600);
    const server = fakeServer({ rows });
    const gone = makeAgent(0, { id: 'gone-1', name: 'gone-1' });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      firstPage: { agents: [gone, ...rows.slice(0, 24)], nextCursor: '25' },
    });
    expect(result.complete).toBe(true);
    expect(result.agents.some((a) => a.id === 'gone-1')).toBe(false);
    expect(result.agents).toHaveLength(1600);
    expect(result.agents.map((a) => a.id)).toEqual(rows.map((a) => a.id));
  });

  it('a short carried first page below the cap: the restarted drain completes the set in four pages', async () => {
    const rows = rowsDesc(1600);
    const server = fakeServer({ rows });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      firstPage: { agents: rows.slice(0, 25), nextCursor: '25' },
    });
    expect(server.urls.map((u) => u.searchParams.get('cursor'))).toEqual([
      null,
      '500',
      '1000',
      '1500',
    ]);
    expect(result.requests).toBe(5);
    expect(result.complete).toBe(true);
    expect(result.capped).toBe(false);
    expect(result.agents).toHaveLength(1600);
    expect(new Set(result.agents.map((a) => a.id)).size).toBe(1600);
  });

  it('a read-filtered short carried page (500 candidates returning 300 rows) never earns a fifth drain page', async () => {
    const rows = rowsDesc(2600);
    // Two of every five candidate rows are unreadable.
    const readable = (a: Agent): boolean => Number(a.id.slice(2)) % 5 >= 2;
    const firstRows = rows.slice(0, 500).filter(readable);
    expect(firstRows).toHaveLength(300);
    const server = fakeServer({ rows, readable });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      firstPage: { agents: firstRows, nextCursor: '500' },
    });
    expect(server.urls).toHaveLength(DRAIN_MAX_REQUESTS);
    expect(server.urls[0].searchParams.has('cursor')).toBe(false);
    expect(server.urls.at(-1)?.searchParams.get('cursor')).toBe('1500');
    expect(result.capped).toBe(true);
    // The readable part of the newest 2,000 candidate rows only.
    expect(result.agents).toHaveLength(1200);
    expect(result.agents.every((a) => rows.indexOf(a) < 2000)).toBe(true);
  });

  it('a short carried first page then a failing first drain page is a first-page failure with no rows', async () => {
    const rows = rowsDesc(1600);
    const server = fakeServer({ rows, override: () => json({}, 503) });
    const result = await drainAgents('/x', {
      fetchFn: server.fn,
      retryDelayMs: 0,
      firstPage: { agents: rows.slice(0, 25), nextCursor: '25' },
    });
    expect(server.urls).toHaveLength(3);
    expect(result.agents).toEqual([]);
    expect(result.error?.status).toBe(503);
    expect(result.firstPageFailed).toBe(true);
  });

  it('the first page that carries capabilities wins over a later page', async () => {
    const rows = rowsDesc(1200);
    const inner = fakeServer({ rows });
    const fn: AgentDrainFetch = async (url, init) => {
      const res = await inner.fn(url, init);
      if (!new URL(url, 'http://localhost').searchParams.has('cursor')) return res;
      const body = (await res.json()) as Record<string, unknown>;
      return json({ ...body, _capabilities: { actions: ['delete'] } });
    };
    const result = await drainAgents('/x', { fetchFn: fn });
    expect(inner.urls).toHaveLength(3);
    expect(result.complete).toBe(true);
    expect(result.capabilities).toEqual({ actions: ['create'] });
  });

  it('a 429 then a 408 then success completes; a 404 is not retried', async () => {
    const server = fakeServer({
      rows: rowsDesc(600),
      override: (attempt) =>
        attempt === 1 ? json({}, 429) : attempt === 2 ? json({}, 408) : undefined,
    });
    const result = await drainAgents('/x', { fetchFn: server.fn, retryDelayMs: 0 });
    expect(server.urls).toHaveLength(4);
    expect(result.complete).toBe(true);
    expect(result.error).toBeNull();
    expect(result.agents).toHaveLength(600);

    const missing = fakeServer({ rows: rowsDesc(10), override: () => json({}, 404) });
    const failed = await drainAgents('/x', { fetchFn: missing.fn, retryDelayMs: 0 });
    expect(missing.urls).toHaveLength(1);
    expect(failed.error?.status).toBe(404);
    expect(failed.complete).toBe(false);
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

  it('a full drain seeds full objects, so a field missing from the drained row is gone from the store', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    connect(sm);
    const stored = makeAgent(1, { taskSummary: 'old task' } as Partial<Agent>);
    sm.seedAgents([stored]);
    const row = makeAgent(1);
    const fn: AgentDrainFetch = () => Promise.resolve(json({ agents: [row] }));
    const runner = new AgentDrainRunner({ state: sm, fetchFn: fn });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result?.complete).toBe(true);
    expect(sm.getAgent(stored.id)).toBeDefined();
    expect(
      (sm.getAgent(stored.id) as Agent & { taskSummary?: string }).taskSummary
    ).toBeUndefined();
    expect((result?.agents[0] as Agent & { taskSummary?: string }).taskSummary).toBeUndefined();
  });

  it('a discarded short carried page keeps its seed epoch: a live change since that request survives the restarted drain', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    connect(sm);
    const rows = rowsDesc(700);
    const target = rows[3];
    sm.seedAgents([target]);
    // Opened by the caller before its (short) request was sent.
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, `agent.${target.id}.status`, { phase: 'stopped' });
    flush(sm);
    const server = fakeServer({ rows });
    const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
    const result = await runner.run({
      url: '/x',
      view: 'full',
      isMember: () => true,
      firstPage: { agents: rows.slice(0, 25), nextCursor: '25' },
      epoch,
    });
    expect(server.urls[0].searchParams.has('cursor')).toBe(false);
    expect(server.urls).toHaveLength(2);
    expect(result?.complete).toBe(true);
    expect(result?.agents).toHaveLength(700);
    // The drained row (phase running) did not overwrite the live phase.
    expect(sm.getAgent(target.id)?.phase).toBe('stopped');
    expect(openEpochs(sm)).toBe(0);
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

  it('a second run started while the first is fetching aborts that fetch; the first resolves null', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    let release: (() => void) | null = null;
    const held = new Promise<void>((r) => (release = r));
    const slowRows = rowsDesc(1200);
    let calls = 0;
    let firstSignal: AbortSignal | undefined;
    const fn: AgentDrainFetch = async (url, init) => {
      calls++;
      if (calls === 1) {
        firstSignal = init.signal;
        await held;
        if (init.signal?.aborted) {
          const e = new Error('aborted');
          e.name = 'AbortError';
          throw e;
        }
      }
      const u = new URL(url, 'http://localhost');
      const start = Number(u.searchParams.get('cursor') ?? '0');
      const end = start + 500;
      return json({
        agents: slowRows.slice(start, end),
        ...(end < slowRows.length ? { nextCursor: String(end) } : {}),
      });
    };
    const runner = new AgentDrainRunner({ state: sm, fetchFn: fn });
    const first = runner.run({ url: '/x', view: 'full', isMember: () => true });
    // The first run's own page fetch is in flight before the second starts.
    await vi.waitFor(() => expect(calls).toBe(1));
    expect(firstSignal?.aborted).toBe(false);
    const second = runner.run({ url: '/y', view: 'full', isMember: () => true });
    expect(firstSignal?.aborted).toBe(true);
    release!();
    expect(await first).toBeNull();
    const r2 = await second;
    expect(r2?.complete).toBe(true);
    expect(r2?.agents).toHaveLength(1200);
    expect(openEpochs(sm)).toBe(0);
  });

  it('a scope switch during a drain aborts its page fetch and sends no further page request', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const signals: Array<AbortSignal | undefined> = [];
    const server = fakeServer({ rows: rowsDesc(2001) });
    const fn: AgentDrainFetch = (url, init) => {
      signals.push(init.signal);
      if (signals.length === 2) sm.setScope({ type: 'project', projectId: 'p2' });
      return server.fn(url, init);
    };
    const runner = new AgentDrainRunner({ state: sm, fetchFn: fn, retryDelayMs: 0 });
    const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
    expect(result).toBeNull();
    expect(signals[1]?.aborted).toBe(true);
    // Page 2 was in flight when the scope changed; pages 3 and 4 are never requested.
    expect(server.urls).toHaveLength(2);
    expect(sm.getAgents()).toHaveLength(0);
    expect(openEpochs(sm)).toBe(0);
  });

  it('removes its abort and scope listeners when the run ends', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'project', projectId: 'p1' });
    connect(sm);
    const added: string[] = [];
    const removed: string[] = [];
    const addSpy = vi.spyOn(AbortSignal.prototype, 'addEventListener').mockImplementation(function (
      this: AbortSignal,
      ...args
    ) {
      added.push(String(args[0]));
      return EventTarget.prototype.addEventListener.apply(this, args);
    });
    const removeSpy = vi
      .spyOn(AbortSignal.prototype, 'removeEventListener')
      .mockImplementation(function (this: AbortSignal, ...args) {
        removed.push(String(args[0]));
        return EventTarget.prototype.removeEventListener.apply(this, args);
      });
    const smAdd = vi.spyOn(sm, 'addEventListener');
    const smRemove = vi.spyOn(sm, 'removeEventListener');
    try {
      const server = fakeServer({ rows: rowsDesc(10) });
      const runner = new AgentDrainRunner({ state: sm, fetchFn: server.fn });
      const result = await runner.run({ url: '/x', view: 'full', isMember: () => true });
      expect(result?.complete).toBe(true);
    } finally {
      addSpy.mockRestore();
      removeSpy.mockRestore();
    }
    expect(added.filter((t) => t === 'abort').length).toBeGreaterThan(0);
    expect(removed.filter((t) => t === 'abort').length).toBe(
      added.filter((t) => t === 'abort').length
    );
    const scopeAdds = smAdd.mock.calls.filter(([t]) => t === 'scope-changed').length;
    const scopeRemoves = smRemove.mock.calls.filter(([t]) => t === 'scope-changed').length;
    expect(scopeAdds).toBe(1);
    expect(scopeRemoves).toBe(1);
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
