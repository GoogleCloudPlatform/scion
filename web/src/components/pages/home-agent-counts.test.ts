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
 * Home's agent counts and the completeness flag it shares with the agents
 * page: which navigations reuse the state store, which send the agents
 * request alone and which send today's full load; the live counts above
 * 500 agents; and count-only mode above 2,000. Uses the real
 * `stateManager`; only `fetch`, `EventSource` and `localStorage` are faked.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, Project } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import type { AgentMemberIndex } from '../../client/agent-member-index.js';
import type { AgentListWindow } from '../../client/agent-list-window.js';
import {
  FakeEventSource,
  SCOPE_CAPS,
  fakeFetch,
  holdable,
  isGlobalAgentsList,
  makeAgent,
  type Fake,
} from './__fixtures__/global-agents-endpoint.js';

type TestEl = HTMLElement & { updateComplete: Promise<boolean> };

interface PageInternals {
  loading?: boolean;
  agentsLoading?: boolean;
  countsLoading?: boolean;
  memberIndex?: AgentMemberIndex | null;
  agents: Agent[];
  agentWindow?: AgentListWindow;
  setScope?(scope: 'all' | 'mine' | 'shared'): void;
}

function internals(el: TestEl): PageInternals {
  return el as unknown as PageInternals;
}

const USER = { id: 'u', email: 'u@example.com', name: 'Uma Admin', role: 'admin' };

/**
 * Waits until the page has no load in flight, runs any pending live flush,
 * re-renders, and checks it is still idle, so a zero-request assertion
 * never races a late request.
 */
/** Calls to the stubbed fetch, and how many of them have not settled yet. */
function fetchState(): { calls: number; pending: number } {
  const mock = vi.mocked(globalThis.fetch);
  if (!vi.isMockFunction(mock)) return { calls: 0, pending: 0 };
  const settled = mock.mock.settledResults.filter((r) => r.type !== 'incomplete').length;
  return { calls: mock.mock.calls.length, pending: mock.mock.calls.length - settled };
}

/**
 * Wait until the page is idle: no loading flag set and every fetch settled
 * (apart from `allowPending()` requests a test holds on purpose), and no new
 * fetch was sent while the page took its responses in and re-rendered.
 */
async function settle(el: TestEl, allowPending: () => number = () => 0): Promise<void> {
  const idle = (): void => {
    const i = internals(el);
    expect(i.loading ?? false).toBe(false);
    expect(i.agentsLoading ?? false).toBe(false);
    expect(i.countsLoading ?? false).toBe(false);
    expect(fetchState().pending).toBeLessThanOrEqual(allowPending());
  };
  for (;;) {
    await vi.waitFor(idle);
    const before = fetchState().calls;
    // Yield one macrotask so response bodies are read and handled.
    await new Promise((resolve) => setTimeout(resolve, 0));
    (stateManager as unknown as { flush(): void }).flush();
    await el.updateComplete;
    if (fetchState().calls === before) {
      idle();
      return;
    }
  }
}

async function mountPage(
  tag: 'scion-page-home' | 'scion-page-agents',
  allowPending?: () => number
): Promise<TestEl> {
  const el = document.createElement(tag) as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: tag === 'scion-page-home' ? '/' : '/agents',
    title: 'Page',
    user: USER,
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await settle(el, allowPending);
  return el;
}

/** Mount a page, let it load, and leave it (a client navigation away). */
async function visit(tag: 'scion-page-home' | 'scion-page-agents'): Promise<TestEl> {
  const el = await mountPage(tag);
  el.remove();
  return el;
}

/**
 * Drops and reopens the live connection through the state store's own SSE
 * client, so the resync goes through the real state path.
 */
function reconnect(): void {
  stateManager.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
  stateManager.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
}

/** The projects list page: dashboard scope, loads projects only. */
function visitProjects(fake: Fake): void {
  stateManager.setScope({ type: 'dashboard' });
  stateManager.seedProjects((fake.projects ?? []) as Project[]);
}

/** A project detail page: a project scope, which clears the state store. */
function visitProjectPage(): void {
  stateManager.setScope({ type: 'project', projectId: 'p-1' });
}

function commitLabel(el: TestEl, value: string): void {
  const input = Array.from(el.shadowRoot?.querySelectorAll('sl-input') ?? []).find(
    (i) => i.getAttribute('placeholder') === 'Filter by label (key=value)'
  ) as HTMLElement & { value: string };
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  input.dispatchEvent(new Event('sl-change'));
}

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

/** Runs state.ts's pending coalesced flush now, then lets the page re-render. */
async function flushLive(el: TestEl): Promise<void> {
  (stateManager as unknown as { flush(): void }).flush();
  await Promise.resolve();
  await el.updateComplete;
}

function activeCount(el: TestEl): string {
  return el.shadowRoot?.querySelector('.stat-card .stat-value span')?.textContent?.trim() ?? '';
}

function statValues(el: TestEl): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.stat-card .stat-value') ?? []).map(
    (n) => n.textContent?.trim() ?? ''
  );
}

function text(el: TestEl): string {
  return el.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';
}

function countsChip(el: TestEl): HTMLElement | null {
  return el.shadowRoot?.querySelector('.counts-chip') ?? null;
}

function newFake(count: number, running = count): Fake {
  return {
    agents: Array.from({ length: count }, (_, i) =>
      makeAgent(i, { phase: i < running ? 'running' : 'stopped' })
    ),
    requests: [],
    otherRequests: [],
    projects: [{ id: 'p-1', name: 'P1' }],
  };
}

/**
 * Request counts across `run`: agents GETs, and the other GETs of home's
 * load (projects, invite stats). The shared project-capabilities lookup
 * (`/api/v1/projects?limit=1`, today's reuse-branch call) is not counted.
 */
async function cost(fake: Fake, run: () => Promise<unknown>): Promise<[number, number]> {
  const others = () => fake.otherRequests!.filter((u) => !u.includes('limit=1')).length;
  const a = fake.requests.length;
  const o = others();
  await run();
  return [fake.requests.length - a, others() - o];
}

describe('home agent counts and the shared completeness flag', () => {
  beforeAll(async () => {
    await import('./home.js');
    await import('./agents.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    stateManager.setScope({ type: 'brokers-list' });
    resetHubProjectCapabilitiesCache();
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'list');
    vi.setConfig({ testTimeout: 30_000 });
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-home, scion-page-agents').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  describe('home load', () => {
    it('sends one agents request with limit=1, fit=500 and stats, plus projects and invite stats; a complete response sets full', async () => {
      const fake = newFake(25, 10);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(fake.requests).toEqual([
        '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1',
      ]);
      expect(fake.otherRequests).toEqual(['/api/v1/projects', '/api/v1/admin/invites/stats']);
      expect(activeCount(el)).toBe('10');
      expect(internals(el).agents.length).toBe(25);
      expect(internals(el).memberIndex).toBeNull();
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(statValues(el)).toEqual(['10', '1', '3', '11']);
    });

    it('above 500 the counts come from stats and stay live under the dashboard add rule; the flag is not set', async () => {
      const fake = newFake(1200, 100);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(fake.requests.length).toBe(1);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      expect(activeCount(el)).toBe('100');
      expect(stateManager.getAgents().length).toBe(1);

      // A create in any project counts.
      const created = makeAgent(7000, { projectId: 'p-elsewhere', phase: 'running' });
      handleUpdate(`agent.${created.id}.created`, { ...created, agentId: created.id });
      await flushLive(el);
      expect(activeCount(el)).toBe('101');

      // A running member stops (not in state: an unknown-ID delta).
      handleUpdate('agent.g-00050.status', { agentId: 'g-00050', phase: 'stopped' });
      await flushLive(el);
      expect(activeCount(el)).toBe('100');

      // A running member is deleted.
      handleUpdate('agent.g-00051.deleted', {});
      await flushLive(el);
      expect(activeCount(el)).toBe('99');

      // A delta for an ID outside the index is ignored.
      handleUpdate('agent.zzz.status', { agentId: 'zzz', phase: 'running' });
      await flushLive(el);
      expect(activeCount(el)).toBe('99');
      expect(fake.requests.length).toBe(1);
      expect(countsChip(el)).toBeNull();
    });

    it('a server without sorted mode gets today’s request once', async () => {
      const fake = newFake(25, 5);
      const inner = fakeFetch(fake);
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(raw, 'http://localhost');
          if (u.pathname === '/api/v1/agents') {
            fake.requests.push(raw);
            return Promise.resolve(
              new Response(JSON.stringify({ agents: fake.agents.slice(0, 1), nextCursor: '1' }), {
                status: 200,
                headers: { 'Content-Type': 'application/json' },
              })
            ).then((r) =>
              u.search ? r : new Response(JSON.stringify({ agents: fake.agents }), r)
            );
          }
          return inner(input, init);
        })
      );
      const el = await mountPage('scion-page-home');
      expect(fake.requests).toEqual([
        '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1',
        '/api/v1/agents',
      ]);
      expect(activeCount(el)).toBe('5');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
    });
  });

  describe('count-only mode above 2,000 agents', () => {
    it('shows the count as of last refresh; a live change shows the chip; a click is one limit=1 fit request that updates the count', async () => {
      const fake = newFake(2003, 40);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(internals(el).memberIndex?.countOnly).toBe(true);
      expect(activeCount(el)).toBe('40');
      expect(text(el)).toContain('2,003 agents, as of last refresh');
      expect(countsChip(el)).toBeNull();

      for (const change of [
        () => handleUpdate('agent.g-00001.deleted', {}),
        () => handleUpdate('agent.g-00002.status', { agentId: 'g-00002', phase: 'stopped' }),
        () => {
          const a = makeAgent(8000);
          handleUpdate(`agent.${a.id}.created`, { ...a, agentId: a.id });
        },
      ]) {
        const before = activeCount(el);
        change();
        await flushLive(el);
        expect(activeCount(el)).toBe(before);
        expect(countsChip(el)).not.toBeNull();
        fake.agents = fake.agents.filter((a) => a.id !== 'g-00001');
        fake.agents = fake.agents.map((a) =>
          a.id === 'g-00002' ? { ...a, phase: 'stopped' as const } : a
        );
        const [agents, others] = await cost(fake, async () => {
          countsChip(el)!.click();
          await settle(el);
        });
        expect([agents, others]).toEqual([1, 0]);
        expect(fake.requests.at(-1)).toBe(
          '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1'
        );
        expect(countsChip(el)).toBeNull();
      }
      expect(activeCount(el)).toBe('38');
      expect(text(el)).toContain('2,002 agents, as of last refresh');
    });
  });

  describe('navigation between home, the agents page and other pages', () => {
    it('home → /agents → home: no second home fetch; /agents → home → /agents: no second agents request', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-home');
      await visit('scion-page-agents');
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([0, 0]);
      expect(await cost(fake, () => visit('scion-page-agents'))).toEqual([0, 0]);
    });

    it('A = 0: /agents sets full, then home sends today’s load and shows the projects count and invite stats', async () => {
      const fake = newFake(0);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-agents');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([1, 2]);
      expect(statValues(el)).toEqual(['0', '1', '3', '11']);
    });

    it('entry on /agents (scope all, A > 0) then home: no home fetch', async () => {
      const fake = newFake(30);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-agents');
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([0, 0]);
      expect(activeCount(el)).toBe('30');
    });

    it('/agents (all) → label commit → home → /agents: no request on home or on the final /agents', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const agents = await mountPage('scion-page-agents');
      commitLabel(agents, 'env=prod');
      await settle(agents);
      agents.remove();
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([0, 0]);
      expect(await cost(fake, () => visit('scion-page-agents'))).toEqual([0, 0]);
    });

    it('persisted mine → label → all with the label committed → home: one home load that sets full; /agents then issues no request', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      localStorage.setItem('scion-scope-agents', 'mine');
      const agents = await mountPage('scion-page-agents');
      commitLabel(agents, 'env=prod');
      await settle(agents);
      internals(agents).setScope!('all');
      await settle(agents);
      agents.remove();
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      let home!: TestEl;
      expect(
        await cost(fake, async () => {
          home = await mountPage('scion-page-home');
        })
      ).toEqual([1, 2]);
      home.remove();
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(await cost(fake, () => visit('scion-page-agents'))).toEqual([0, 0]);
    });

    it('/projects → home with the flag not held sends only the agents request; home → /projects → home sends none', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      visitProjects(fake);
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([1, 0]);
      expect(activeCount(el)).toBe('25');
      el.remove();
      visitProjects(fake);
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([0, 0]);
    });

    it('home → project page → home fetches as today', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-home');
      visitProjectPage();
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([1, 2]);
    });

    it('a compact flag satisfies home', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      stateManager.setScope({ type: 'dashboard' });
      stateManager.seedAgents(fake.agents, { partial: true });
      stateManager.markAgentSetComplete('compact');
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([0, 0]);
      expect(activeCount(el)).toBe('25');
    });

    it('home’s incomplete load and its live changes do not clear the flag set by a complete load', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-agents');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      const agents = await mountPage('scion-page-agents');
      internals(agents).setScope!('mine');
      await settle(agents);
      agents.remove();
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      const resync = vi.fn();
      stateManager.addEventListener('agents-resync', resync);
      reconnect();
      stateManager.removeEventListener('agents-resync', resync);
      expect(resync).toHaveBeenCalledTimes(1);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      // Home after the reconnect still reuses the complete set.
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([0, 0]);
    });

    it('home reached with no fetch shows today’s placeholders: 0 projects and -- on both invite cards', async () => {
      const fake = newFake(30, 12);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      await visit('scion-page-agents');
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([0, 0]);
      // The projects and invite stats are not loaded on this path, as today.
      expect(statValues(el)).toEqual(['12', '0', '--', '--']);
    });

    it('entry on /agents with server-rendered agents discards them and loads its own complete set; home then sends nothing', async () => {
      const fake = newFake(30, 9);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      // The server-rendered payload: a subset of the agents plus the scope
      // capabilities, hydrated before the page connects.
      stateManager.setScope({ type: 'dashboard' });
      stateManager.hydrate({ agents: fake.agents.slice(0, 5) }, SCOPE_CAPS);
      expect(stateManager.getAgents()).toHaveLength(5);
      expect(stateManager.isAgentSetComplete('full')).toBe(false);

      const agents = await mountPage('scion-page-agents');
      expect(fake.requests).toHaveLength(1);
      expect(fake.requests[0]).toContain('fit=500');
      expect(internals(agents).agentWindow!.display).toHaveLength(30);
      expect(stateManager.getAgents()).toHaveLength(30);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      agents.remove();

      let home!: TestEl;
      expect(
        await cost(fake, async () => {
          home = await mountPage('scion-page-home');
        })
      ).toEqual([0, 0]);
      expect(activeCount(home)).toBe('9');
      expect(internals(home).agents).toHaveLength(30);
    });

    for (const [count, running] of [
      [25, 10],
      [1200, 100],
    ] as const) {
      it(`${count} agents: home mounted while the agents page load is in flight sends its own load and never counts a partial set`, async () => {
        const fake = newFake(count, running);
        const h = holdable(fakeFetch(fake), isGlobalAgentsList);
        h.hold(1);
        vi.stubGlobal('fetch', vi.fn(h.fn));

        // Open /agents and leave it while its first request is still held.
        const agentsEl = document.createElement('scion-page-agents') as TestEl;
        (agentsEl as unknown as { pageData: unknown }).pageData = {
          path: '/agents',
          title: 'Page',
          user: USER,
        };
        document.body.appendChild(agentsEl);
        await agentsEl.updateComplete;
        await vi.waitFor(() => expect(h.heldCount).toBe(1));
        agentsEl.remove();

        // Leaving /agents aborts its load.
        expect(h.sent[0].signal?.aborted).toBe(true);
        const heldPending = (): number => (h.sent[0].signal?.aborted ? 0 : 1);
        const home = await mountPage('scion-page-home', heldPending);
        // Home's own one-request load, sent while the /agents one is held.
        expect(fake.requests).toEqual([
          '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1',
        ]);
        expect(activeCount(home)).toBe(String(running));

        h.release();
        // The aborted /agents response is released late and changes nothing.
        await settle(home);
        expect(fake.requests).toHaveLength(1);
        await flushLive(home);
        expect(activeCount(home)).toBe(String(running));
      });
    }
  });

  describe('home request counts at 25, 500 and 1,200 agents', () => {
    // [agents requests, other requests] for each home navigation.
    const rows: Array<{
      row: string;
      run: (fake: Fake) => Promise<[number, number]>;
      expected: Record<number, [number, number]>;
    }> = [
      {
        row: 'home load (entry)',
        run: (fake) => cost(fake, () => visit('scion-page-home')),
        expected: { 25: [1, 2], 500: [1, 2], 1200: [1, 2] },
      },
      {
        row: 'home revisit after a home load',
        run: async (fake) => {
          await visit('scion-page-home');
          return cost(fake, () => visit('scion-page-home'));
        },
        // Above 500 the flag is not held: the agents request alone.
        expected: { 25: [0, 0], 500: [0, 0], 1200: [1, 0] },
      },
      {
        row: 'home revisit after a held drain on /agents',
        run: async (fake) => {
          localStorage.setItem('scion-view-agents', 'graph');
          await visit('scion-page-agents');
          return cost(fake, () => visit('scion-page-home'));
        },
        expected: { 25: [0, 0], 500: [0, 0], 1200: [0, 0] },
      },
      {
        row: 'home revisit, flag not held (/agents with persisted mine), projects in state',
        run: async (fake) => {
          visitProjects(fake);
          localStorage.setItem('scion-scope-agents', 'mine');
          await visit('scion-page-agents');
          return cost(fake, () => visit('scion-page-home'));
        },
        expected: { 25: [1, 0], 500: [1, 0], 1200: [1, 0] },
      },
      {
        row: 'home revisit, flag not held, no projects in state',
        run: async (fake) => {
          localStorage.setItem('scion-scope-agents', 'mine');
          await visit('scion-page-agents');
          return cost(fake, () => visit('scion-page-home'));
        },
        expected: { 25: [1, 2], 500: [1, 2], 1200: [1, 2] },
      },
      {
        row: '/projects → home, flag not held',
        run: (fake) => {
          visitProjects(fake);
          return cost(fake, () => visit('scion-page-home'));
        },
        expected: { 25: [1, 0], 500: [1, 0], 1200: [1, 0] },
      },
      {
        row: 'home after a project page',
        run: async (fake) => {
          await visit('scion-page-home');
          visitProjectPage();
          return cost(fake, () => visit('scion-page-home'));
        },
        expected: { 25: [1, 2], 500: [1, 2], 1200: [1, 2] },
      },
    ];

    for (const count of [25, 500, 1200]) {
      it(`${count} agents`, async () => {
        const got: string[] = [];
        const want: string[] = [];
        for (const r of rows) {
          stateManager.setScope({ type: 'brokers-list' });
          localStorage.clear();
          localStorage.setItem('scion-view-agents', 'list');
          const fake = newFake(count);
          vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
          got.push(`${r.row}: ${(await r.run(fake)).join('/')}`);
          want.push(`${r.row}: ${r.expected[count].join('/')}`);
        }
        expect(got).toEqual(want);
      }, 120_000);
    }
  });
});
