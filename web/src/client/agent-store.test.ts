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
 * Agent store: coalescing, abort semantics, errors, eviction, feed lifetime
 * and the window-level refetch triggers.
 */

// @vitest-environment happy-dom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { agent, createHarness, settle } from './__fixtures__/agent-store-harness.js';
import { AgentStore, agentQueryKey } from './agent-store.js';
import type { AgentListSnapshot } from './agent-store.js';
import { apiFetch } from './api.js';
import { StateManager, stateManager } from './state.js';
import { MEMBERSHIP_CHANGED_EVENT } from '../utils/membership-events.js';
import { ACCOUNT_TEARDOWN_EVENT } from '../utils/auth.js';

vi.mock('./api.js', () => ({ apiFetch: vi.fn() }));

const HUB = { scope: 'hub' } as const;
const P1 = { scope: 'project', projectId: 'p1' } as const;

function many(n: number, projectId = 'p1'): ReturnType<typeof agent>[] {
  return Array.from({ length: n }, (_, i) => agent(`a${i}`, { projectId }));
}

function ids(snapshot: AgentListSnapshot | undefined): string[] {
  return (snapshot?.agents ?? []).map((a) => a.id);
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

describe('agentQueryKey', () => {
  it('builds canonical keys with server filters in a fixed order', () => {
    expect(agentQueryKey({ scope: 'hub' })).toBe('hub');
    expect(agentQueryKey({ scope: 'hub', ownership: 'mine' })).toBe('hub?ownership=mine');
    expect(agentQueryKey({ scope: 'hub', ownership: 'mine', label: 'team=a' })).toBe(
      'hub?label=team%3Da&ownership=mine'
    );
    expect(agentQueryKey({ scope: 'project', projectId: 'p1' })).toBe('project:p1');
    expect(agentQueryKey({ scope: 'project', projectId: 'p1', label: ' k=v ' })).toBe(
      'project:p1?label=k%3Dv'
    );
  });
});

describe('AgentStore coalescing', () => {
  it('concurrent ensure calls for one key share a single walk', async () => {
    const h = createHarness(many(5), { pageSize: 2 });

    const first = h.store.ensure(HUB);
    const second = h.store.ensure(HUB);
    await h.connect();
    const [a, b] = await Promise.all([first, second]);

    expect(h.server.walks()).toBe(1);
    expect(h.server.requests).toHaveLength(3);
    expect(a).toBe(b);
    expect(a.status).toBe('ready');
    expect(a.complete).toBe(true);
    expect(ids(a)).toEqual(['a0', 'a1', 'a2', 'a3', 'a4']);
  });

  it('ensure on a ready entry answers from memory with no request', async () => {
    const h = createHarness(many(3));
    const loading = h.store.ensure(HUB);
    await h.connect();
    const first = await loading;
    const requests = h.server.requests.length;

    const second = await h.store.ensure(HUB);

    expect(second).toBe(first);
    expect(h.server.requests).toHaveLength(requests);
  });

  it('requests every page at the store page size of 200', async () => {
    const h = createHarness(many(3));
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    expect(h.server.requests).toEqual(['/api/v1/agents?view=compact&limit=200']);
  });

  it('one caller aborting detaches only that caller; the walk continues for the others', async () => {
    const h = createHarness(many(3));
    const release = h.server.pause();
    const leaving = new AbortController();
    const detached = h.store.ensure(HUB, { signal: leaving.signal }).catch((e: unknown) => e);
    const staying = h.store.ensure(HUB);
    await h.connect();

    leaving.abort();
    const err = await detached;
    release();
    const snapshot = await staying;

    expect((err as DOMException).name).toBe('AbortError');
    expect(h.server.fetch.mock.calls[0]?.[1].signal?.aborted).toBe(false);
    expect(ids(snapshot)).toEqual(['a0', 'a1', 'a2']);
    expect(h.server.walks()).toBe(1);
  });

  it('the last waiter leaving with no retainer aborts the walk', async () => {
    const h = createHarness(many(3));
    const release = h.server.pause();
    const leaving = new AbortController();
    const detached = h.store.ensure(HUB, { signal: leaving.signal }).catch((e: unknown) => e);
    await h.connect();
    const pageSignal = h.server.fetch.mock.calls[0]?.[1].signal;

    leaving.abort();
    await detached;
    release();
    await settle();

    expect(pageSignal?.aborted).toBe(true);
    expect(h.store.peek(HUB)?.status).toBe('idle');

    const again = await h.store.ensure(HUB);
    expect(h.server.walks()).toBe(2);
    expect(ids(again)).toEqual(['a0', 'a1', 'a2']);
  });

  it('a retained entry keeps its walk when the last waiter leaves', async () => {
    const h = createHarness(many(2));
    const seen: AgentListSnapshot[] = [];
    h.store.retain(HUB, (s) => seen.push(s));
    const release = h.server.pause();
    const leaving = new AbortController();
    const detached = h.store.ensure(HUB, { signal: leaving.signal }).catch((e: unknown) => e);
    await h.connect();

    leaving.abort();
    await detached;
    release();
    await settle();

    expect(h.server.fetch.mock.calls[0]?.[1].signal?.aborted).toBe(false);
    expect(seen[seen.length - 1]?.status).toBe('ready');
    expect(ids(seen[seen.length - 1])).toEqual(['a0', 'a1']);
  });

  it('a walk error rejects every waiter and the next ensure walks again', async () => {
    const h = createHarness(many(2));
    h.server.status = 500;
    const first = h.store.ensure(HUB).catch((e: unknown) => e);
    const second = h.store.ensure(HUB).catch((e: unknown) => e);
    await h.connect();

    const [e1, e2] = await Promise.all([first, second]);
    expect(e1).toBeInstanceOf(Error);
    expect(e2).toBe(e1);
    expect(h.store.peek(HUB)?.status).toBe('error');
    expect(h.store.peek(HUB)?.agents).toEqual([]);

    h.server.status = 200;
    const retried = await h.store.ensure(HUB);

    expect(retried.status).toBe('ready');
    expect(ids(retried)).toEqual(['a0', 'a1']);
    expect(h.server.walks()).toBe(2);
  });

  it('a walk cut off by the page bound is ready but not complete', async () => {
    const h = createHarness(many(5), { pageSize: 2, maxPages: 2 });
    const loading = h.store.ensure(HUB);
    await h.connect();
    const snapshot = await loading;

    expect(snapshot.status).toBe('ready');
    expect(snapshot.complete).toBe(false);
    expect(ids(snapshot)).toEqual(['a0', 'a1', 'a2', 'a3']);
  });

  it('a failed revalidation keeps the previous rows with an error status', async () => {
    const h = createHarness(many(2));
    const seen: AgentListSnapshot[] = [];
    h.store.retain(HUB, (s) => seen.push(s));
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    h.server.status = 503;
    h.store.invalidate('manual');
    await settle();

    const last = seen[seen.length - 1];
    expect(last?.status).toBe('error');
    expect(last?.complete).toBe(false);
    expect(ids(last)).toEqual(['a0', 'a1']);
  });

  it('a revalidation publishes a loading, incomplete snapshot that keeps the rows', async () => {
    const h = createHarness(many(2));
    const seen: AgentListSnapshot[] = [];
    h.store.retain(HUB, (s) => seen.push(s));
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    const release = h.server.pause();

    h.store.invalidate('manual');
    await settle();

    const during = seen[seen.length - 1];
    expect(during?.status).toBe('loading');
    expect(during?.complete).toBe(false);
    expect(ids(during)).toEqual(['a0', 'a1']);
    release();
    await settle();
    expect(seen[seen.length - 1]?.complete).toBe(true);
  });

  it('reports progress as loading, never-complete snapshots of the rows so far', async () => {
    const h = createHarness(many(5), { pageSize: 2 });
    const progress: AgentListSnapshot[] = [];
    const loading = h.store.ensure(HUB, { onProgress: (s) => progress.push(s) });
    await h.connect();
    const done = await loading;

    expect(progress.map((s) => s.status)).toEqual(['loading', 'loading', 'loading', 'loading']);
    expect(progress.every((s) => !s.complete)).toBe(true);
    expect(progress.map((s) => s.agents.length)).toEqual([0, 2, 4, 5]);
    expect(done.complete).toBe(true);
  });

  it('a walk waits for the feed to connect before its first request', async () => {
    const h = createHarness(many(1));
    const loading = h.store.ensure(HUB);
    await settle();
    expect(h.server.requests).toHaveLength(0);

    await h.connect();
    await loading;
    expect(h.server.requests).toHaveLength(1);
  });

  it('walks after the connect timeout when the feed never connects, and keeps the entry stale', async () => {
    const h = createHarness(many(1), { connectTimeoutMs: 5_000 });
    const loading = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(5_000);
    const snapshot = await loading;

    expect(snapshot.status).toBe('ready');
    expect(h.server.walks()).toBe(1);
    expect(h.feeds[0]?.isAgentSetComplete('compact')).toBe(false);

    await h.connect();
    await h.store.ensure(HUB);
    expect(h.server.walks()).toBe(2);
  });

  it('after one connect timeout, a walk while the feed is still down does not wait again', async () => {
    const h = createHarness(many(1), { connectTimeoutMs: 5_000 });
    const first = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(5_000);
    await first;
    expect(h.server.walks()).toBe(1);

    let resolved = false;
    const second = h.store.ensure(HUB).then((s) => {
      resolved = true;
      return s;
    });
    await settle();

    expect(h.server.walks()).toBe(2);
    expect(resolved).toBe(true);
    expect((await second).status).toBe('ready');
  });

  it('once the feed connects after a timeout, a walk after a later drop waits for it again', async () => {
    const h = createHarness(many(1), { connectTimeoutMs: 5_000 });
    const first = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(5_000);
    await first;
    await h.connect();
    h.stream().drop();

    const again = h.store.ensure(HUB);
    await vi.advanceTimersByTimeAsync(1_200);
    expect(h.server.walks()).toBe(1);

    await h.connect();
    await again;
    expect(h.server.walks()).toBe(2);
  });

  it('a revalidation does not publish partial rows from its pages', async () => {
    const h = createHarness(many(3), { pageSize: 1 });
    const seen: AgentListSnapshot[] = [];
    h.store.retain(HUB, (s) => seen.push(s));
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    seen.length = 0;

    h.store.invalidate('manual');
    await settle();

    expect(h.server.walks()).toBe(2);
    expect(seen.map((s) => s.status)).toEqual(['loading', 'ready']);
    for (const s of seen) expect(ids(s)).toEqual(['a0', 'a1', 'a2']);
  });

  it('an aborted load that is still waiting for the feed leaves no connect timer behind', async () => {
    const h = createHarness(many(1), { connectTimeoutMs: 5_000 });
    const controller = new AbortController();
    const loading = h.store.ensure(HUB, { signal: controller.signal }).catch((e: unknown) => e);
    await settle();
    const before = vi.getTimerCount();

    controller.abort();
    expect(((await loading) as DOMException).name).toBe('AbortError');
    await settle();

    // The connect timer is cleared; the eviction grace and feed idle timers start.
    expect(vi.getTimerCount()).toBe(before + 1);
  });

  it('a listener that throws does not keep the others from hearing the change', async () => {
    const h = createHarness(many(1));
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    const seen: AgentListSnapshot[] = [];
    h.store.retain(HUB, () => {
      throw new Error('listener failed');
    });
    h.store.retain(HUB, (s) => seen.push(s));
    const loading = h.store.ensure(HUB);
    await h.connect();

    expect((await loading).status).toBe('ready');
    expect(seen.length).toBeGreaterThan(1);
    expect(seen[seen.length - 1]?.status).toBe('ready');
    expect(error).toHaveBeenCalled();
  });

  it('uses the server filter parameters for ownership, label and project lists', async () => {
    const h = createHarness([agent('a1', { projectId: 'p 1' })]);
    const loads = [
      h.store.ensure({ scope: 'hub', ownership: 'mine', label: 'team=a' }),
      h.store.ensure({ scope: 'project', projectId: 'p 1' }),
    ];
    await h.connect();
    await Promise.all(loads);

    expect(h.server.requests.sort()).toEqual([
      '/api/v1/agents?scope=mine&label=team%3Da&view=compact&limit=200',
      '/api/v1/projects/p%201/agents?view=compact&limit=200',
    ]);
  });
});

describe('AgentStore lifetime', () => {
  it('evicts an unused entry after the five-minute grace period', async () => {
    const h = createHarness(many(1));
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    await vi.advanceTimersByTimeAsync(4 * 60_000);
    expect(h.store.peek(HUB)).toBeDefined();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.store.peek(HUB)).toBeUndefined();
  });

  it('keeps at most eight unused entries, dropping the least recently used', async () => {
    const h = createHarness(many(1));
    const queries = Array.from({ length: 9 }, (_, i) => ({
      scope: 'project' as const,
      projectId: `p${i}`,
    }));
    const first = h.store.ensure(queries[0]);
    await h.connect();
    await first;
    for (const q of queries.slice(1)) {
      vi.advanceTimersByTime(1);
      await h.store.ensure(q);
    }

    expect(h.store.peek(queries[0])).toBeUndefined();
    for (const q of queries.slice(1)) expect(h.store.peek(q)).toBeDefined();
  });

  it('never evicts a retained entry', async () => {
    const h = createHarness(many(1));
    h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    await vi.advanceTimersByTimeAsync(10 * 60_000);
    expect(h.store.peek(HUB)?.status).toBe('ready');
  });

  it('closes the feed 60s after the last release and revalidates on the next ensure', async () => {
    const h = createHarness(many(1));
    const release = h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    const stream = h.stream();

    release();
    await vi.advanceTimersByTimeAsync(59_000);
    expect(stream.closed).toBe(false);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(stream.closed).toBe(true);

    const again = h.store.ensure(HUB);
    expect(h.feeds).toHaveLength(2);
    await h.connect();
    await again;
    expect(h.server.walks()).toBe(2);
  });

  it('after the feed closed idle, a retained entry is still revalidated once a new feed connects', async () => {
    const h = createHarness(many(1));
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    await vi.advanceTimersByTimeAsync(60_000);
    h.store.retain(HUB, () => {});
    expect(h.feeds).toHaveLength(2);
    await h.connect();
    await h.store.ensure(HUB);

    expect(h.server.walks()).toBe(2);
  });

  it('a revalidation aborted because every caller left keeps the entry stale for the next ensure', async () => {
    const h = createHarness(many(1));
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;

    const release = h.server.pause();
    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));
    const controller = new AbortController();
    const revalidating = h.store
      .ensure(HUB, { signal: controller.signal })
      .catch((e: unknown) => e);
    await h.connect();
    controller.abort();
    await revalidating;
    release();
    await settle();
    expect(h.store.peek(HUB)?.status).toBe('ready');

    await h.store.ensure(HUB);
    expect(h.server.walks()).toBe(3);
  });

  it('closing the feed detaches the store from it', async () => {
    const h = createHarness(many(1));
    h.store.retain(HUB, () => {});
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    const old = h.feeds[0];
    const removed = vi.spyOn(old, 'removeEventListener');

    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));

    const types = removed.mock.calls.map(([type]) => type);
    expect(types).toEqual(
      expect.arrayContaining(['agents-changed', 'agents-resync', 'connected', 'disconnected'])
    );
  });

  it('reset aborts walks, rejects waiters and discards the feed and entries', async () => {
    const h = createHarness(many(1));
    h.server.pause();
    const waiting = h.store.ensure(HUB).catch((e: unknown) => e);
    await h.connect();
    const stream = h.stream();
    const pageSignal = h.server.fetch.mock.calls[0]?.[1].signal;

    h.store.reset('test');

    expect(((await waiting) as DOMException).name).toBe('AbortError');
    expect(pageSignal?.aborted).toBe(true);
    expect(stream.closed).toBe(true);
    expect(h.store.peek(HUB)).toBeUndefined();
  });

  it('resets when the signed-in user changes, not when it is first known or unchanged', async () => {
    let user = '';
    const h = createHarness(many(1), { currentUserId: () => user });
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    user = 'u1';
    await h.store.ensure(HUB);
    h.store.retain(P1, () => {});
    expect(h.store.peek(HUB)).toBeDefined();
    expect(h.server.walks()).toBe(1);

    user = 'u2';
    h.store.retain(P1, () => {});
    expect(h.store.peek(HUB)).toBeUndefined();
  });

  it('a user change keeps retainers registered and walks their loaded lists again', async () => {
    let user = 'u1';
    const h = createHarness(many(1), { currentUserId: () => user });
    const heard: string[][] = [];
    const release = h.store.retain(HUB, (snapshot) => heard.push(ids(snapshot)));
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    user = 'u2';
    h.server.agents = [agent('x1')];
    h.store.retain(P1, () => {});
    expect(h.feeds).toHaveLength(2);
    await h.connect();

    expect(h.server.walks()).toBe(2);
    expect(ids(h.store.peek(HUB))).toEqual(['x1']);
    expect(heard[heard.length - 1]).toEqual(['x1']);

    release();
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(h.store.peek(HUB)).toBeUndefined();
  });

  it('reads the signed-in user from the global state manager by default', async () => {
    const h = createHarness(many(1));
    stateManager.setCurrentUserId('u1');
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;

    stateManager.setCurrentUserId('u2');
    h.store.retain(P1, () => {});
    expect(h.store.peek(HUB)).toBeUndefined();
    stateManager.setCurrentUserId('');
  });

  it('resets when the page unloads, but not when it enters the back/forward cache', async () => {
    const h = createHarness(many(1));
    h.store.retain(HUB, () => {});
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    const stream = h.stream();

    h.events.dispatchEvent(Object.assign(new Event('pagehide'), { persisted: true }));
    expect(stream.closed).toBe(false);
    expect(h.store.peek(HUB)).toBeDefined();

    h.events.dispatchEvent(Object.assign(new Event('pagehide'), { persisted: false }));
    expect(stream.closed).toBe(true);
    expect(h.store.peek(HUB)).toBeUndefined();
  });

  it('destroy stops listening for window triggers', async () => {
    const h = createHarness(many(1));
    h.store.destroy();
    const loading = h.store.ensure(HUB);
    await h.connect();
    await loading;
    const stream = h.stream();

    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));
    h.events.dispatchEvent(new Event('pagehide'));

    expect(h.feeds).toHaveLength(1);
    expect(stream.closed).toBe(false);
    expect(h.store.peek(HUB)).toBeDefined();
  });
});

describe('AgentStore refetch triggers', () => {
  async function twoRetained(): Promise<ReturnType<typeof createHarness>> {
    const h = createHarness([agent('a1'), agent('b1', { projectId: 'p2' })]);
    h.store.retain(HUB, () => {});
    h.store.retain(P1, () => {});
    const loads = [h.store.ensure(HUB), h.store.ensure(P1)];
    await h.connect();
    await Promise.all(loads);
    return h;
  }

  it('membership-changed revalidates each retained entry exactly once and reconnects the feed', async () => {
    const h = await twoRetained();
    const unretained = { scope: 'project' as const, projectId: 'p2' };
    await h.store.ensure(unretained);
    const oldStream = h.stream();

    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));
    expect(oldStream.closed).toBe(true);
    expect(h.feeds).toHaveLength(2);
    expect(h.stream().subjects).toEqual(['project.*.agent.>']);
    await h.connect();

    expect(h.server.walks('/api/v1/agents')).toBe(2);
    expect(h.server.walks('/api/v1/projects/p1/')).toBe(2);
    expect(h.server.walks('/api/v1/projects/p2/')).toBe(1);

    await h.store.ensure(unretained);
    expect(h.server.walks('/api/v1/projects/p2/')).toBe(2);
  });

  it('a walk in flight when membership changes restarts on the new feed and resolves its waiter', async () => {
    const h = createHarness([agent('a1')]);
    const release = h.server.pause();
    const loading = h.store.ensure(HUB);
    await h.connect();

    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));
    release();
    await h.connect();
    const snapshot = await loading;

    expect(h.server.walks()).toBe(2);
    expect(snapshot.agents.map((a) => a.id)).toEqual(['a1']);
    expect(h.feeds[1]?.getAgent('a1')).toBeDefined();
  });

  it('a denied read or list of an agent or project revalidates and reconnects; other denials do not', async () => {
    const h = await twoRetained();
    const oldStream = h.stream();

    for (const detail of [
      undefined,
      {},
      { action: 'read' },
      { resource: 'runtime_broker', action: 'read' },
      { resource: 'agent' },
      { resource: 'agent', action: 'stop' },
      { resource: 'agent', action: 'attach' },
      { resource: 'project', action: 'message' },
    ]) {
      h.events.dispatchEvent(new CustomEvent('scion:access-denied', { detail }));
    }
    await settle();
    expect(oldStream.closed).toBe(false);
    expect(h.feeds).toHaveLength(1);
    expect(h.server.walks()).toBe(2);

    h.events.dispatchEvent(
      new CustomEvent('scion:access-denied', { detail: { resource: 'agent', action: 'read' } })
    );
    expect(oldStream.closed).toBe(true);
    await h.connect();
    expect(h.server.walks('/api/v1/agents')).toBe(2);
    expect(h.server.walks('/api/v1/projects/p1/')).toBe(2);

    const replaced = h.stream();
    h.events.dispatchEvent(
      new CustomEvent('scion:access-denied', { detail: { resource: 'project', action: 'list' } })
    );
    expect(replaced.closed).toBe(true);
    await h.connect();
    expect(h.server.walks('/api/v1/agents')).toBe(3);
  });

  it('agents-resync from the feed revalidates retained entries once', async () => {
    const h = await twoRetained();

    h.stream().drop();
    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();

    expect(h.server.walks('/api/v1/agents')).toBe(2);
    expect(h.server.walks('/api/v1/projects/p1/')).toBe(2);
  });

  it('invalidating during a walk that is already fetching walks once more after it', async () => {
    const h = await twoRetained();
    const release = h.server.pause();
    h.store.invalidate('manual', (key) => key === 'hub');
    await settle();
    expect(h.server.walks('/api/v1/agents')).toBe(2);

    h.store.invalidate('manual', (key) => key === 'hub');
    h.store.invalidate('manual', (key) => key === 'hub');
    release();
    await settle();

    expect(h.server.walks('/api/v1/agents')).toBe(3);
  });

  it('a retained entry that was never loaded is not walked by a refetch trigger', async () => {
    const h = createHarness([agent('a1')]);
    h.store.retain(HUB, () => {});
    h.store.retain(P1, () => {});
    await h.connect();

    h.events.dispatchEvent(new CustomEvent(MEMBERSHIP_CHANGED_EVENT));
    await h.connect();
    h.stream().drop();
    await vi.advanceTimersByTimeAsync(1_200);
    await h.connect();

    expect(h.server.walks()).toBe(0);
  });

  it('unrelated events (chat messages, DMs) cause no request', async () => {
    const h = await twoRetained();
    const requests = h.server.requests.length;

    h.stream().emit('project.p1.chat.message', { id: 'm1' });
    h.stream().emit('user.me.chat.dm', { conversationKey: 'k' });
    h.stream().emit('notification.created', { id: 'n1' });
    h.events.dispatchEvent(new CustomEvent('scion:chat-message', { detail: {} }));
    await vi.advanceTimersByTimeAsync(200);
    await settle();

    expect(h.server.requests).toHaveLength(requests);
  });

  it('account teardown resets the store and closes the feed', async () => {
    const h = await twoRetained();
    const stream = h.stream();

    h.events.dispatchEvent(new CustomEvent(ACCOUNT_TEARDOWN_EVENT));

    expect(stream.closed).toBe(true);
    expect(h.store.peek(HUB)).toBeUndefined();
    expect(h.store.peek(P1)).toBeUndefined();
  });

  it('issues its own requests with the access-denied event suppressed', async () => {
    vi.stubGlobal(
      'EventSource',
      class {
        onopen: (() => void) | null = null;
        constructor() {
          queueMicrotask(() => this.onopen?.());
        }
        addEventListener(): void {}
        close(): void {}
      }
    );
    vi.mocked(apiFetch).mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ agents: [{ id: 'g1', name: 'g1', projectId: 'p1' }] }),
    } as unknown as Response);
    const setScope = vi.spyOn(StateManager.prototype, 'setScope');
    const store = new AgentStore({ events: null });

    const snapshot = await store.ensure(HUB);

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/agents?view=compact&limit=200',
      expect.objectContaining({ suppressAccessDeniedToast: true })
    );
    // The default feed is a StateManager of its own, never the app-wide one.
    expect(setScope).toHaveBeenCalledWith({ type: 'agent-feed' });
    const feed = setScope.mock.contexts[setScope.mock.calls.length - 1];
    expect(feed).toBeInstanceOf(StateManager);
    expect(feed).not.toBe(stateManager);
    // Its compact rows stay in that feed.
    expect(snapshot.agents.map((a) => a.id)).toEqual(['g1']);
    expect((feed as StateManager).getAgent('g1')).toBeDefined();
    expect(stateManager.getAgent('g1')).toBeUndefined();
    store.destroy();
  });
});
