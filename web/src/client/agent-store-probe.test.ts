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
 * Agent store: the delta probe that keeps retained lists current for
 * changes SSE does not carry.
 */

// @vitest-environment happy-dom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { agent, createHarness, settle, type Harness } from './__fixtures__/agent-store-harness.js';
import {
  AGENT_PROBE_INTERVAL_MS,
  AGENT_PROBE_JITTER_MS,
  AGENT_PROBE_LIMIT,
  AGENT_PROBE_REFUSED_RETRY_MS,
  AGENT_PROBE_TIMEOUT_MS,
  type AgentListSnapshot,
  type AgentQuery,
  type AgentStoreOptions,
} from './agent-store.js';
import type { Agent } from '../shared/types.js';
import { StateManager } from './state.js';

const HUB = { scope: 'hub' } as const;
const P1 = { scope: 'project', projectId: 'p1' } as const;
const CAPS = { actions: ['read', 'attach'] };

/** An ISO time `n` seconds into the test day. */
function t(n: number): string {
  return new Date(Date.UTC(2026, 0, 1, 0, 0, 0) + n * 1000).toISOString();
}

/** A row as the server lists it: with its time and per-item capabilities. */
function row(id: string, updated: number, extra: Partial<Agent> = {}): Agent {
  return agent(id, { updated: t(updated), _capabilities: CAPS, ...extra });
}

function ids(snapshot: AgentListSnapshot | undefined): string[] {
  return (snapshot?.agents ?? []).map((a) => a.id);
}

function find(snapshot: AgentListSnapshot | undefined, id: string): Agent | undefined {
  return snapshot?.agents.find((a) => a.id === id);
}

/** Retain and load `q`, with the feed connected. */
async function loaded(
  initial: Agent[],
  q: AgentQuery = HUB,
  options: Partial<AgentStoreOptions> = {}
): Promise<Harness> {
  const h = createHarness(initial, options);
  h.store.retain(q, () => {});
  const load = h.store.ensure(q);
  await h.connect();
  await load;
  return h;
}

/** The abort signal of the latest probe request. */
function probeSignal(h: Harness): AbortSignal | undefined {
  const calls = h.server.fetch.mock.calls.filter(([path]) => path.includes('sort='));
  return calls[calls.length - 1]?.[1].signal ?? undefined;
}

/** Run timers to the next probe and let it finish. */
async function tick(ms = AGENT_PROBE_INTERVAL_MS): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
  await settle();
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

describe('AgentStore delta probe', () => {
  it('adds a new agent and merges a renamed one with no walk and no agent read', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2)]);
    const a2 = find(h.store.peek(HUB), 'a2');

    h.server.agents[0] = row('a1', 10, { name: 'renamed' });
    h.server.agents.push(row('a3', 11));
    await tick();

    expect(h.server.probes()).toBe(1);
    expect(h.server.walks()).toBe(1);
    expect(h.server.agentFetches()).toBe(0);
    const snapshot = h.store.peek(HUB);
    expect(ids(snapshot).sort()).toEqual(['a1', 'a2', 'a3']);
    expect(find(snapshot, 'a1')?.name).toBe('renamed');
    expect(find(snapshot, 'a3')?._capabilities).toEqual(CAPS);
    // A row the probe lists unchanged keeps its identity.
    expect(find(snapshot, 'a2')).toBe(a2);
    expect(h.feeds[0].getAgent('a3')?.name).toBe('a3');
  });

  it('requests one compact page of the most recently updated rows', async () => {
    const h = await loaded([row('a1', 1)]);
    await tick();
    const probe = h.server.requests.find((p) => p.includes('sort='));
    const url = new URL(probe!, 'http://localhost');
    expect(url.pathname).toBe('/api/v1/agents');
    expect(Object.fromEntries(url.searchParams)).toEqual({
      sort: 'updated',
      dir: 'desc',
      limit: String(AGENT_PROBE_LIMIT),
      view: 'compact',
    });
  });

  it('probes a project list through the project endpoint', async () => {
    const h = await loaded([row('a1', 1), row('b1', 1, { projectId: 'p2' })], P1);
    h.server.agents.push(row('a2', 5));
    await tick();
    expect(h.server.probes('/api/v1/projects/p1/agents?')).toBe(1);
    expect(h.server.probes()).toBe(1);
    expect(ids(h.store.peek(P1)).sort()).toEqual(['a1', 'a2']);
  });

  it('probes once per interval, jittered by up to three seconds either way', async () => {
    const draws = [0, 0.99999];
    const h = await loaded([row('a1', 1)], HUB, { random: () => draws.shift() ?? 0.5 });

    await tick(AGENT_PROBE_INTERVAL_MS - AGENT_PROBE_JITTER_MS - 1);
    expect(h.server.probes()).toBe(0);
    await tick(1);
    expect(h.server.probes()).toBe(1);

    // Second draw: just under the interval plus the jitter.
    await tick(AGENT_PROBE_INTERVAL_MS + AGENT_PROBE_JITTER_MS - 2);
    expect(h.server.probes()).toBe(1);
    await tick(2);
    expect(h.server.probes()).toBe(2);
  });

  it('does not probe while the page is hidden, and resumes when it is visible', async () => {
    const h = await loaded([row('a1', 1)]);
    await tick(AGENT_PROBE_INTERVAL_MS - 1);
    h.visibility.set('hidden');
    await tick(5 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(0);

    h.visibility.set('visible');
    await tick(AGENT_PROBE_INTERVAL_MS - 1);
    expect(h.server.probes()).toBe(0);
    await tick(1);
    expect(h.server.probes()).toBe(1);
  });

  it('abandons a probe in flight when the page is hidden', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    h.server.agents.push(row('a2', 5));
    await tick();
    expect(h.server.probes()).toBe(1);
    h.visibility.set('hidden');
    expect(probeSignal(h)?.aborted).toBe(true);
    release();
    await settle();
    expect(ids(h.store.peek(HUB))).toEqual(['a1']);
  });

  it('does not probe a list nobody retains', async () => {
    const h = createHarness([row('a1', 1)]);
    const load = h.store.ensure(HUB);
    await h.connect();
    await load;
    await tick(3 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(0);
  });

  it('stops probing when the last retainer releases', async () => {
    const h = createHarness([row('a1', 1)]);
    const release = h.store.retain(HUB, () => {});
    const load = h.store.ensure(HUB);
    await h.connect();
    await load;
    await tick();
    expect(h.server.probes()).toBe(1);
    release();
    await tick(3 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(1);
  });

  it('does not probe a retained list that never loaded', async () => {
    const h = createHarness([row('a1', 1)]);
    h.store.retain(HUB, () => {});
    await h.connect();
    await tick(3 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(0);
  });

  it('does not probe a server-filtered list', async () => {
    const h = await loaded([row('a1', 1)], { scope: 'hub', ownership: 'mine' });
    h.store.retain({ scope: 'project', projectId: 'p1', label: 'team=a' }, () => {});
    await h.store.ensure({ scope: 'project', projectId: 'p1', label: 'team=a' });
    await tick(3 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(0);
  });

  it('stops probing after the store resets', async () => {
    const h = await loaded([row('a1', 1)]);
    h.store.reset('test');
    await tick(3 * AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes()).toBe(0);
  });

  it('skips a probe while a walk is in flight', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    h.store.invalidate('manual');
    await settle();
    expect(h.server.walks()).toBe(2);
    await tick();
    expect(h.server.probes()).toBe(0);
    release();
    await settle();
    await tick();
    expect(h.server.probes()).toBe(1);
  });

  it('drops a probe in flight when a walk starts', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    await tick();
    expect(h.server.probes()).toBe(1);
    h.store.invalidate('manual');
    h.server.agents.push(row('a2', 5));
    const seed = vi.spyOn(StateManager.prototype, 'seedAgents');
    release();
    await settle();
    // Only the walk seeds; the probe's response is discarded.
    expect(seed).toHaveBeenCalledTimes(1);
    expect(ids(h.store.peek(HUB)).sort()).toEqual(['a1', 'a2']);
  });

  it('starts probing when a list that already loaded is retained', async () => {
    const h = createHarness([row('a1', 1)]);
    const load = h.store.ensure(HUB);
    await h.connect();
    await load;
    h.store.retain(HUB, () => {});
    await tick();
    expect(h.server.probes()).toBe(1);
  });

  it('a reset during a probe aborts it, and a list retained again has one probe schedule', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    await tick();
    expect(h.server.probes()).toBe(1);
    h.store.reset('test');
    expect(probeSignal(h)?.aborted).toBe(true);
    release();
    await settle();

    h.store.retain(HUB, () => {});
    const load = h.store.ensure(HUB);
    await h.connect();
    await load;
    await tick();
    expect(h.server.probes()).toBe(2);
    await tick();
    expect(h.server.probes()).toBe(3);
  });

  it('fills a fresh feed with rows the list holds from before the feed closed', async () => {
    const h = createHarness([agent('a1')]);
    const release = h.store.retain(HUB, () => {});
    const load = h.store.ensure(HUB);
    await h.connect();
    await load;
    release();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.feeds[0].isConnected).toBe(false);

    h.store.retain(HUB, () => {});
    expect(h.feeds).toHaveLength(2);
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(h.feeds[1].getAgent('a1')?.id).toBe('a1');
  });

  it('adds to a project list an agent the feed already holds from a hub walk', async () => {
    const h = await loaded([row('a1', 1)], P1);
    h.server.agents.push(row('a2', 2));
    h.store.retain(HUB, () => {});
    await h.store.ensure(HUB);
    expect(ids(h.store.peek(P1))).toEqual(['a1']);
    const walks = h.server.walks();

    await tick();
    expect(ids(h.store.peek(P1)).sort()).toEqual(['a1', 'a2']);
    expect(h.server.walks()).toBe(walks);
  });

  describe('a probe adds rows only to the list it read', () => {
    const NO_DM = { _messageability: { canMessage: false } } as Partial<Agent>;
    const withoutMessageability = (a: Agent): Agent => {
      const copy: Agent & { _messageability?: unknown } = { ...a };
      delete copy._messageability;
      return copy;
    };

    it('a project probe does not add to the hub list a row the project endpoint renders', async () => {
      // The project list probes first, the hub list a few seconds later.
      const draws = [0, 1];
      const h = await loaded([row('a1', 1, NO_DM)], P1, {
        random: (): number => draws.shift() ?? 0.5,
      });
      h.server.projectRow = withoutMessageability;
      h.store.retain(HUB, () => {});
      await h.store.ensure(HUB);
      h.server.agents.push(row('a2', 5, NO_DM));

      await tick(AGENT_PROBE_INTERVAL_MS - AGENT_PROBE_JITTER_MS);
      expect(h.server.probes('/api/v1/projects/')).toBe(1);
      expect(ids(h.store.peek(P1)).sort()).toEqual(['a1', 'a2']);
      expect(ids(h.store.peek(HUB))).toEqual(['a1']);

      await tick(2 * AGENT_PROBE_JITTER_MS);
      expect(h.server.probes('/api/v1/agents?')).toBe(1);
      const a2 = find(h.store.peek(HUB), 'a2') as
        | (Agent & { _messageability?: unknown })
        | undefined;
      expect(a2?._messageability).toEqual({ canMessage: false });
    });

    it('a hub probe does not add to a project list, and updates the rows it holds', async () => {
      const h = await loaded([row('a1', 1)], P1);
      h.store.retain(HUB, () => {});
      await h.store.ensure(HUB);
      h.server.sortedStatus = (path): number | undefined =>
        path.startsWith('/api/v1/projects/') ? 422 : undefined;
      h.server.agents[0] = row('a1', 4, { name: 'renamed' });
      h.server.agents.push(row('a2', 5));
      await tick();

      expect(ids(h.store.peek(HUB)).sort()).toEqual(['a1', 'a2']);
      expect(ids(h.store.peek(P1))).toEqual(['a1']);
      expect(find(h.store.peek(P1), 'a1')?.name).toBe('renamed');
    });
  });

  it('measures the next probe from the newest row the last probe saw', async () => {
    const h = await loaded([row('a1', 1)]);
    h.server.agents.push(
      ...Array.from({ length: AGENT_PROBE_LIMIT + 10 }, (_, i) => row(`n${i}`, 100 + i))
    );
    await tick();
    expect(h.server.probes()).toBe(2);
    await tick();
    expect(h.server.probes()).toBe(3);
  });

  describe('a probe interrupted after its first page', () => {
    const renamedCount = 2 * AGENT_PROBE_LIMIT + 20;
    const initial = (): Agent[] =>
      Array.from({ length: renamedCount }, (_, i) => row(`a${i}`, 1 + i));
    const renameAll = (h: Harness): void => {
      h.server.agents = h.server.agents.map((a, i) =>
        row(a.id, 1000 + i, { name: `renamed-${a.id}` })
      );
    };
    const renamed = (h: Harness): number =>
      (h.store.peek(HUB)?.agents ?? []).filter((a) => a.name.startsWith('renamed-')).length;

    it('reads the same pages again after a later page fails', async () => {
      const h = await loaded(initial());
      renameAll(h);
      h.server.sortedStatus = (path): number | undefined =>
        path.includes('cursor=') ? 500 : undefined;
      await tick();
      h.server.sortedStatus = undefined;
      await tick();
      await tick();
      expect(renamed(h)).toBe(renamedCount);
    });

    it('reads the same pages again after the page is hidden mid-probe', async () => {
      const h = await loaded(initial());
      renameAll(h);
      h.server.sortedStatus = (path): undefined => {
        if (path.includes('cursor=')) h.visibility.set('hidden');
        return undefined;
      };
      await tick();
      h.server.sortedStatus = undefined;
      h.visibility.set('visible');
      await tick();
      await tick();
      expect(renamed(h)).toBe(renamedCount);
    });
  });

  it('stops at a full page without a next cursor', async () => {
    const h = await loaded([]);
    h.server.agents.push(
      ...Array.from({ length: AGENT_PROBE_LIMIT }, (_, i) => row(`n${i}`, 100 + i))
    );
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(h.server.walks()).toBe(1);
    expect(h.store.peek(HUB)?.agents).toHaveLength(AGENT_PROBE_LIMIT);
  });

  it('stops at a page whose last row is exactly as new as the last probe', async () => {
    const h = await loaded([row('a0', 0), row('a1', 1)]);
    h.server.agents.push(
      ...Array.from({ length: AGENT_PROBE_LIMIT - 1 }, (_, i) => row(`n${i}`, 100 + i))
    );
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(h.server.walks()).toBe(1);
  });

  it('follows further pages when the first is all newer than the last probe', async () => {
    const initial = [row('a1', 1), row('a2', 2), row('a3', 3)];
    const h = await loaded(initial);
    const burst = Array.from({ length: 2 * AGENT_PROBE_LIMIT + 20 }, (_, i) =>
      row(`n${i}`, 100 + i)
    );
    h.server.agents.push(...burst);
    await tick();

    expect(h.server.probes()).toBe(3);
    expect(h.server.walks()).toBe(1);
    expect(h.store.peek(HUB)?.agents).toHaveLength(initial.length + burst.length);
  });

  it('falls back to one full walk when five pages do not catch up', async () => {
    const h = await loaded([row('a1', 1)]);
    const burst = Array.from({ length: 6 * AGENT_PROBE_LIMIT }, (_, i) => row(`n${i}`, 100 + i));
    h.server.agents.push(...burst);
    await tick();

    expect(h.server.probes()).toBe(5);
    expect(h.server.walks()).toBe(2);
    const snapshot = h.store.peek(HUB);
    expect(snapshot?.status).toBe('ready');
    expect(snapshot?.agents).toHaveLength(1 + burst.length);
  });

  describe('a list whose rows carry no time', () => {
    it('reads one page after an empty walk and walks for the count', async () => {
      const h = await loaded([]);
      h.server.agents.push(
        ...Array.from({ length: 6 * AGENT_PROBE_LIMIT }, (_, i) => row(`n${i}`, 100 + i))
      );
      await tick();
      expect(h.server.probes()).toBe(1);
      expect(h.server.walks()).toBe(2);
      expect(h.store.peek(HUB)?.agents).toHaveLength(6 * AGENT_PROBE_LIMIT);
    });

    it('reads one page per probe and does not walk once the count matches', async () => {
      const timeless = (id: string): Agent => agent(id, { _capabilities: CAPS });
      const h = await loaded([timeless('a1')]);
      h.server.agents.push(
        ...Array.from({ length: 6 * AGENT_PROBE_LIMIT }, (_, i) => timeless(`n${i}`))
      );
      await tick();
      await tick();
      await tick();
      expect(h.server.probes()).toBe(3);
      expect(h.server.walks()).toBe(2);
      expect(h.store.peek(HUB)?.agents).toHaveLength(1 + 6 * AGENT_PROBE_LIMIT);
    });
  });

  describe('sustained churn the probe cannot catch up with', () => {
    const fleet = (): Agent[] => Array.from({ length: 400 }, (_, i) => row(`a${i}`, 1));

    /** Heartbeat every row before each probe for `intervals`; the minutes walks started at. */
    async function churn(h: Harness, intervals: number, from = 0): Promise<number[]> {
      const walkMinutes: number[] = [];
      for (let i = 1; i <= intervals; i++) {
        h.server.heartbeat(t(1000 + (from + i) * 30));
        const walks = h.server.walks();
        await tick();
        if (h.server.walks() > walks) walkMinutes.push(((from + i) * 30) / 60);
      }
      return walkMinutes;
    }

    it('walks on overflow at most at doubling intervals, and probes read one page meanwhile', async () => {
      const h = await loaded(fleet());
      const requests = h.server.requests.length;
      const walkMinutes = await churn(h, 120);

      expect(walkMinutes).toEqual([0.5, 2.5, 6.5, 14.5, 30.5, 46.5]);
      // Each overflowing probe reads five pages; each probe in between, one.
      expect(h.server.probes()).toBe(6 * 5 + (120 - 6));
      // One hour: 144 probe requests plus six walks of two pages.
      expect(h.server.requests.length - requests).toBe(h.server.probes() + 6 * 2);
    });

    it('walks at once again on overflow after a probe has caught up', async () => {
      const h = await loaded(fleet());
      expect(await churn(h, 14)).toEqual([0.5, 2.5, 6.5]);
      await tick();
      // Without the catch-up, the next walk would wait until 14.5 minutes.
      expect(await churn(h, 2, 15)).toEqual([8]);
    });
  });

  it('stops at a page that reaches the last probe even when that page is full', async () => {
    const initial = Array.from({ length: AGENT_PROBE_LIMIT + 5 }, (_, i) => row(`o${i}`, i));
    const h = await loaded(initial);
    h.server.agents.push(row('n1', 1000));
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(h.server.walks()).toBe(1);
  });

  it('walks once when the server count differs from the rows held, and not again for the same count', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2), row('a3', 3)]);
    // Revoked or deleted without an event.
    h.server.agents.splice(1, 1);
    await tick();
    expect(h.server.walks()).toBe(2);
    expect(ids(h.store.peek(HUB)).sort()).toEqual(['a1', 'a3']);

    await tick();
    expect(h.server.probes()).toBe(2);
    expect(h.server.walks()).toBe(2);
  });

  it('walks only once for a mismatch a walk does not resolve, and again when the count changes', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2)]);
    h.server.totalCount = 5;
    await tick();
    expect(h.server.walks()).toBe(2);
    await tick();
    await tick();
    expect(h.server.probes()).toBe(3);
    expect(h.server.walks()).toBe(2);

    h.server.totalCount = 6;
    await tick();
    expect(h.server.walks()).toBe(3);
  });

  it('a mismatch walks again after the counts have matched in between', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2)]);
    h.server.totalCount = 5;
    await tick();
    expect(h.server.walks()).toBe(2);
    h.server.totalCount = undefined;
    await tick();
    expect(h.server.walks()).toBe(2);
    h.server.totalCount = 5;
    await tick();
    expect(h.server.walks()).toBe(3);
  });

  it('a probe right after a walk finds the server count equal to the rows walked, on the hub and a project', async () => {
    const rows = [row('a1', 1), row('a2', 2), row('b1', 3, { projectId: 'p2' }), row('a3', 4)];
    const h = await loaded(rows, HUB, { pageSize: 2 });
    h.store.retain(P1, () => {});
    await h.store.ensure(P1);
    const walks = h.server.walks();
    await tick();
    expect(h.server.probes()).toBe(2);
    expect(h.server.walks()).toBe(walks);
  });

  it('does not count-check a list cut off by the page bound', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2), row('a3', 3)], HUB, {
      pageSize: 1,
      maxPages: 2,
    });
    expect(h.store.peek(HUB)?.complete).toBe(false);
    h.server.totalCount = 10;
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(h.server.walks()).toBe(1);
  });

  it('keeps an SSE delta that lands while the probe is in flight', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    // The probe's row is newer than the held one but older than the delta.
    h.server.agents[0] = row('a1', 10, { phase: 'running', name: 'renamed' });
    await tick();
    expect(h.server.probes()).toBe(1);
    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped', activity: 'completed' });
    release();
    await settle();

    const a1 = find(h.store.peek(HUB), 'a1');
    expect(a1?.phase).toBe('stopped');
    expect(a1?.name).toBe('renamed');
    expect(h.feeds[0].getAgent('a1')?.phase).toBe('stopped');
  });

  it('never brings back an agent deleted over SSE', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2)]);
    await h.emitAgent('deleted', { agentId: 'a1' });
    h.server.agents[0] = row('a1', 10);
    h.server.totalCount = 1;
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(ids(h.store.peek(HUB))).toEqual(['a2']);
    expect(h.feeds[0].getAgent('a1')).toBeUndefined();
  });

  it('never brings back an agent deleted on an earlier feed', async () => {
    const h = await loaded([row('a1', 1), row('a2', 2)]);
    await h.emitAgent('deleted', { agentId: 'a1' });
    h.events.dispatchEvent(new Event('scion:membership-changed'));
    await h.connect();
    h.server.agents[0] = row('a1', 10);
    h.server.totalCount = 1;
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(ids(h.store.peek(HUB))).toEqual(['a2']);
    expect(h.feeds[1].getAgent('a1')).toBeUndefined();
  });

  it('never sets the completeness flag', async () => {
    const h = await loaded([row('a1', 1)], P1);
    const mark = vi.spyOn(StateManager.prototype, 'markAgentSetComplete');
    h.server.agents.push(row('a2', 5));
    await tick();
    expect(h.server.probes()).toBe(1);
    expect(ids(h.store.peek(P1)).sort()).toEqual(['a1', 'a2']);
    expect(mark).not.toHaveBeenCalled();
    expect(h.feeds[0].isAgentSetComplete('compact')).toBe(false);
  });

  it('merges compact rows into full rows without dropping full fields', async () => {
    const full = row('a1', 1, { appliedConfig: { harness: 'claude' } } as Partial<Agent>);
    const h = await loaded([full]);
    h.server.agents[0] = row('a1', 10, { name: 'renamed' });
    await tick();
    const a1 = find(h.store.peek(HUB), 'a1') as Agent & { appliedConfig?: unknown };
    expect(a1.name).toBe('renamed');
    expect(a1.appliedConfig).toEqual({ harness: 'claude' });
  });

  it('pauses probing a project list whose sorted view the server refuses, and keeps probing the hub', async () => {
    const info = vi.mocked(console.info);
    const h = await loaded([row('a1', 1)], P1);
    h.store.retain(HUB, () => {});
    await h.store.ensure(HUB);
    h.server.sortedStatus = (path): number | undefined =>
      path.startsWith('/api/v1/projects/') ? 422 : undefined;
    await tick();
    expect(h.server.probes('/api/v1/projects/')).toBe(1);
    await tick(AGENT_PROBE_REFUSED_RETRY_MS - AGENT_PROBE_INTERVAL_MS);
    expect(h.server.probes('/api/v1/projects/')).toBe(1);
    expect(h.server.probes('/api/v1/agents?')).toBe(
      AGENT_PROBE_REFUSED_RETRY_MS / AGENT_PROBE_INTERVAL_MS
    );

    // Refused again after the pause: logged once, paused again.
    await tick();
    expect(h.server.probes('/api/v1/projects/')).toBe(2);
    const refusals = (): number =>
      info.mock.calls.filter(([message]) => String(message).includes('probing paused')).length;
    expect(refusals()).toBe(1);

    // Served after the next pause: probing resumes each interval.
    h.server.sortedStatus = undefined;
    h.server.agents.push(row('a2', 5));
    await tick(AGENT_PROBE_REFUSED_RETRY_MS);
    expect(h.server.probes('/api/v1/projects/')).toBe(3);
    expect(ids(h.store.peek(P1)).sort()).toEqual(['a1', 'a2']);
    await tick();
    expect(h.server.probes('/api/v1/projects/')).toBe(4);
    expect(refusals()).toBe(1);
  });

  it('abandons a probe that hangs and probes again on the next tick', async () => {
    const h = await loaded([row('a1', 1)]);
    const release = h.server.pause();
    await tick();
    expect(h.server.probes()).toBe(1);
    await tick(AGENT_PROBE_TIMEOUT_MS);
    await tick();
    expect(h.server.probes()).toBe(2);
    release();
    await settle();
  });

  it('resets for a different signed-in user before probing as them', async () => {
    let user = 'u1';
    const h = await loaded([row('a1', 1)], HUB, { currentUserId: () => user });
    const listed = h.store.peek(HUB);
    user = 'u2';
    h.server.agents.push(row('b1', 5));
    await tick();

    // The old user's list is not merged into; the new user's walk reads it.
    expect(ids(listed)).toEqual(['a1']);
    expect(h.server.probes()).toBe(0);
    expect(h.feeds).toHaveLength(2);
    await h.connect();
    expect(h.server.walks()).toBe(2);
    expect(ids(h.store.peek(HUB)).sort()).toEqual(['a1', 'b1']);
  });

  it('keeps probing after a failed probe', async () => {
    const h = await loaded([row('a1', 1)]);
    h.server.sortedStatus = (): number => 500;
    await tick();
    h.server.sortedStatus = undefined;
    h.server.agents.push(row('a2', 5));
    await tick();
    expect(h.server.probes()).toBe(2);
    expect(ids(h.store.peek(HUB)).sort()).toEqual(['a1', 'a2']);
  });
});
