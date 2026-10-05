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
 * Agent store: SSE application through the store-owned feed, seeded walks,
 * tombstones, the feed's completeness flag, and freshness while the feed is
 * down.
 */

// @vitest-environment happy-dom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { agent, createHarness, settle } from './__fixtures__/agent-store-harness.js';
import {
  AGENT_READ_BURST_LIMIT,
  AGENT_READ_CONCURRENCY,
  AGENT_READ_TIMEOUT_MS,
  type AgentListSnapshot,
} from './agent-store.js';
import type { Agent } from '../shared/types.js';
import { StateManager } from './state.js';

const VIEWS = ['compact', 'full'] as const;
const HUB = { scope: 'hub' } as const;
const P1 = { scope: 'project', projectId: 'p1' } as const;

function ids(snapshot: AgentListSnapshot | undefined): string[] {
  return (snapshot?.agents ?? []).map((a) => a.id);
}

/** The hub's `created` event payload for an agent. */
function created(id: string, projectId = 'p1'): Record<string, unknown> {
  return { agentId: id, projectId, name: id, slug: id, phase: 'running' };
}

function find(snapshot: AgentListSnapshot | undefined, id: string): Agent | undefined {
  return snapshot?.agents.find((a) => a.id === id);
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('AgentStore SSE application', () => {
  it('status, created, deleted and ports events reach two retained entries and both listeners with no request', async () => {
    const h = createHarness([agent('a1'), agent('a2')]);
    const hubSeen: AgentListSnapshot[] = [];
    const projectSeen: AgentListSnapshot[] = [];
    h.store.retain(HUB, (s) => hubSeen.push(s));
    h.store.retain(P1, (s) => projectSeen.push(s));
    const loads = [h.store.ensure(HUB), h.store.ensure(P1)];
    await h.connect();
    await Promise.all(loads);
    const requests = h.server.requests.length;
    expect(h.server.walks()).toBe(2);
    const hubCalls = hubSeen.length;
    const projectCalls = projectSeen.length;

    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped', activity: 'completed' });
    expect(find(h.store.peek(HUB), 'a1')?.phase).toBe('stopped');
    expect(find(h.store.peek(P1), 'a1')?.phase).toBe('stopped');

    await h.emitAgent('created', created('a3'));
    expect(ids(h.store.peek(HUB))).toContain('a3');
    expect(ids(h.store.peek(P1))).toContain('a3');

    await h.emitAgent('deleted', { agentId: 'a2' });
    expect(ids(h.store.peek(HUB))).not.toContain('a2');
    expect(ids(h.store.peek(P1))).not.toContain('a2');

    const port = { port: 8080, exposedAt: '2026-01-01T00:00:00Z', exposedBy: 'u' };
    await h.emitAgent('ports', { agentId: 'a1', ports: [port] });
    expect(find(h.store.peek(HUB), 'a1')?.exposedPorts).toEqual([port]);
    expect(find(h.store.peek(P1), 'a1')?.exposedPorts).toEqual([port]);

    // The created agent is fetched once on its own (it is not on this
    // server, so the fetch fails and the row stays); nothing walks.
    expect(h.server.requests).toHaveLength(requests + 1);
    expect(h.server.agentFetches('a3')).toBe(1);
    expect(h.server.walks()).toBe(2);
    expect(hubSeen.length - hubCalls).toBe(4);
    expect(projectSeen.length - projectCalls).toBe(4);
    // One object per agent, shared by every entry.
    expect(find(h.store.peek(HUB), 'a1')).toBe(find(h.store.peek(P1), 'a1'));
  });

  it('adds a created agent to a project entry only for its own project, and never to a filtered entry', async () => {
    const h = createHarness([agent('a1')]);
    const labelled = { scope: 'hub' as const, label: 'team=a' };
    const mine = { scope: 'hub' as const, ownership: 'mine' as const };
    for (const q of [HUB, P1, labelled, mine]) h.store.retain(q, () => {});
    const loads = [HUB, P1, labelled, mine].map((q) => h.store.ensure(q));
    await h.connect();
    await Promise.all(loads);

    await h.emitAgent('created', created('x2', 'p2'), 'p2');
    await h.emitAgent('created', created('x1'));

    expect(ids(h.store.peek(HUB))).toEqual(['a1', 'x2', 'x1']);
    expect(ids(h.store.peek(P1))).toEqual(['a1', 'x1']);
    expect(ids(h.store.peek(labelled))).toEqual(['a1']);
    expect(ids(h.store.peek(mine))).toEqual(['a1']);
  });

  it('keeps an agent created over SSE while a walk runs, though the walk did not list it', async () => {
    const h = createHarness([agent('a1')]);
    h.store.retain(HUB, () => {});
    const release = h.server.pause();
    const loading = h.store.ensure(HUB);
    await h.connect();

    await h.emitAgent('created', created('a9'));
    release();
    const snapshot = await loading;

    expect(ids(snapshot)).toEqual(['a1', 'a9']);
  });

  it('keeps an agent tombstoned when it was deleted before its page arrived in a progress snapshot', async () => {
    const h = createHarness([agent('a1'), agent('a2')], { pageSize: 1 });
    const progress: AgentListSnapshot[] = [];
    const release = h.server.pause();
    const loading = h.store.ensure(HUB, { onProgress: (s) => progress.push(s) });
    await h.connect();

    await h.emitAgent('deleted', { agentId: 'a2' });
    release();
    const snapshot = await loading;

    expect(progress.some((s) => ids(s).includes('a2'))).toBe(false);
    expect(ids(snapshot)).toEqual(['a1']);
  });

  it('keeps an agent created over SSE during a first load in every later progress snapshot', async () => {
    const h = createHarness([agent('a1'), agent('a2'), agent('a3')], { pageSize: 1 });
    const progress: AgentListSnapshot[] = [];
    const release = h.server.pause();
    const loading = h.store.ensure(HUB, { onProgress: (s) => progress.push(s) });
    await h.connect();

    await h.emitAgent('created', created('a9'));
    release();
    const snapshot = await loading;

    const first = progress.findIndex((s) => ids(s).includes('a9'));
    expect(first).toBeGreaterThanOrEqual(0);
    const later = progress.slice(first);
    expect(later.length).toBeGreaterThan(1);
    expect(later.every((s) => ids(s).includes('a9'))).toBe(true);
    expect(ids(progress[progress.length - 1])).toEqual(['a1', 'a2', 'a3', 'a9']);
    expect(ids(snapshot)).toEqual(['a1', 'a2', 'a3', 'a9']);
  });
  it('keeps a status change that lands between pages of a first load in later progress snapshots', async () => {
    const h = createHarness([agent('a1', { phase: 'running' }), agent('a2'), agent('a3')], {
      pageSize: 1,
    });
    const progress: AgentListSnapshot[] = [];
    let release: (() => void) | undefined;
    const loading = h.store.ensure(HUB, {
      onProgress: (s) => {
        progress.push(s);
        // Hold the pages after the first.
        if (progress.length === 1) release = h.server.pause();
      },
    });
    await h.connect();
    await vi.waitFor(() => expect(release).toBeDefined());

    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped' });
    const changed = progress.length;
    release!();
    const snapshot = await loading;

    const later = progress.slice(changed);
    expect(later.length).toBeGreaterThan(0);
    expect(later.map((s) => find(s, 'a1')?.phase)).toEqual(later.map(() => 'stopped'));
    expect(find(snapshot, 'a1')?.phase).toBe('stopped');
  });
});

describe('AgentStore agents created over SSE', () => {
  const SCOPE = { actions: ['create', 'list', 'message'] };
  const OWN = { actions: ['read', 'attach', 'message'] };

  async function loadedHub(): Promise<ReturnType<typeof createHarness>> {
    const h = createHarness([agent('a1')]);
    h.server.scopeCapabilities = SCOPE;
    h.store.retain(HUB, () => {});
    h.store.retain(P1, () => {});
    const loads = [h.store.ensure(HUB), h.store.ensure(P1)];
    await h.connect();
    await Promise.all(loads);
    return h;
  }

  it('fetches a created agent once and takes its own capabilities and messageability', async () => {
    const h = await loadedHub();
    h.server.agents.push(
      agent('a3', { _capabilities: OWN, _messageability: { canMessage: true } } as Partial<Agent>)
    );

    // The hub's created payload carries no capabilities or messageability.
    await h.emitAgent('created', created('a3'));

    for (const q of [HUB, P1]) {
      const row = find(h.store.peek(q), 'a3');
      expect(row?._capabilities).toEqual(OWN);
      expect(row?._messageability).toEqual({ canMessage: true });
    }
    expect(find(h.store.peek(HUB), 'a3')).toBe(find(h.store.peek(P1), 'a3'));
    expect(h.server.agentFetches('a3')).toBe(1);
    expect(h.server.requests.filter((p) => p.startsWith('/api/v1/agents/'))).toEqual([
      '/api/v1/agents/a3',
    ]);
    expect(h.server.walks()).toBe(2);
  });

  it('does not fetch a created agent whose event already carries capabilities', async () => {
    const h = await loadedHub();
    const requests = h.server.requests.length;

    await h.emitAgent('created', { ...created('a3'), _capabilities: OWN });

    expect(find(h.store.peek(HUB), 'a3')?._capabilities).toEqual(OWN);
    expect(h.server.requests).toHaveLength(requests);
  });

  it('keeps a status change that arrives while the created agent is fetched', async () => {
    const h = await loadedHub();
    h.server.agents.push(agent('a3', { _capabilities: OWN, phase: 'running' }));
    const release = h.server.pause();

    await h.emitAgent('created', created('a3'));
    await h.emitAgent('status', { agentId: 'a3', phase: 'stopped', activity: 'completed' });
    release();
    await settle();

    const row = find(h.store.peek(HUB), 'a3');
    expect(row?._capabilities).toEqual(OWN);
    expect(row?.phase).toBe('stopped');
  });

  it('does not bring back an agent deleted while its fetch is in flight', async () => {
    const h = await loadedHub();
    h.server.agents.push(agent('a3', { _capabilities: OWN }));
    const release = h.server.pause();

    await h.emitAgent('created', created('a3'));
    await h.emitAgent('deleted', { agentId: 'a3' });
    release();
    await settle();

    expect(ids(h.store.peek(HUB))).toEqual(['a1']);
    expect(h.feeds[0]?.getAgent('a3')).toBeUndefined();
  });

  it('keeps the created row with the list capabilities when its fetch fails', async () => {
    const h = await loadedHub();

    await h.emitAgent('created', created('a3'));

    expect(h.server.agentFetches('a3')).toBe(1);
    expect(find(h.store.peek(HUB), 'a3')?._capabilities).toEqual(SCOPE);
  });
});

describe('AgentStore reads of created agents', () => {
  const OWN = { actions: ['read', 'attach', 'message'] };

  async function loadedHub(): Promise<ReturnType<typeof createHarness>> {
    const h = createHarness([agent('a1')]);
    h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    return h;
  }

  /** Deliver `created` for each id in one feed flush. */
  async function createMany(h: ReturnType<typeof createHarness>, idList: string[]): Promise<void> {
    for (const id of idList) h.stream().emit('project.p1.agent.created', created(id));
    await vi.advanceTimersByTimeAsync(100);
    await settle();
  }

  function readSignal(h: ReturnType<typeof createHarness>, id: string): AbortSignal | undefined {
    const call = h.server.fetch.mock.calls.find(([path]) => path === `/api/v1/agents/${id}`);
    return call?.[1].signal ?? undefined;
  }

  it('reads at most four created agents at once and queues the rest', async () => {
    const h = await loadedHub();
    const created6 = ['n1', 'n2', 'n3', 'n4', 'n5', 'n6'];
    for (const id of created6) h.server.agents.push(agent(id, { _capabilities: OWN }));
    const release = h.server.holdAgentReads();

    await createMany(h, created6);
    expect(h.server.agentFetches()).toBe(AGENT_READ_CONCURRENCY);

    release();
    await settle();
    expect(h.server.agentFetches()).toBe(6);
    for (const id of created6) expect(find(h.store.peek(HUB), id)?._capabilities).toEqual(OWN);
    expect(h.server.walks()).toBe(1);
  });

  it('a burst of created agents past the limit revalidates once instead of reading each', async () => {
    const h = await loadedHub();
    const burst = Array.from({ length: AGENT_READ_BURST_LIMIT + 1 }, (_, i) => `n${i}`);
    for (const id of burst) h.server.agents.push(agent(id, { _capabilities: OWN }));

    await createMany(h, burst);

    expect(h.server.agentFetches()).toBe(0);
    expect(h.server.walks()).toBe(2);
    for (const id of burst) expect(find(h.store.peek(HUB), id)?._capabilities).toEqual(OWN);
  });

  it('reads an agent once while its read is in flight, even when another list adds it', async () => {
    const h = await loadedHub();
    const release = h.server.holdAgentReads();
    await h.emitAgent('created', created('a3'));
    expect(h.server.agentFetches('a3')).toBe(1);

    // The project list, read before the server listed a3, leaves it out;
    // a later status event adds it to that list too.
    await h.store.ensure(P1);
    expect(ids(h.store.peek(P1))).toEqual(['a1']);
    h.server.agents.push(agent('a3', { _capabilities: OWN }));
    await h.emitAgent('status', { agentId: 'a3', phase: 'stopped' });
    expect(ids(h.store.peek(P1))).toEqual(['a1', 'a3']);

    release();
    await settle();
    expect(h.server.agentFetches('a3')).toBe(1);
    expect(find(h.store.peek(P1), 'a3')?._capabilities).toEqual(OWN);
  });

  it('cancels reads when the feed closes, and reads again on the next feed', async () => {
    const h = await loadedHub();
    h.server.holdAgentReads();
    await h.emitAgent('created', created('a3'));
    expect(readSignal(h, 'a3')?.aborted).toBe(false);

    h.store.reset('test');
    expect(readSignal(h, 'a3')?.aborted).toBe(true);

    h.store.retain(HUB, () => {});
    const again = h.store.ensure(HUB);
    await h.connect();
    await again;
    await h.emitAgent('created', created('a3'));
    expect(h.server.agentFetches('a3')).toBe(2);
  });

  it('abandons a read after its timeout and frees its slot', async () => {
    const h = await loadedHub();
    h.server.holdAgentReads();
    await createMany(h, ['n1', 'n2', 'n3', 'n4', 'n5']);
    expect(h.server.agentFetches()).toBe(4);

    await vi.advanceTimersByTimeAsync(AGENT_READ_TIMEOUT_MS);
    await settle();

    expect(readSignal(h, 'n1')?.aborted).toBe(true);
    expect(h.server.agentFetches('n5')).toBe(1);
  });

  it('a read answered after the feed was replaced does not seed the old feed', async () => {
    const h = await loadedHub();
    h.server.agents.push(agent('a3', { _capabilities: OWN }));
    const release = h.server.holdAgentReads({ ignoreAbort: true });
    await h.emitAgent('created', created('a3'));

    h.events.dispatchEvent(new CustomEvent('scion:membership-changed'));
    await h.connect();
    release();
    await settle();

    expect(h.feeds).toHaveLength(2);
    expect(h.feeds[0]?.getAgent('a3')?._capabilities).not.toEqual(OWN);
    expect(find(h.store.peek(HUB), 'a3')).toBe(h.feeds[1]?.getAgent('a3'));
  });
  it('a retained list that never loaded takes in no created agents and reads none', async () => {
    const h = createHarness([agent('a1')]);
    const heard = vi.fn();
    h.store.retain(HUB, heard);
    await h.connect();
    h.server.agents.push(agent('a3', { _capabilities: OWN }));

    await h.emitAgent('created', created('a3'));

    expect(ids(h.store.peek(HUB))).toEqual([]);
    expect(h.server.agentFetches()).toBe(0);
    expect(heard).not.toHaveBeenCalled();

    const loaded = await h.store.ensure(HUB);
    expect(ids(loaded)).toEqual(['a1', 'a3']);
    expect(find(loaded, 'a3')?._capabilities).toEqual(OWN);
  });

  it('reads each agent of a burst of exactly the limit instead of revalidating', async () => {
    const h = await loadedHub();
    const burst = Array.from({ length: AGENT_READ_BURST_LIMIT }, (_, i) => `n${i}`);
    for (const id of burst) h.server.agents.push(agent(id, { _capabilities: OWN }));

    await createMany(h, burst);

    expect(h.server.agentFetches()).toBe(AGENT_READ_BURST_LIMIT);
    expect(h.server.walks()).toBe(1);
    for (const id of burst) expect(find(h.store.peek(HUB), id)?._capabilities).toEqual(OWN);
  });

  it('counts reads in flight toward the burst limit', async () => {
    const h = await loadedHub();
    const release = h.server.holdAgentReads();
    await createMany(h, ['n1', 'n2', 'n3', 'n4']);
    expect(h.server.agentFetches()).toBe(AGENT_READ_CONCURRENCY);

    await createMany(h, ['m1', 'm2', 'm3', 'm4', 'm5']);

    expect(h.server.walks()).toBe(2);
    release();
    await settle();
    expect(h.server.agentFetches()).toBe(AGENT_READ_CONCURRENCY);
  });

  it('drops the queued reads of a burst, so a later created agent reads only itself', async () => {
    const h = await loadedHub();
    const burst = Array.from({ length: AGENT_READ_BURST_LIMIT + 1 }, (_, i) => `n${i}`);
    for (const id of burst) h.server.agents.push(agent(id, { _capabilities: OWN }));
    await createMany(h, burst);
    expect(h.server.walks()).toBe(2);

    h.server.agents.push(agent('late', { _capabilities: OWN }));
    await h.emitAgent('created', created('late'));

    expect(h.server.agentFetches()).toBe(1);
    expect(h.server.agentFetches('late')).toBe(1);
    expect(h.server.walks()).toBe(2);
    expect(find(h.store.peek(HUB), 'late')?._capabilities).toEqual(OWN);
  });

  it('queues an agent once while its read waits, even when another list adds it', async () => {
    const h = await loadedHub();
    const release = h.server.holdAgentReads();
    await createMany(h, ['n1', 'n2', 'n3', 'n4', 'n5']);
    expect(h.server.agentFetches('n5')).toBe(0);

    await h.store.ensure(P1);
    for (const id of ['n1', 'n2', 'n3', 'n4', 'n5']) {
      h.server.agents.push(agent(id, { _capabilities: OWN }));
    }
    await h.emitAgent('status', { agentId: 'n5', phase: 'stopped' });
    expect(ids(h.store.peek(P1))).toContain('n5');

    release();
    await settle();
    expect(h.server.agentFetches('n5')).toBe(1);
    expect(find(h.store.peek(P1), 'n5')?._capabilities).toEqual(OWN);
  });

  it('a read settling after the feed was replaced leaves the reads of the new feed alone', async () => {
    const h = await loadedHub();
    const release = h.server.holdAgentReads({ ignoreAbort: true });
    await createMany(h, ['n1', 'n2', 'n3', 'n4']);

    // The new feed reads n1 again, with m4 queued behind its four reads.
    h.events.dispatchEvent(new CustomEvent('scion:membership-changed'));
    await h.connect();
    const next = ['n1', 'm1', 'm2', 'm3', 'm4'];
    await createMany(h, next);
    for (const id of next) h.server.agents.push(agent(id, { _capabilities: OWN }));
    expect(h.server.agentFetches('n1')).toBe(2);
    expect(h.server.agentFetches('m4')).toBe(0);

    release();
    await settle();

    expect(h.feeds).toHaveLength(2);
    expect(h.server.agentFetches('m4')).toBe(1);
    for (const id of next) expect(find(h.store.peek(HUB), id)?._capabilities).toEqual(OWN);
  });

  it('ends the seed epoch of a read that fails or is cancelled', async () => {
    const begin = vi.spyOn(StateManager.prototype, 'beginSeedEpoch');
    const end = vi.spyOn(StateManager.prototype, 'endSeedEpoch');
    const h = await loadedHub();
    const walkEpochs = begin.mock.results.length;

    // a3 is not on the server, so its read fails.
    await h.emitAgent('created', created('a3'));
    h.server.holdAgentReads();
    await h.emitAgent('created', created('a4'));
    h.store.reset('test');
    await settle();

    expect(h.server.agentFetches()).toBe(2);
    const readEpochs = begin.mock.results.slice(walkEpochs).map((r) => r.value as symbol);
    const ended = end.mock.calls.map(([token]) => token);
    expect(readEpochs).toHaveLength(2);
    for (const token of readEpochs) expect(ended).toContain(token);
  });
});

describe('AgentStore rows shared across lists', () => {
  it('a project walk merges into the hub rows and keeps fields only the hub list carries', async () => {
    const h = createHarness([
      agent('a1', { _messageability: { canMessage: true } } as Partial<Agent>),
    ]);
    h.server.projectRow = ({ _messageability: _omitted, ...row }): Agent => row as Agent;
    h.store.retain(HUB, () => {});
    h.store.retain(P1, () => {});
    const hub = h.store.ensure(HUB);
    await h.connect();
    await hub;
    await h.store.ensure(P1);

    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped' });

    const row = find(h.store.peek(HUB), 'a1');
    expect(row?.phase).toBe('stopped');
    expect(row?._messageability).toEqual({ canMessage: true });
    expect(find(h.store.peek(P1), 'a1')).toBe(row);
  });

  it('a full hub walk replaces feed rows, so a field the hub stopped sending is gone', async () => {
    const h = createHarness(
      [agent('a1', { _messageability: { canMessage: true } } as Partial<Agent>)],
      { view: 'full' }
    );
    h.store.retain(HUB, () => {});
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    h.server.agents = [agent('a1')];
    h.store.invalidate('manual');
    await settle();

    expect(h.server.walks()).toBe(2);
    expect(h.feeds[0]?.getAgent('a1')?._messageability).toBeUndefined();
    expect(find(h.store.peek(HUB), 'a1')?._messageability).toBeUndefined();
  });
});

describe('AgentStore seeded walks', () => {
  it('a delta that arrives during an in-flight walk survives the walk snapshot', async () => {
    const h = createHarness([agent('a1', { phase: 'running' })]);
    const release = h.server.pause();
    const loading = h.store.ensure(HUB);
    await h.connect();

    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped' });
    release();
    const snapshot = await loading;

    expect(find(snapshot, 'a1')?.phase).toBe('stopped');
    expect(h.feeds[0]?.getAgent('a1')?.phase).toBe('stopped');
  });

  it('a delete during an in-flight walk is not resurrected by the walk', async () => {
    const h = createHarness([agent('a1'), agent('a2')]);
    const release = h.server.pause();
    const loading = h.store.ensure(HUB);
    await h.connect();

    await h.emitAgent('deleted', { agentId: 'a2' });
    release();
    const snapshot = await loading;

    expect(ids(snapshot)).toEqual(['a1']);
    expect(h.feeds[0]?.getAgent('a2')).toBeUndefined();
  });

  it('a delta during a revalidation walk survives the walk snapshot', async () => {
    const h = createHarness([agent('a1', { phase: 'running' })]);
    h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    const release = h.server.pause();

    h.store.invalidate('manual');
    await settle();
    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped' });
    release();
    await settle();

    expect(h.server.walks()).toBe(2);
    expect(find(h.store.peek(HUB), 'a1')?.phase).toBe('stopped');
  });

  it('wraps every walk in a seed epoch: first load, resync, membership, access-denied, stale and retry', async () => {
    const begin = vi.spyOn(StateManager.prototype, 'beginSeedEpoch');
    const seed = vi.spyOn(StateManager.prototype, 'seedAgents');
    const h = createHarness([agent('a1')], { connectTimeoutMs: 5_000 });
    h.store.retain(HUB, () => {});
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    h.stream().drop();
    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();
    h.events.dispatchEvent(new CustomEvent('scion:membership-changed'));
    await h.connect();
    h.events.dispatchEvent(
      new CustomEvent('scion:access-denied', { detail: { resource: 'agent', action: 'read' } })
    );
    await h.connect();
    h.server.status = 500;
    h.store.invalidate('manual');
    await settle();
    h.server.status = 200;
    await h.store.ensure(HUB);

    expect(h.server.walks()).toBe(6);
    expect(begin).toHaveBeenCalledTimes(6);
    const tokened = seed.mock.calls.filter(([, opts]) => opts?.token !== undefined);
    expect(tokened).toHaveLength(5);
  });

  it('after a feed disconnect, the next walk waits for the feed to reconnect', async () => {
    const h = createHarness([agent('a1')]);
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    const requests = h.server.requests.length;

    h.stream().drop();
    const again = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(1_200);
    expect(h.server.requests).toHaveLength(requests);

    await h.connect();
    await again;
    expect(h.server.requests).toHaveLength(requests + 1);
  });
});

describe('AgentStore freshness while the feed is down', () => {
  it('ensure revalidates instead of answering from memory while the feed is down', async () => {
    const h = createHarness([agent('a1')]);
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    h.stream().drop();
    h.server.agents.push(agent('a2'));
    const again = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();

    expect(ids(await again)).toEqual(['a1', 'a2']);
    expect(h.server.walks()).toBe(2);
  });

  it('a refresh during an in-flight walk with the feed down produces exactly one follow-up walk', async () => {
    const h = createHarness([agent('a1')]);
    const release = h.server.pause();
    const first = h.store.ensure(HUB);
    await h.connect();

    h.stream().drop();
    const refresh = h.store.ensure(HUB);
    const refreshAgain = h.store.ensure(HUB);
    release();
    await settle();
    expect(h.server.walks()).toBe(1);

    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();
    const results = await Promise.all([first, refresh, refreshAgain]);

    expect(h.server.walks()).toBe(2);
    expect(results.every((s) => s.status === 'ready')).toBe(true);
  });

  it('a feed drop while a walk reads pages holds its waiter for one more walk after reconnect', async () => {
    const h = createHarness([agent('a1')]);
    const release = h.server.pause();
    let resolved = false;
    const loading = h.store.ensure(HUB).then((s) => {
      resolved = true;
      return s;
    });
    await h.connect();

    h.stream().drop();
    release();
    await settle();
    expect(h.server.walks()).toBe(1);
    expect(resolved).toBe(false);

    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();
    await loading;
    expect(h.server.walks()).toBe(2);
  });

  it('a refresh joining a walk that started without the feed produces exactly one follow-up walk', async () => {
    const h = createHarness([agent('a1')], { connectTimeoutMs: 5_000 });
    const release = h.server.pause();
    const first = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(h.server.walks()).toBe(1);

    const refresh = h.store.ensure(HUB);
    const refreshAgain = h.store.ensure(HUB);
    release();
    await h.connect();
    const results = await Promise.all([first, refresh, refreshAgain]);

    expect(h.server.walks()).toBe(2);
    expect(results.every((s) => s.status === 'ready')).toBe(true);
  });
});

describe('AgentStore freshness after a resync', () => {
  it('a loaded list nobody retains walks again on its next ensure after a resync', async () => {
    const h = createHarness([agent('a1')]);
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    h.stream().drop();
    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();
    expect(h.server.walks()).toBe(1);
    h.server.agents.push(agent('a2'));

    expect(ids(await h.store.ensure(HUB))).toEqual(['a1', 'a2']);
    expect(h.server.walks()).toBe(2);
  });
});

describe('AgentStore feed completeness flag', () => {
  it('a hub walk with the feed connected sets the compact flag', async () => {
    const h = createHarness([agent('a1')]);
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);
    expect(h.feeds[0]?.isAgentSetComplete('full')).toBe(false);
  });

  it('a full-view hub walk with the feed connected sets the full flag', async () => {
    const h = createHarness([agent('a1')], { view: 'full' });
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    expect(h.feeds[0]?.isAgentSetComplete('full')).toBe(true);
  });

  // `isAgentSetComplete('compact')` is also true for a full flag, so each
  // negative below checks it in both views.
  describe.each(VIEWS)('in %s view', (view) => {
    it('a hub walk cut off by the page bound does not set the flag', async () => {
      const h = createHarness([agent('a1'), agent('a2'), agent('a3')], {
        view,
        pageSize: 1,
        maxPages: 2,
      });
      const loading = h.store.ensure(HUB);
      await h.connect();
      await loading;

      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);
    });

    it('a hub walk does not set the flag when the feed drops before it finishes', async () => {
      const h = createHarness([agent('a1')], { view });
      const release = h.server.pause();
      const loading = h.store.ensure(HUB);
      await h.connect();

      h.stream().drop();
      release();
      await settle();

      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);
      void loading.catch(() => {});
    });

    it('a hub walk spanning a feed drop and reconnect does not set the flag', async () => {
      const h = createHarness([agent('a1')], { view });
      const release = h.server.pause();
      const loading = h.store.ensure(HUB);
      await h.connect();

      h.stream().drop();
      await vi.advanceTimersByTimeAsync(1_200);
      await h.connect();
      release();
      const releaseFollowUp = h.server.pause();
      await settle();

      expect(h.server.walks()).toBe(2);
      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);

      releaseFollowUp();
      await loading;
      expect(h.feeds[0]?.isAgentSetComplete(view)).toBe(true);
    });

    it('a complete walk of a filtered hub list does not set the flag', async () => {
      const h = createHarness([agent('a1')], { view });
      const loads = [
        h.store.ensure({ scope: 'hub', ownership: 'mine' }),
        h.store.ensure({ scope: 'hub', label: 'team=a' }),
      ];
      await h.connect();
      const snapshots = await Promise.all(loads);

      expect(snapshots.every((s) => s.complete)).toBe(true);
      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);
    });

    it('a hub walk that started after the connect timeout does not set the flag when the feed connects mid-walk', async () => {
      const h = createHarness([agent('a1')], { view, connectTimeoutMs: 5_000 });
      const release = h.server.pause();
      const loading = h.store.ensure(HUB);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(h.server.walks()).toBe(1);

      await h.connect();
      const releaseFollowUp = h.server.pause();
      release();
      await loading;

      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);

      // The list is stale, so the next ensure walks again with the feed up.
      const again = h.store.ensure(HUB);
      releaseFollowUp();
      await again;
      expect(h.server.walks()).toBe(2);
      expect(h.feeds[0]?.isAgentSetComplete(view)).toBe(true);
    });

    it('a project walk does not set the flag', async () => {
      const h = createHarness([agent('a1')], { view });
      const loading = h.store.ensure(P1);
      await h.connect();
      await loading;

      expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);
    });
  });

  it('a loading snapshot and an SSE-created row do not set the flag', async () => {
    const h = createHarness([agent('a1')]);
    h.store.retain(HUB, () => {});
    const release = h.server.pause();
    const loading = h.store.ensure(HUB);
    await h.connect();

    await h.emitAgent('created', created('a2'));
    expect(h.store.peek(HUB)?.status).toBe('loading');
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);
    release();
    await loading;
  });

  it('reset discards the feed instead of clearing its flag in place', async () => {
    const h = createHarness([agent('a1')]);
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    h.store.reset('test');
    const again = h.store.ensure(HUB);

    expect(h.feeds).toHaveLength(2);
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);
    expect(h.feeds[1]?.isAgentSetComplete('compact')).toBe(false);
    await h.connect();
    await again;
  });

  it('never reads the flag', async () => {
    const read = vi.spyOn(StateManager.prototype, 'isAgentSetComplete');
    const h = createHarness([agent('a1')]);
    h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    await h.emitAgent('created', created('a2'));
    h.store.invalidate('manual');
    await settle();

    expect(read).not.toHaveBeenCalled();
  });
});

describe('AgentStore compact rows', () => {
  it('a compact walk merges into full rows in the feed and never strips full fields', async () => {
    const h = createHarness([agent('a1', { phase: 'stopped' })]);
    h.store.retain(HUB, () => {});
    h.feeds[0]?.seedAgents([agent('a1', { harnessConfig: 'claude', phase: 'running' })]);
    const loading = h.store.ensure(HUB);
    await h.connect();
    const snapshot = await loading;

    expect(h.server.requests).toEqual(['/api/v1/agents?view=compact&limit=200']);
    const row = h.feeds[0]?.getAgent('a1');
    expect(row?.harnessConfig).toBe('claude');
    expect(row?.phase).toBe('stopped');
    expect(find(snapshot, 'a1')).toBe(row);
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);
    expect(h.feeds[0]?.isAgentSetComplete('full')).toBe(false);
  });
});

describe('AgentStore eviction', () => {
  const P2 = { scope: 'project', projectId: 'p2' } as const;

  it('keeps tombstones when an entry is evicted, so a later seed cannot bring a deleted agent back', async () => {
    const h = createHarness([agent('a1'), agent('a2'), agent('b1', { projectId: 'p2' })]);
    h.store.retain(P2, () => {});
    const loads = [h.store.ensure(P1), h.store.ensure(P2)];
    await h.connect();
    await Promise.all(loads);

    await h.emitAgent('deleted', { agentId: 'a2' });
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(h.store.peek(P1)).toBeUndefined();
    expect(h.feeds[0]?.getAgent('a1')).toBeUndefined();
    expect(h.feeds[0]?.getAgent('b1')).toBeDefined();

    // The server still lists a2 (a stale read); the feed's tombstone wins.
    const again = await h.store.ensure(P1);

    expect(h.feeds).toHaveLength(1);
    expect(ids(again)).toEqual(['a1']);
    expect(h.feeds[0]?.getAgent('a2')).toBeUndefined();
  });

  it('evicting the hub entry keeps its rows in a feed marked complete, so the flag stays true', async () => {
    const h = createHarness([agent('a1'), agent('b1', { projectId: 'p2' })]);
    h.store.retain(P2, () => {});
    const loads = [h.store.ensure(HUB), h.store.ensure(P2)];
    await h.connect();
    await Promise.all(loads);
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);

    await vi.advanceTimersByTimeAsync(5 * 60_000);

    expect(h.store.peek(HUB)).toBeUndefined();
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);
    expect(h.feeds[0]?.getAgent('a1')).toBeDefined();
    expect(h.feeds[0]?.getAgent('b1')).toBeDefined();
  });
});

describe('AgentStore eviction while the feed holds the hub set', () => {
  const P2 = { scope: 'project', projectId: 'p2' } as const;

  it('evicting a project entry keeps its rows too', async () => {
    const h = createHarness([agent('a1'), agent('b1', { projectId: 'p2' })]);
    h.store.retain(P2, () => {});
    const loads = [h.store.ensure(HUB), h.store.ensure(P2)];
    await h.connect();
    await Promise.all(loads);
    await vi.advanceTimersByTimeAsync(1_000);
    await h.store.ensure(P1);

    // The hub entry goes first, then the project entry that also held a1.
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(h.store.peek(HUB)).toBeUndefined();
    expect(h.store.peek(P1)).toBeUndefined();

    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(true);
    expect(h.feeds[0]?.getAgent('a1')).toBeDefined();
  });

  it('a feed opened after an idle close does not hold the hub set until a hub walk marks it', async () => {
    const h = createHarness([agent('a1'), agent('b1', { projectId: 'p2' })]);
    const hub = h.store.ensure(HUB);
    await h.connect();
    await hub;
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.feeds[0]?.isConnected).toBe(false);

    h.store.retain(P2, () => {});
    const p1 = h.store.ensure(P1);
    expect(h.feeds).toHaveLength(2);
    await h.connect();
    await p1;
    expect(h.feeds[1]?.getAgent('a1')).toBeDefined();

    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(h.store.peek(P1)).toBeUndefined();
    expect(h.feeds[1]?.getAgent('a1')).toBeUndefined();
  });
});

describe('AgentStore tombstones across feeds', () => {
  it('an agent deleted before the feed is replaced does not return from a stale walk on the new feed', async () => {
    const h = createHarness([agent('a1'), agent('a2')]);
    h.store.retain(HUB, () => {});
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    await h.emitAgent('deleted', { agentId: 'a2' });
    // The server still lists a2 (a stale read).
    h.events.dispatchEvent(new CustomEvent('scion:membership-changed'));
    await h.connect();

    expect(h.feeds).toHaveLength(2);
    expect(h.server.walks()).toBe(2);
    expect(ids(h.store.peek(HUB))).toEqual(['a1']);
    expect(h.feeds[1]?.getAgent('a2')).toBeUndefined();
  });

  it('an agent deleted before the feed closed idle does not return on the next feed', async () => {
    const h = createHarness([agent('a1'), agent('a2')]);
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    await h.emitAgent('deleted', { agentId: 'a2' });

    await vi.advanceTimersByTimeAsync(60_000);
    const again = h.store.ensure(HUB);
    expect(h.feeds).toHaveLength(2);
    await h.connect();

    expect(ids(await again)).toEqual(['a1']);
    expect(h.feeds[1]?.getAgent('a2')).toBeUndefined();
  });

  it('reset forgets the tombstones of earlier feeds', async () => {
    const h = createHarness([agent('a1'), agent('a2')]);
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    await h.emitAgent('deleted', { agentId: 'a2' });

    h.store.reset('test');
    const again = h.store.ensure(HUB);
    await h.connect();

    expect(ids(await again)).toEqual(['a1', 'a2']);
  });
});
