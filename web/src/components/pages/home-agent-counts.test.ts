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
  jsonResponse,
  makeAgent,
  type Fake,
} from './__fixtures__/global-agents-endpoint.js';

type TestEl = HTMLElement & { updateComplete: Promise<boolean> };

interface PageInternals {
  loading?: boolean;
  agentsLoading?: boolean;
  countsLoading?: boolean;
  /** Home: the counts chip is raised (rendered unless the response was the complete set). */
  countsMayHaveChanged?: boolean;
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
    await vi.waitFor(idle, { timeout: 10_000 });
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

type PageTag =
  | 'scion-page-home'
  | 'scion-page-agents'
  | 'scion-page-projects'
  | 'scion-page-project-detail';

const PAGE_PATHS: Record<PageTag, string> = {
  'scion-page-home': '/',
  'scion-page-agents': '/agents',
  'scion-page-projects': '/projects',
  'scion-page-project-detail': '/projects/p-1',
};

async function mountPage(tag: PageTag, allowPending?: () => number): Promise<TestEl> {
  const el = document.createElement(tag) as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: PAGE_PATHS[tag],
    title: 'Page',
    user: USER,
  };
  if (tag === 'scion-page-project-detail') {
    (el as unknown as { projectId: string }).projectId = 'p-1';
  }
  document.body.appendChild(el);
  await el.updateComplete;
  await settle(el, allowPending);
  return el;
}

/** Mount a page, let it load, and leave it (a client navigation away). */
async function visit(tag: PageTag): Promise<TestEl> {
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

/**
 * Adds the project page's own endpoints for project p-1 (the project, and
 * a complete sorted answer for its agents) in front of `inner`; every
 * project agents request is recorded in `projectAgentRequests`.
 */
function withProjectPage(
  inner: ReturnType<typeof fakeFetch>,
  fake: Fake,
  projectAgentRequests: string[]
): ReturnType<typeof fakeFetch> {
  return (input, init) => {
    const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const u = new URL(raw, 'http://localhost');
    const method = (init?.method ?? 'GET').toUpperCase();
    if (u.pathname === '/api/v1/projects/p-1' && method === 'GET') {
      return Promise.resolve(jsonResponse({ id: 'p-1', name: 'P1', slug: 'p1' }));
    }
    if (u.pathname === '/api/v1/projects/p-1/agents' && method === 'GET') {
      projectAgentRequests.push(raw);
      const agents = fake.agents.filter((a) => a.projectId === 'p-1');
      return Promise.resolve(
        jsonResponse({
          agents,
          totalCount: agents.length,
          complete: true,
          _capabilities: SCOPE_CAPS,
        })
      );
    }
    return inner(input, init);
  };
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

// The timeout is a suite option: a test's timeout is fixed when it is collected, so setting it
// from a hook would not apply.
describe('home agent counts and the shared completeness flag', { timeout: 30_000 }, () => {
  beforeAll(async () => {
    await import('./home.js');
    await import('./agents.js');
    await import('./projects.js');
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    stateManager.setScope({ type: 'brokers-list' });
    resetHubProjectCapabilitiesCache();
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'list');
  });

  afterEach(() => {
    document.body
      .querySelectorAll(
        'scion-page-home, scion-page-agents, scion-page-projects, scion-page-project-detail'
      )
      .forEach((n) => n.remove());
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
    it('a server without sorted mode above 500 agents: today’s request once, counts from that page, and the flag is not set', async () => {
      const fake = newFake(1200, 700);
      const inner = fakeFetch(fake);
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(raw, 'http://localhost');
          // Sorted parameters are ignored: every agents request gets the
          // legacy list, which honours limit and cursor (default 500).
          if (u.pathname === '/api/v1/agents' && u.searchParams.get('sort')) {
            const legacy = new URL(u.href);
            for (const key of ['sort', 'dir', 'fit', 'stats', 'phase'])
              legacy.searchParams.delete(key);
            return inner(legacy.pathname + legacy.search, init);
          }
          return inner(input, init);
        })
      );
      const el = await mountPage('scion-page-home');
      expect(fake.requests).toEqual(['/api/v1/agents?limit=1', '/api/v1/agents']);
      // Counts from the 500-row page today's request returns.
      expect(internals(el).agents.length).toBe(500);
      expect(activeCount(el)).toBe('500');
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      el.remove();

      // The agents page cannot reuse that page: it sends its own load.
      const before = fake.requests.length;
      await visit('scion-page-agents');
      expect(fake.requests.length).toBeGreaterThan(before);
    });

    it('above 500 the counts include a phase change and a create that land while home’s request is in flight', async () => {
      const fake = newFake(1200, 100);
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      h.hold(1);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = document.createElement('scion-page-home') as TestEl;
      (el as unknown as { pageData: unknown }).pageData = { path: '/', title: 'Page', user: USER };
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(h.heldCount).toBe(1));

      // g-00500 is stopped in the stats the held answer will carry, and not
      // in the store: an unknown-ID delta.
      expect(stateManager.getAgent('g-00500')).toBeUndefined();
      handleUpdate('agent.g-00500.status', { agentId: 'g-00500', phase: 'running' });
      // A running agent created in flight: the held answer predates it.
      const created = makeAgent(7000, { projectId: 'p-elsewhere', phase: 'running' });
      handleUpdate(`agent.${created.id}.created`, { ...created, agentId: created.id });
      (stateManager as unknown as { flush(): void }).flush();

      h.release();
      await settle(el);
      expect(fake.requests).toHaveLength(1);
      expect(internals(el).memberIndex).not.toBeNull();
      expect(internals(el).memberIndex?.has('g-07000')).toBe(true);
      expect(activeCount(el)).toBe('102');
      // Exact counts raise no chip, and the flag behind it is off.
      expect(countsChip(el)).toBeNull();
      expect(internals(el).countsMayHaveChanged).toBe(false);
    });
  });

  describe('a resync of the live connection', () => {
    it('stats-ID mode: a resync that lands while home’s request is in flight shows the chip', async () => {
      const fake = newFake(1200, 100);
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      h.hold(1);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = document.createElement('scion-page-home') as TestEl;
      (el as unknown as { pageData: unknown }).pageData = { path: '/', title: 'Page', user: USER };
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      reconnect();
      h.release();
      await settle(el);
      expect(fake.requests).toHaveLength(1);
      expect(internals(el).memberIndex?.countOnly).toBe(false);
      expect(activeCount(el)).toBe('100');
      expect(countsChip(el)).not.toBeNull();
    });

    it('stats-ID mode: a resync after the load shows the chip; a click is one request that clears it', async () => {
      const fake = newFake(1200, 100);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(internals(el).memberIndex?.countOnly).toBe(false);
      expect(countsChip(el)).toBeNull();
      reconnect();
      await flushLive(el);
      expect(countsChip(el)).not.toBeNull();
      expect(fake.requests).toHaveLength(1);
      const [agents, others] = await cost(fake, async () => {
        countsChip(el)!.click();
        await settle(el);
      });
      expect([agents, others]).toEqual([1, 0]);
      expect(countsChip(el)).toBeNull();
      expect(activeCount(el)).toBe('100');
    });

    it('the complete set: a resync shows no chip', async () => {
      const fake = newFake(25, 10);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(internals(el).memberIndex).toBeNull();
      reconnect();
      await flushLive(el);
      expect(countsChip(el)).toBeNull();
      expect(internals(el).countsMayHaveChanged).toBe(false);
    });

    it('a home that left the page no longer reacts to a resync', async () => {
      const fake = newFake(2003, 40);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      expect(internals(el).memberIndex?.countOnly).toBe(true);
      expect(internals(el).countsMayHaveChanged).toBe(false);
      el.remove();
      reconnect();
      expect(internals(el).countsMayHaveChanged).toBe(false);
    });
  });

  describe('home load: changes to agents not in the store while the request is in flight', () => {
    async function mountHeld(fake: Fake): Promise<{ el: TestEl; h: ReturnType<typeof holdable> }> {
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      h.hold(1);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = document.createElement('scion-page-home') as TestEl;
      (el as unknown as { pageData: unknown }).pageData = { path: '/', title: 'Page', user: USER };
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      return { el, h };
    }

    it('a delta for an agent missing from the stats changes neither the total nor the running count', async () => {
      const fake = newFake(1200, 100);
      const { el, h } = await mountHeld(fake);
      handleUpdate('agent.g-09999.status', { agentId: 'g-09999', phase: 'running' });
      (stateManager as unknown as { flush(): void }).flush();
      h.release();
      await settle(el);
      expect(internals(el).memberIndex?.has('g-09999')).toBe(false);
      expect(internals(el).memberIndex?.stats).toEqual({ total: 1200, running: 100 });
      expect(activeCount(el)).toBe('100');
    });

    it('an agent changed while unknown, then created and changed again, counts with its latest phase', async () => {
      const fake = newFake(1200, 100);
      const { el, h } = await mountHeld(fake);
      // g-00500 is stopped in the stats the held answer will carry.
      handleUpdate('agent.g-00500.status', { agentId: 'g-00500', phase: 'running' });
      (stateManager as unknown as { flush(): void }).flush();
      const a = makeAgent(500, { phase: 'running' });
      handleUpdate('agent.g-00500.created', { ...a, agentId: a.id });
      handleUpdate('agent.g-00500.status', { agentId: 'g-00500', phase: 'stopped' });
      (stateManager as unknown as { flush(): void }).flush();
      expect(stateManager.getAgent('g-00500')?.phase).toBe('stopped');
      h.release();
      await settle(el);
      expect(internals(el).memberIndex?.getPhase('g-00500')).toBe('stopped');
      expect(activeCount(el)).toBe('100');
    });
  });

  describe('every home response keeps the live changes that land while it is in flight', () => {
    const rows = [
      // 25 agents, 10 running: the complete set.
      { name: 'the complete response', count: 25, running: 10, member: 5, idMode: false },
      // 1,200 agents, 100 running: counts from the stats IDs.
      { name: 'the stats-ID response', count: 1200, running: 100, member: 50, idMode: true },
      // A server without sorted mode: today's request, held, after the first.
      {
        name: 'today’s request to a server without sorted mode',
        count: 25,
        running: 10,
        member: 5,
        idMode: false,
        legacy: true,
      },
    ];

    describe.each(rows)('$name', (row) => {
      let el: TestEl;
      let h: ReturnType<typeof holdable>;
      let fake: Fake;
      const id = `g-${String(row.member).padStart(5, '0')}`;
      const total = (): number =>
        internals(el).memberIndex?.stats.total ?? internals(el).agents.length;

      beforeEach(async () => {
        fake = newFake(row.count, row.running);
        const inner = fakeFetch(fake);
        const serve: typeof inner = (input, init) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          // The sorted request gets a legacy page (no complete).
          if (row.legacy && new URL(raw, 'http://localhost').searchParams.has('sort')) {
            fake.requests.push(raw);
            return Promise.resolve(
              jsonResponse({ agents: fake.agents.slice(0, 1), nextCursor: '1' })
            );
          }
          return inner(input, init);
        };
        // Holds the request whose response the page adopts.
        h = holdable(serve, (u) => isGlobalAgentsList(u) && (!row.legacy || u.search === ''));
        h.hold(1);
        vi.stubGlobal('fetch', vi.fn(h.fn));
        el = document.createElement('scion-page-home') as TestEl;
        (el as unknown as { pageData: unknown }).pageData = {
          path: '/',
          title: 'Page',
          user: USER,
        };
        document.body.appendChild(el);
        await el.updateComplete;
        await vi.waitFor(() => expect(h.heldCount).toBe(1));
      });

      const done = async (): Promise<void> => {
        (stateManager as unknown as { flush(): void }).flush();
        h.release();
        await settle(el);
        expect(fake.requests).toHaveLength(row.legacy ? 2 : 1);
        expect(internals(el).memberIndex === null).toBe(!row.idMode);
        expect(countsChip(el)).toBeNull();
        expect(internals(el).countsMayHaveChanged).toBe(false);
      };

      it('with no live change the counts are the response’s', async () => {
        await done();
        expect(activeCount(el)).toBe(String(row.running));
        expect(total()).toBe(row.count);
      });

      it('a phase change to an agent the store holds counts', async () => {
        stateManager.seedAgents([makeAgent(row.member)]);
        handleUpdate(`agent.${id}.status`, { agentId: id, phase: 'stopped' });
        await done();
        expect(activeCount(el)).toBe(String(row.running - 1));
      });

      it('a phase change to an agent not in the store counts', async () => {
        expect(stateManager.getAgent(id)).toBeUndefined();
        handleUpdate(`agent.${id}.status`, { agentId: id, phase: 'stopped' });
        await done();
        expect(activeCount(el)).toBe(String(row.running - 1));
      });

      it('an activity change to a running agent not in the store leaves it running', async () => {
        expect(stateManager.getAgent(id)).toBeUndefined();
        handleUpdate(`agent.${id}.status`, { agentId: id, activity: 'thinking' });
        await done();
        expect(activeCount(el)).toBe(String(row.running));
        if (row.idMode) expect(internals(el).memberIndex?.getPhase(id)).toBe('running');
        else expect(internals(el).agents.find((a) => a.id === id)?.activity).toBe('thinking');
      });

      it('a create the response predates counts', async () => {
        const created = makeAgent(7000, { phase: 'running' });
        handleUpdate(`agent.${created.id}.created`, { ...created, agentId: created.id });
        await done();
        expect(activeCount(el)).toBe(String(row.running + 1));
        expect(total()).toBe(row.count + 1);
      });

      it('a delete leaves the agent out of the counts', async () => {
        handleUpdate(`agent.${id}.deleted`, {});
        await done();
        expect(activeCount(el)).toBe(String(row.running - 1));
        expect(total()).toBe(row.count - 1);
        expect(stateManager.getAgent(id)).toBeUndefined();
      });
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

  describe('count-only mode: changes while home’s request is in flight', () => {
    async function mountHeldCountOnly(): Promise<{
      el: TestEl;
      h: ReturnType<typeof holdable>;
      fake: Fake;
    }> {
      const fake = newFake(2003, 40);
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      h.hold(1);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = document.createElement('scion-page-home') as TestEl;
      (el as unknown as { pageData: unknown }).pageData = { path: '/', title: 'Page', user: USER };
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      return { el, h, fake };
    }

    const changes: Array<[string, () => void]> = [
      [
        'a status change of an agent not in the store',
        () => handleUpdate('agent.g-00002.status', { agentId: 'g-00002', phase: 'stopped' }),
      ],
      [
        'an activity change of an agent not in the store',
        () => handleUpdate('agent.g-00002.status', { agentId: 'g-00002', activity: 'thinking' }),
      ],
      [
        'a status change of an agent the store holds',
        () => {
          stateManager.seedAgents([makeAgent(3)]);
          handleUpdate('agent.g-00003.status', { agentId: 'g-00003', phase: 'stopped' });
        },
      ],
      ['a delete', () => handleUpdate('agent.g-00001.deleted', {})],
      ['a resync of the live connection', reconnect],
      [
        'a create',
        () => {
          const a = makeAgent(8000);
          handleUpdate(`agent.${a.id}.created`, { ...a, agentId: a.id });
        },
      ],
    ];

    for (const [name, change] of changes) {
      it(`count-only: ${name} that lands while home’s request is in flight shows the chip`, async () => {
        const { el, h, fake } = await mountHeldCountOnly();
        change();
        (stateManager as unknown as { flush(): void }).flush();
        h.release();
        await settle(el);
        expect(fake.requests).toHaveLength(1);
        expect(internals(el).memberIndex?.countOnly).toBe(true);
        expect(activeCount(el)).toBe('40');
        expect(countsChip(el)).not.toBeNull();
      });
    }

    it('count-only: a resync of the live connection after the load shows the chip', async () => {
      const { el, h, fake } = await mountHeldCountOnly();
      h.release();
      await settle(el);
      expect(countsChip(el)).toBeNull();
      reconnect();
      await flushLive(el);
      expect(countsChip(el)).not.toBeNull();
      expect(fake.requests).toHaveLength(1);
    });

    it('count-only: with no change in flight the chip stays hidden', async () => {
      const { el, h } = await mountHeldCountOnly();
      h.release();
      await settle(el);
      expect(internals(el).memberIndex?.countOnly).toBe(true);
      expect(countsChip(el)).toBeNull();
    });
  });

  describe('home load guards', () => {
    it('a second chip click while the first refresh is in flight sends no request', async () => {
      const fake = newFake(2003, 40);
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mountPage('scion-page-home');
      expect(internals(el).memberIndex?.countOnly).toBe(true);
      handleUpdate('agent.g-00002.status', { agentId: 'g-00002', phase: 'stopped' });
      await flushLive(el);
      expect(countsChip(el)).not.toBeNull();

      h.hold(1);
      const before = fake.requests.length;
      countsChip(el)!.click();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      await el.updateComplete;
      expect(internals(el).countsLoading).toBe(true);
      expect(countsChip(el)).not.toBeNull();
      countsChip(el)!.click();
      await el.updateComplete;
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(h.sent.length).toBe(2);
      expect(h.heldCount).toBe(1);

      h.release();
      await settle(el);
      expect(fake.requests.length - before).toBe(1);
      expect(countsChip(el)).toBeNull();
    });

    it('when two loads overlap, the later load’s response wins even if it arrives first', async () => {
      const fake = newFake(2003, 40);
      // Each agents request waits for its own gate; the fake answers with
      // whatever its agents are when the gate opens.
      const gates: Array<() => void> = [];
      const inner = fakeFetch(fake);
      const gated: typeof inner = async (input, init) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (isGlobalAgentsList(new URL(raw, 'http://localhost'))) {
          await new Promise<void>((resolve) => gates.push(resolve));
        }
        return inner(input, init);
      };
      vi.stubGlobal('fetch', vi.fn(gated));
      // Projects in state and no agents: home sends the agents request alone.
      visitProjects(fake);
      const el = document.createElement('scion-page-home') as TestEl;
      (el as unknown as { pageData: unknown }).pageData = { path: '/', title: 'Page', user: USER };
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(gates).toHaveLength(1));
      // Leave and come back: the second visit sends its own load.
      el.remove();
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(gates).toHaveLength(2));

      // The later load answers first, with 2,005 agents, 45 running.
      fake.agents = newFake(2005, 45).agents;
      gates[1]();
      await settle(el, () => 1);
      expect(activeCount(el)).toBe('45');
      expect(text(el)).toContain('2,005 agents, as of last refresh');

      // The earlier load answers last, with 2,003 agents, 40 running: it is discarded.
      fake.agents = newFake(2003, 40).agents;
      gates[0]();
      await settle(el);
      expect(fake.requests).toHaveLength(2);
      expect(activeCount(el)).toBe('45');
      expect(text(el)).toContain('2,005 agents, as of last refresh');
    });
  });

  describe('home seeds full objects', () => {
    it('a field dropped by a later full-view load is gone from the store', async () => {
      const fake = newFake(2003, 40);
      fake.agents = fake.agents.map((a) => ({ ...a, taskSummary: 'old task' }));
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountPage('scion-page-home');
      const seeded = stateManager.getAgents();
      expect(seeded).toHaveLength(1);
      const id = seeded[0].id;
      expect(stateManager.getAgent(id)?.taskSummary).toBe('old task');

      fake.agents = fake.agents.map((a) => {
        const { taskSummary: _dropped, ...rest } = a;
        return rest as Agent;
      });
      handleUpdate('agent.g-00002.status', { agentId: 'g-00002', phase: 'stopped' });
      await flushLive(el);
      const [agents] = await cost(fake, async () => {
        countsChip(el)!.click();
        await settle(el);
      });
      expect(agents).toBe(1);
      expect(stateManager.getAgent(id)).toBeDefined();
      expect(stateManager.getAgent(id)?.taskSummary).toBeUndefined();
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
      // The real projects page: it loads projects and sends no agents request.
      const [agentsOnProjects, othersOnProjects] = await cost(fake, () =>
        visit('scion-page-projects')
      );
      expect(agentsOnProjects).toBe(0);
      expect(othersOnProjects).toBe(1);
      expect(stateManager.getProjects().map((p) => p.id)).toEqual(['p-1']);
      let el!: TestEl;
      expect(
        await cost(fake, async () => {
          el = await mountPage('scion-page-home');
        })
      ).toEqual([1, 0]);
      expect(activeCount(el)).toBe('25');
      el.remove();
      // Projects and their capabilities are in state now, so the projects
      // page reuses them.
      expect(await cost(fake, () => visit('scion-page-projects'))).toEqual([0, 0]);
      expect(await cost(fake, () => visit('scion-page-home'))).toEqual([0, 0]);
    });

    it('home → project page → home fetches as today', async () => {
      const fake = newFake(25);
      const projectAgentRequests: string[] = [];
      vi.stubGlobal('fetch', vi.fn(withProjectPage(fakeFetch(fake), fake, projectAgentRequests)));
      await visit('scion-page-home');
      // The real project page: its own project-scoped agents request only.
      const page = await mountPage('scion-page-project-detail');
      expect(fake.requests).toHaveLength(1);
      expect(projectAgentRequests).toHaveLength(1);
      expect(stateManager.currentScope).toEqual({ type: 'project', projectId: 'p-1' });
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      page.remove();
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

    it('entry on /agents with a partial set in the store ignores it and loads its own complete set; home then sends nothing', async () => {
      const fake = newFake(30, 9);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      // A subset of the agents plus the scope capabilities, already in the
      // store when the page connects.
      stateManager.setScope({ type: 'dashboard' });
      stateManager.seedAgents(fake.agents.slice(0, 5));
      stateManager.seedScopeCapabilities('agent', SCOPE_CAPS);
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
