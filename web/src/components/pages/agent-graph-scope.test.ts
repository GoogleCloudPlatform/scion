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
 * The standalone agent graph's load path, scope and live membership:
 * - a held complete set renders with no request, for any project filter;
 * - an unscoped graph drains every agent in the compact view;
 * - a scoped graph sends one fit probe, then drains only its project when
 *   the probe is not complete;
 * - picker changes abort the in-flight load, never repeat the probe, and
 *   reuse a capped unscoped set for "all projects";
 * - capped and failed drains show the incomplete banner, a failure keeps
 *   the previous complete graph, and a resync or a late first connect shows
 *   the may-be-stale banner whose Refresh drains again;
 * - the live scope is the dashboard scope throughout.
 * Request counts follow the interaction-cost table for the standalone graph.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Agent } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import { AgentDrainRunner } from '../../client/agent-drain.js';
import {
  FakeEventSource,
  fakeFetch,
  holdable,
  isGlobalAgentsList,
  jsonResponse,
  makeAgent,
  type Fake,
} from './__fixtures__/global-agents-endpoint.js';

type TestEl = HTMLElement & { updateComplete: Promise<boolean> };

interface GraphInternals {
  agents: Agent[];
  visibleAgents: Agent[];
  loading: boolean;
  reloading: boolean;
  error: string | null;
  stale: boolean;
  focusId: string;
  orientation: string;
  drainRunner: AgentDrainRunner;
  setProjectFilter(value: string): void;
}

function g(el: TestEl): GraphInternals {
  return el as unknown as GraphInternals;
}

const PROBE = '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&view=compact';
const ALL_PAGE = (cursor?: number): string =>
  `/api/v1/agents?limit=500&view=compact${cursor ? `&cursor=${cursor}` : ''}`;
const PROJECT_PAGE = (id: string, cursor?: number): string =>
  `/api/v1/agents?projectId=${id}&limit=500&view=compact${cursor ? `&cursor=${cursor}` : ''}`;

/** An EventSource that never opens: the live connection never comes up. */
class SilentEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = SilentEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = SilentEventSource.CLOSED;
  }
}

function newFake(count: number): Fake {
  return {
    agents: Array.from({ length: count }, (_, i) => makeAgent(i)),
    requests: [],
    otherRequests: [],
    projects: [{ id: 'p-1', name: 'P1' }],
  };
}

function fetchState(): { calls: number; pending: number } {
  const mock = vi.mocked(globalThis.fetch);
  if (!vi.isMockFunction(mock)) return { calls: 0, pending: 0 };
  const settled = mock.mock.settledResults.filter((r) => r.type !== 'incomplete').length;
  return { calls: mock.mock.calls.length, pending: mock.mock.calls.length - settled };
}

/** Waits until the page is idle: no load in flight and every request answered. */
async function settle(el: TestEl): Promise<void> {
  const idle = (): void => {
    const i = el as unknown as {
      loading?: boolean;
      reloading?: boolean;
      countsLoading?: boolean;
      agentsLoading?: boolean;
    };
    expect(i.loading ?? false).toBe(false);
    expect(i.reloading ?? false).toBe(false);
    expect(i.countsLoading ?? false).toBe(false);
    expect(i.agentsLoading ?? false).toBe(false);
    expect(fetchState().pending).toBe(0);
  };
  for (;;) {
    await vi.waitFor(idle, { timeout: 15_000 });
    const before = fetchState().calls;
    await new Promise((resolve) => setTimeout(resolve, 0));
    (stateManager as unknown as { flush(): void }).flush();
    await el.updateComplete;
    if (fetchState().calls === before) {
      idle();
      return;
    }
  }
}

const USER = { id: 'u', email: 'u@example.com', name: 'U', role: 'admin' };

/** Mounts the graph at `/agents/graph<search>` without waiting for its load. */
async function mountUnsettled(
  search = '',
  runner: ConstructorParameters<typeof AgentDrainRunner>[0] = {}
): Promise<TestEl> {
  window.history.replaceState({}, '', `/agents/graph${search}`);
  const el = document.createElement('scion-page-agent-graph') as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: '/agents/graph',
    title: 'Agent graph',
    user: USER,
  };
  g(el).drainRunner = new AgentDrainRunner({ retryDelayMs: 0, ...runner });
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function mountGraph(
  search = '',
  runner: ConstructorParameters<typeof AgentDrainRunner>[0] = {}
): Promise<TestEl> {
  const el = await mountUnsettled(search, runner);
  await settle(el);
  return el;
}

async function mountPage(tag: 'scion-page-home' | 'scion-page-agents'): Promise<TestEl> {
  window.history.replaceState({}, '', tag === 'scion-page-home' ? '/' : '/agents');
  const el = document.createElement(tag) as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: tag === 'scion-page-home' ? '/' : '/agents',
    title: 'Page',
    user: USER,
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await settle(el);
  return el;
}

/** Changes the project filter and waits for the page to settle. */
async function pick(el: TestEl, project: string): Promise<void> {
  g(el).setProjectFilter(project);
  await el.updateComplete;
  await settle(el);
}

function liveUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
  (stateManager as unknown as { flush(): void }).flush();
}

function reconnect(): void {
  stateManager.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
  stateManager.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
}

function ids(agents: Agent[]): string[] {
  return agents.map((a) => a.id).sort();
}

function projectIds(fake: Fake, projectId: string): string[] {
  return ids(fake.agents.filter((a) => a.projectId === projectId));
}

function banner(el: TestEl, kind: 'incomplete' | 'stale'): HTMLElement | null {
  return el.shadowRoot?.querySelector(`.graph-${kind}`) ?? null;
}

function bannerText(el: TestEl, kind: 'incomplete' | 'stale'): string {
  return banner(el, kind)?.querySelector('span')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

async function clickBanner(el: TestEl, kind: 'incomplete' | 'stale'): Promise<void> {
  (banner(el, kind)?.querySelector('sl-button') as HTMLElement).click();
  await el.updateComplete;
  await settle(el);
}

function treeNode(el: TestEl, id: string): HTMLElement | null {
  const tree = el.shadowRoot?.querySelector('scion-agent-tree-view');
  return tree?.shadowRoot?.querySelector(`.node[data-agent-id="${id}"]`) ?? null;
}

/** Puts the store in the dashboard scope holding `agents`, optionally marked complete. */
function holdInState(agents: Agent[], complete: boolean): void {
  stateManager.setScope({ type: 'dashboard' });
  stateManager.seedAgents(agents);
  if (complete) stateManager.markAgentSetComplete('compact');
}

/** Requests since `from`, each of which must ask for the compact view. */
function graphRequests(fake: Fake, from = 0): string[] {
  const sent = fake.requests.slice(from);
  for (const url of sent) expect(new URL(url, 'http://x').searchParams.get('view')).toBe('compact');
  return sent;
}

let setScopeSpy: ReturnType<typeof vi.spyOn>;

describe('/agents/graph scope and loading', { timeout: 60_000 }, () => {
  beforeAll(async () => {
    await import('./agent-graph.js');
    await import('./home.js');
    await import('./agents.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // A real scope change clears the store and its completeness flag.
    stateManager.setScope({ type: 'brokers-list' });
    resetHubProjectCapabilitiesCache();
    localStorage.clear();
    setScopeSpy = vi.spyOn(stateManager, 'setScope');
  });

  afterEach(() => {
    // The page never leaves the dashboard scope (a project scope would
    // clear the complete set later picker changes and pages reuse).
    for (const [scope] of setScopeSpy.mock.calls) {
      expect((scope as { type: string }).type).not.toBe('project');
    }
    setScopeSpy.mockRestore();
    document.body
      .querySelectorAll('scion-page-agent-graph, scion-page-home, scion-page-agents')
      .forEach((n) => n.remove());
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
    window.history.replaceState({}, '', '/');
  });

  describe('load path', () => {
    it('a held complete set renders with no request, and picker changes issue none', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      const el = await mountGraph('?project=p-1');
      expect(fake.requests).toEqual([]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      for (const p of ['', 'p-1', 'p-2']) await pick(el, p);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-2'));
      expect(fake.requests).toEqual([]);
    });

    it('the page only ever sets the dashboard scope, through loads and picker changes', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      setScopeSpy.mockClear();
      const el = await mountGraph('?project=p-1');
      for (const p of ['p-2', '', 'p-3']) await pick(el, p);
      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(setScopeSpy.mock.calls.length).toBeGreaterThan(0);
      for (const [scope] of setScopeSpy.mock.calls) expect(scope).toEqual({ type: 'dashboard' });
    });

    it('an unscoped graph with an empty store drains in one compact request and marks the set compact', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      expect(ids(g(el).visibleAgents)).toEqual(ids(fake.agents));
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
    });

    it('a scoped graph at 25 sends one complete probe, sets the flag, and picker changes issue none', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      expect(stateManager.getAgents()).toHaveLength(25);
      for (const p of ['', 'p-1', 'p-3']) await pick(el, p);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-3'));
      expect(fake.requests).toHaveLength(1);
    });

    it('a scoped graph at 1,200 probes, then drains only its project; X to all drains once and all to X issues none', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);

      // A live create in another project updates the store but not the graph.
      // The server holds both live creates from now on.
      const yNew = makeAgent(9001, { id: 'y-new', projectId: 'p-2' });
      const xNew = makeAgent(9002, { id: 'x-new', projectId: 'p-1' });
      fake.agents.unshift(xNew, yNew);
      liveUpdate('agent.y-new.created', yNew);
      await el.updateComplete;
      expect(stateManager.getAgent('y-new')).toBeDefined();
      expect(g(el).agents.some((a) => a.id === 'y-new')).toBe(false);
      // One in the scoped project joins it.
      liveUpdate('agent.x-new.created', xNew);
      await el.updateComplete;
      expect(g(el).visibleAgents.some((a) => a.id === 'x-new')).toBe(true);

      const before = fake.requests.length;
      await pick(el, '');
      expect(graphRequests(fake, before)).toEqual([ALL_PAGE(), ALL_PAGE(500), ALL_PAGE(1000)]);
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      await pick(el, 'p-1');
      expect(fake.requests).toHaveLength(before + 3);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(projectIds(fake, 'p-1')).toContain('x-new');
    });

    it('a probe without complete: true (an older server answering a legacy page) is not complete', async () => {
      const fake = newFake(25);
      const inner = fakeFetch(fake);
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : '';
          if (new URL(raw, 'http://x').searchParams.has('sort')) {
            fake.requests.push(raw);
            // The whole set, but no `complete` field.
            return Promise.resolve(jsonResponse({ agents: fake.agents }));
          }
          return inner(input, init);
        })
      );
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('a non-empty store without the flag is not reused: the graph loads (unscoped and scoped)', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents.slice(0, 3), false);
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      expect(g(el).visibleAgents).toHaveLength(25);
      el.remove();

      stateManager.setScope({ type: 'brokers-list' });
      holdInState(fake.agents.slice(0, 3), false);
      fake.requests.length = 0;
      const scoped = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(scoped).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('a live delete after the load removes the agent from the graph', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const victim = fake.agents[4].id;
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      await el.updateComplete;
      expect(g(el).agents.some((a) => a.id === victim)).toBe(false);
      expect(g(el).agents).toHaveLength(24);
    });

    it('a live status change updates the member object', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const id = fake.agents[2].id;
      liveUpdate(`agent.${id}.status`, { agentId: id, phase: 'stopped' });
      await el.updateComplete;
      expect(g(el).agents.find((a) => a.id === id)?.phase).toBe('stopped');
    });
  });

  describe('drain', () => {
    it('1,201 agents over 3 pages give every node, with lineage across a page boundary', async () => {
      const fake = newFake(1201);
      const parent = fake.agents[10];
      fake.agents[1100] = { ...fake.agents[1100], ancestry: ['user-1', parent.id] } as Agent;
      parent.ancestry = ['user-1'];
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([ALL_PAGE(), ALL_PAGE(500), ALL_PAGE(1000)]);
      expect(g(el).visibleAgents).toHaveLength(1201);
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      expect(banner(el, 'incomplete')).toBeNull();
      await vi.waitFor(() => expect(treeNode(el, fake.agents[1100].id)).not.toBeNull());
      expect(treeNode(el, fake.agents[1100].id)?.querySelector('.ancestor-missing')).toBeNull();
    });

    it('2,001 agents stop after 4 requests with the capped banner, and missing ancestors are marked; Retry drains again', async () => {
      const fake = newFake(2001);
      fake.agents[5] = { ...fake.agents[5], ancestry: ['user-1', fake.agents[2000].id] } as Agent;
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([
        ALL_PAGE(),
        ALL_PAGE(500),
        ALL_PAGE(1000),
        ALL_PAGE(1500),
      ]);
      expect(g(el).visibleAgents).toHaveLength(2000);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      expect(bannerText(el, 'incomplete')).toBe(
        'Graph incomplete: 2,000 loaded (newest 2,000 checked), more exist · narrow with a project filter'
      );
      await vi.waitFor(() => expect(treeNode(el, fake.agents[5].id)).not.toBeNull());
      const marker = treeNode(el, fake.agents[5].id)?.querySelector('.ancestor-missing');
      expect(marker?.getAttribute('aria-label')).toBe('Ancestor not loaded');

      await clickBanner(el, 'incomplete');
      expect(fake.requests).toHaveLength(8);
    });

    it('a page-2 failure shows the incomplete banner with what loaded', async () => {
      const fake = newFake(1201);
      const inner = fakeFetch(fake);
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : '';
          if (new URL(raw, 'http://x').searchParams.get('cursor') === '500') {
            fake.requests.push(raw);
            return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
          }
          return inner(input, init);
        })
      );
      const el = await mountGraph();
      expect(g(el).visibleAgents).toHaveLength(500);
      expect(bannerText(el, 'incomplete')).toBe('Incomplete: loaded 500');
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
    });

    it('a failed re-drain keeps the previous complete graph, with the banner', async () => {
      const fake = newFake(1201);
      const inner = fakeFetch(fake);
      let failPage2 = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : '';
          if (failPage2 && new URL(raw, 'http://x').searchParams.get('cursor') === '500') {
            fake.requests.push(raw);
            return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
          }
          return inner(input, init);
        })
      );
      const el = await mountGraph();
      expect(g(el).visibleAgents).toHaveLength(1201);
      failPage2 = true;
      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(g(el).visibleAgents).toHaveLength(1201);
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 500 · showing the previous graph'
      );
    });

    it('a first-page failure with nothing loaded shows the error; Retry loads', async () => {
      const fake = newFake(25);
      fake.failAll = true;
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(g(el).error).toBe('HTTP 500');
      fake.failAll = false;
      const retry = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
        b.textContent?.includes('Retry')
      ) as HTMLElement;
      retry.click();
      await settle(el);
      expect(g(el).error).toBeNull();
      expect(g(el).visibleAgents).toHaveLength(25);
    });

    it('a create and a delete during an unscoped drain are kept', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.get('cursor') === '500');
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const victim = fake.agents[3].id;
      liveUpdate('agent.n-new.created', makeAgent(9003, { id: 'n-new', projectId: 'p-5' }));
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      h.release();
      await settle(el);
      const shown = ids(g(el).visibleAgents);
      expect(shown).toContain('n-new');
      expect(shown).not.toContain(victim);
      expect(shown).toHaveLength(1200);
      expect(stateManager.getAgent(victim)).toBeUndefined();
    });

    it('a create during a project drain joins only when it belongs to the project', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      liveUpdate('agent.in-x.created', makeAgent(9004, { id: 'in-x', projectId: 'p-1' }));
      liveUpdate('agent.in-y.created', makeAgent(9005, { id: 'in-y', projectId: 'p-2' }));
      h.release();
      await settle(el);
      expect(g(el).agents.some((a) => a.id === 'in-x')).toBe(true);
      expect(g(el).agents.some((a) => a.id === 'in-y')).toBe(false);
      expect(stateManager.getAgent('in-y')).toBeDefined();
    });

    it('a picker change aborts the in-flight drain and keeps focus and orientation', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.get('cursor') === '500');
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?focus=g-00003&dir=horizontal');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      g(el).setProjectFilter('p-1');
      expect(h.sent[0].signal?.aborted).toBe(true);
      await settle(el);
      // The aborted page never reached the server; the probe and the
      // project drain follow.
      expect(graphRequests(fake)).toEqual([ALL_PAGE(), PROBE, PROJECT_PAGE('p-1')]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(g(el).focusId).toBe('g-00003');
      expect(g(el).orientation).toBe('horizontal');
      expect(
        el.shadowRoot?.querySelector('scion-agent-tree-view')?.getAttribute('orientation')
      ).toBe('horizontal');
    });
  });

  describe('picker', () => {
    it('a capped unscoped drain is reused for all projects; a project choice drains that project', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(fake.requests).toHaveLength(4);
      await pick(el, 'p-1');
      expect(graphRequests(fake, 4)).toEqual([PROJECT_PAGE('p-1')]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(banner(el, 'incomplete')).toBeNull();
      await pick(el, '');
      expect(fake.requests).toHaveLength(5);
      expect(g(el).visibleAgents).toHaveLength(2000);
      expect(bannerText(el, 'incomplete')).toContain('narrow with a project filter');
    });

    it('a picker-change drain starts with no connection wait and shows no stale banner', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      // A connection wait would last a minute.
      const el = await mountGraph('?project=p-1', { connectTimeoutMs: 60_000 });
      const before = fake.requests.length;
      g(el).setProjectFilter('p-2');
      await vi.waitFor(() => expect(fake.requests.length).toBe(before + 1), { timeout: 1000 });
      expect(fake.requests[before]).toBe(PROJECT_PAGE('p-2'));
      await settle(el);
      expect(g(el).stale).toBe(false);
      expect(banner(el, 'stale')).toBeNull();
    });
  });

  describe('stale banner', () => {
    it('on the held path a resync shows the banner with no request; Refresh drains again', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      const el = await mountGraph('?project=p-1');
      await new Promise((resolve) => setTimeout(resolve, 0));
      reconnect();
      await el.updateComplete;
      expect(bannerText(el, 'stale')).toBe('Graph may be stale');
      expect(fake.requests).toEqual([]);
      await clickBanner(el, 'stale');
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('after a drain a resync shows the banner with no request; Refresh drains the project again', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(fake.requests).toHaveLength(2);
      reconnect();
      await el.updateComplete;
      expect(banner(el, 'stale')).not.toBeNull();
      expect(fake.requests).toHaveLength(2);
      await clickBanner(el, 'stale');
      expect(graphRequests(fake, 2)).toEqual([PROJECT_PAGE('p-1')]);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('a drain whose live connection comes up late shows the banner', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      stateManager.setScope({ type: 'agent-detail', agentId: 'x' });
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('', { connectTimeoutMs: 5 });
      expect(fake.requests).toEqual([ALL_PAGE()]);
      expect(banner(el, 'stale')).not.toBeNull();
    });

    it('on the held path a late first connect shows the banner with no request', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled();
      expect(banner(el, 'stale')).toBeNull();
      vi.advanceTimersByTime(3000);
      await el.updateComplete;
      expect(banner(el, 'stale')).not.toBeNull();
      expect(fake.requests).toEqual([]);
    });
  });

  describe('state consumers after the graph', () => {
    it('a scoped graph at 25 then home issues no agents request', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph('?project=p-1')).remove();
      const before = fake.requests.length;
      await mountPage('scion-page-home');
      expect(fake.requests.length - before).toBe(0);
    });

    it('a scoped graph at 1,200 then home issues one home load', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph('?project=p-1')).remove();
      const before = fake.requests.length;
      await mountPage('scion-page-home');
      expect(fake.requests.slice(before)).toEqual([
        '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1',
      ]);
      expect(fake.otherRequests).toEqual(['/api/v1/projects', '/api/v1/admin/invites/stats']);
    });

    it('a graph-drained (compact) flag satisfies home but not the agents page reuse', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph()).remove();
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      let before = fake.requests.length;
      (await mountPage('scion-page-home')).remove();
      expect(fake.requests.length - before).toBe(0);
      localStorage.setItem('scion-view-agents', 'list');
      before = fake.requests.length;
      await mountPage('scion-page-agents');
      expect(fake.requests.length - before).toBe(1);
    });

    it('a 0-row graph then home runs the normal empty-state load', async () => {
      const fake = newFake(0);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph()).remove();
      expect(fake.requests).toEqual([ALL_PAGE()]);
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      await mountPage('scion-page-home');
      expect(fake.requests.slice(1)).toEqual([
        '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1',
      ]);
      expect(fake.otherRequests).toEqual(['/api/v1/projects', '/api/v1/admin/invites/stats']);
    });
  });

  describe('interaction cost (standalone graph rows)', () => {
    const allPages = (n: number): string[] =>
      Array.from({ length: Math.min(Math.ceil(Math.max(n, 1) / 500), 4) }, (_, i) =>
        ALL_PAGE(i * 500 || undefined)
      );

    for (const n of [25, 500, 1200]) {
      it(`graph entry with the flag held, any project, issues no request (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        (await mountGraph()).remove();
        expect(stateManager.isAgentSetComplete('compact')).toBe(true);
        const before = fake.requests.length;
        for (const search of ['', '?project=p-1', '?project=p-2']) {
          (await mountGraph(search)).remove();
        }
        expect(fake.requests.length).toBe(before);
      });
    }

    for (const n of [25, 500, 1200, 2001]) {
      it(`unscoped graph entry, empty store (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        await mountGraph();
        expect(graphRequests(fake)).toEqual(allPages(n));
      });

      it(`scoped graph entry, empty store (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        await mountGraph('?project=p-1');
        expect(graphRequests(fake)).toEqual(n <= 500 ? [PROBE] : [PROBE, PROJECT_PAGE('p-1')]);
      });

      it(`graph entry with a non-empty store and no flag loads as from empty (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        holdInState(fake.agents.slice(0, 5), false);
        (await mountGraph()).remove();
        expect(graphRequests(fake)).toEqual(allPages(n));
        stateManager.setScope({ type: 'brokers-list' });
        holdInState(fake.agents.slice(0, 5), false);
        fake.requests.length = 0;
        await mountGraph('?project=p-1');
        expect(graphRequests(fake)).toEqual(n <= 500 ? [PROBE] : [PROBE, PROJECT_PAGE('p-1')]);
      });

      it(`a resync during or after a graph drain issues no request (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        const el = await mountGraph('?project=p-1');
        const before = fake.requests.length;
        reconnect();
        await el.updateComplete;
        await settle(el);
        expect(fake.requests.length).toBe(before);
        expect(banner(el, 'stale')).not.toBeNull();
      });
    }

    for (const n of [25, 500]) {
      it(`picker changes after a complete first load issue no request (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        const el = await mountGraph('?project=p-1');
        expect(fake.requests).toEqual([PROBE]);
        for (const p of ['', 'p-1', 'p-2', '']) await pick(el, p);
        expect(fake.requests).toHaveLength(1);
      });
    }

    it('picker changes at 1,200: X to Y drains Y, Y to all drains once, then none', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      await pick(el, 'p-2');
      expect(graphRequests(fake, 2)).toEqual([PROJECT_PAGE('p-2')]);
      await pick(el, '');
      expect(graphRequests(fake, 3)).toEqual(allPages(1200));
      for (const p of ['p-1', 'p-2', '']) await pick(el, p);
      expect(fake.requests).toHaveLength(6);
    });

    it('picker changes above 2,000: to all drains once (capped), a project drains it, back to all issues none', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(fake.requests).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      await pick(el, '');
      expect(graphRequests(fake, 2)).toEqual(allPages(2001));
      await pick(el, 'p-1');
      expect(graphRequests(fake, 6)).toEqual([PROJECT_PAGE('p-1')]);
      await pick(el, '');
      expect(fake.requests).toHaveLength(7);
    });
  });
});
