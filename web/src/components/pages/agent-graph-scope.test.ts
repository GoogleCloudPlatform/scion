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
 * - the live scope is the dashboard scope throughout;
 * - every interaction issues an exact, counted set of requests, and none
 *   besides the agents list.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Agent, DeletionInfo } from '../../shared/types.js';
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
  SCOPE_CAPS,
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
  cappedAll: { members: Map<string, Agent>; loaded: number; stale: boolean } | null;
  memberScope: string | null;
  probed: boolean;
  knownLarge: boolean;
  setProjectFilter(value: string): void;
  onReload(): void;
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

/** A fake whose rows carry `deletion: null`, as the hub's compact rows do when no delete is open. */
function newFake(count: number): Fake {
  return {
    agents: Array.from({ length: count }, (_, i) => makeAgent(i, { deletion: null })),
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

/** No request went anywhere but the agents list (no projects, stats or other fetch). */
function expectOnlyAgentRequests(fake: Fake): void {
  expect(vi.mocked(globalThis.fetch).mock.calls.length).toBe(fake.requests.length);
  expect(fake.otherRequests).toEqual([]);
}

function rawUrl(input: string | URL | Request): string {
  return typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
}

/** `fakeFetch`, except agents requests matching `fails` (decided per request) answer a 500. */
function failingFetch(fake: Fake, fails: (u: URL) => boolean) {
  const inner = fakeFetch(fake);
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const raw = rawUrl(input);
    const u = new URL(raw, 'http://x');
    if (u.pathname === '/api/v1/agents' && fails(u)) {
      fake.requests.push(raw);
      return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
    }
    return inner(input, init);
  };
}

/** `fakeFetch`, except the fit probe answers the whole set with no `complete` field (an older server). */
function legacyProbeFetch(fake: Fake) {
  const inner = fakeFetch(fake);
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const raw = rawUrl(input);
    if (new URL(raw, 'http://x').searchParams.has('sort')) {
      fake.requests.push(raw);
      return Promise.resolve(jsonResponse({ agents: fake.agents }));
    }
    return inner(input, init);
  };
}

const CAPPED_ALL =
  'Graph incomplete: 2,000 loaded (newest 2,000 checked), more exist · narrow with a project filter';
const STALE = 'Graph may be stale';

function treeView(el: TestEl): (HTMLElement & { markMissingAncestors: boolean }) | null {
  return el.shadowRoot?.querySelector('scion-agent-tree-view') ?? null;
}

/** Listeners of `name` on the store added minus removed, since the spies started. */
function listenerBalance(
  add: ReturnType<typeof vi.spyOn>,
  remove: ReturnType<typeof vi.spyOn>,
  name: string
): number {
  const count = (spy: ReturnType<typeof vi.spyOn>): number =>
    spy.mock.calls.filter(([type]) => type === name).length;
  return count(add) - count(remove);
}

/**
 * Gives each `stateManager.sseConnected` call its own promise, resolved by
 * the test, so one attachment's connect wait can end without another's.
 */
function deferredConnects(): Array<{ promise: Promise<void>; resolve: () => void }> {
  const waits: Array<{ promise: Promise<void>; resolve: () => void }> = [];
  vi.spyOn(stateManager, 'sseConnected').mockImplementation(() => {
    let resolve!: () => void;
    const promise = new Promise<void>((r) => {
      resolve = r;
    });
    waits.push({ promise, resolve });
    return promise;
  });
  return waits;
}

/** The agents page's own first load in the list view. */
const AGENTS_PAGE_LOAD = '/api/v1/agents?sort=updated&dir=desc&limit=25&fit=500&stats=1';

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
      vi.stubGlobal('fetch', vi.fn(legacyProbeFetch(fake)));
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

    it('an agent deleted and then created again live stays out, as in a drain', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const victim = fake.agents[4];
      liveUpdate(`agent.${victim.id}.deleted`, { agentId: victim.id });
      liveUpdate(`agent.${victim.id}.created`, { ...victim, phase: 'stopped' });
      await el.updateComplete;
      expect(stateManager.getDeletedAgentIds().has(victim.id)).toBe(true);
      expect(g(el).agents.some((a) => a.id === victim.id)).toBe(false);
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
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
      await vi.waitFor(() => expect(treeNode(el, fake.agents[5].id)).not.toBeNull());
      const marker = treeNode(el, fake.agents[5].id)?.querySelector('.ancestor-missing');
      expect(marker?.getAttribute('aria-label')).toBe('Ancestor not loaded');

      await clickBanner(el, 'incomplete');
      expect(graphRequests(fake, 4)).toEqual([
        ALL_PAGE(),
        ALL_PAGE(500),
        ALL_PAGE(1000),
        ALL_PAGE(1500),
      ]);
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
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

    it('a failed re-drain keeps the previous complete graph, with a banner counting the rows shown', async () => {
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
        'Incomplete: loaded 1,201 · showing the previous graph'
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

    it('a create during a project drain joins only when it belongs to the project; a delete leaves', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const victim = projectIds(fake, 'p-1')[2];
      liveUpdate('agent.in-x.created', makeAgent(9004, { id: 'in-x', projectId: 'p-1' }));
      liveUpdate('agent.in-y.created', makeAgent(9005, { id: 'in-y', projectId: 'p-2' }));
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      h.release();
      await settle(el);
      expect(g(el).agents.some((a) => a.id === 'in-x')).toBe(true);
      expect(g(el).agents.some((a) => a.id === 'in-y')).toBe(false);
      expect(stateManager.getAgent('in-y')).toBeDefined();
      expect(g(el).agents.some((a) => a.id === victim)).toBe(false);
      expect(stateManager.getAgent(victim)).toBeUndefined();
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
    it('a project drained after a capped unscoped set still offers every known project', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const all = (g(el).projects as Array<{ id: string }>).map((p) => p.id);
      expect(all).toHaveLength(7);
      const optionValues = () =>
        [...(el.shadowRoot?.querySelectorAll('sl-select sl-option') ?? [])].map((o) =>
          o.getAttribute('value')
        );
      expect(optionValues()).toEqual(all);
      await pick(el, 'p-1');
      expect(g(el).memberScope).toBe('p-1');
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect((g(el).projects as Array<{ id: string }>).map((p) => p.id)).toEqual(all);
      expect(optionValues()).toEqual(all);
    });

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
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
    });

    it('a picker-change drain starts with no connection wait and shows no stale banner', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      // A connection wait would last a minute.
      const el = await mountGraph('?project=p-1', { connectTimeoutMs: 60_000 });
      const before = fake.requests.length;
      g(el).setProjectFilter('p-2');
      await vi.waitFor(() => expect(fake.requests.length).toBe(before + 1), { timeout: 5000 });
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
      expect(bannerText(el, 'stale')).toBe(STALE);
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
      expect(bannerText(el, 'stale')).toBe(STALE);
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
      expect(bannerText(el, 'stale')).toBe(STALE);
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
      expect(bannerText(el, 'stale')).toBe(STALE);
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

    it('a graph-drained (compact) flag satisfies home; the agents page, with its capabilities held, still loads', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph()).remove();
      expect(stateManager.isAgentSetComplete('compact')).toBe(true);
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      let before = fake.requests.length;
      (await mountPage('scion-page-home')).remove();
      expect(fake.requests.length - before).toBe(0);
      // Only the flag's view can decide the agents page's reuse now.
      stateManager.seedScopeCapabilities('agent', SCOPE_CAPS);
      localStorage.setItem('scion-view-agents', 'list');
      before = fake.requests.length;
      await mountPage('scion-page-agents');
      expect(fake.requests.slice(before)).toEqual([AGENTS_PAGE_LOAD]);
    });

    it('a full flag with the capabilities held lets the agents page reuse the set with no request', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      (await mountGraph()).remove();
      stateManager.markAgentSetComplete('full');
      stateManager.seedScopeCapabilities('agent', SCOPE_CAPS);
      localStorage.setItem('scion-view-agents', 'list');
      const before = fake.requests.length;
      const el = await mountPage('scion-page-agents');
      expect(fake.requests.length - before).toBe(0);
      expect((el as unknown as { agents: Agent[] }).agents).toHaveLength(25);
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

  describe('request counts', () => {
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
        expectOnlyAgentRequests(fake);
      });
    }

    for (const n of [25, 500, 1200, 2001]) {
      it(`unscoped graph entry, empty store (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        await mountGraph();
        expect(graphRequests(fake)).toEqual(allPages(n));
        expectOnlyAgentRequests(fake);
      });

      it(`scoped graph entry, empty store (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        await mountGraph('?project=p-1');
        expect(graphRequests(fake)).toEqual(n <= 500 ? [PROBE] : [PROBE, PROJECT_PAGE('p-1')]);
        expectOnlyAgentRequests(fake);
      });

      it(`graph entry with a non-empty store and no flag loads as from empty (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        holdInState(fake.agents.slice(0, 5), false);
        (await mountGraph()).remove();
        expect(graphRequests(fake)).toEqual(allPages(n));
        expectOnlyAgentRequests(fake);
        stateManager.setScope({ type: 'brokers-list' });
        holdInState(fake.agents.slice(0, 5), false);
        fake.requests.length = 0;
        vi.mocked(globalThis.fetch).mockClear();
        await mountGraph('?project=p-1');
        expect(graphRequests(fake)).toEqual(n <= 500 ? [PROBE] : [PROBE, PROJECT_PAGE('p-1')]);
        expectOnlyAgentRequests(fake);
      });

      it(`a resync after a graph load issues no request (A = ${n})`, async () => {
        const fake = newFake(n);
        vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
        const el = await mountGraph('?project=p-1');
        const before = fake.requests.length;
        reconnect();
        await el.updateComplete;
        await settle(el);
        expect(fake.requests.length).toBe(before);
        expect(bannerText(el, 'stale')).toBe(STALE);
        expectOnlyAgentRequests(fake);
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
        expectOnlyAgentRequests(fake);
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
      expectOnlyAgentRequests(fake);
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
      expectOnlyAgentRequests(fake);
    });
  });

  describe('fit probe', () => {
    it('a probe answered with a 500 falls back to the project drain, with no error', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, (u) => u.searchParams.has('fit'))));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(g(el).error).toBeNull();
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('a probe that fails on the network falls back to the project drain, with no error', async () => {
      const fake = newFake(25);
      const inner = fakeFetch(fake);
      vi.spyOn(console, 'warn').mockImplementation(() => {});
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw = rawUrl(input);
          if (new URL(raw, 'http://x').searchParams.has('fit')) {
            fake.requests.push(raw);
            return Promise.reject(new TypeError('Failed to fetch'));
          }
          return inner(input, init);
        })
      );
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(g(el).error).toBeNull();
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('after a legacy probe answer, a picker change drains the new project with no second probe', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(legacyProbeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      await pick(el, 'p-2');
      await pick(el, 'p-3');
      expect(graphRequests(fake)).toEqual([
        PROBE,
        PROJECT_PAGE('p-1'),
        PROJECT_PAGE('p-2'),
        PROJECT_PAGE('p-3'),
      ]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-3'));
    });

    it('after a probe answered with a 500, a picker change drains the new project with no second probe', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, (u) => u.searchParams.has('fit'))));
      const el = await mountGraph('?project=p-1');
      await pick(el, 'p-2');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1'), PROJECT_PAGE('p-2')]);
    });

    it('a probe answered after the first-connect timeout shows the stale banner', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('fit'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      vi.advanceTimersByTime(3000);
      expect(g(el).stale).toBe(true);
      h.release();
      await vi.waitFor(() => expect(g(el).loading).toBe(false));
      await el.updateComplete;
      expect(fake.requests).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(bannerText(el, 'stale')).toBe(STALE);
    });

    it('a probe sent after a late first connect has come up shows no stale banner', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      stateManager.setScope({ type: 'agent-detail', agentId: 'x' });
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), (u) => !u.searchParams.has('sort'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled('', { connectTimeoutMs: 60_000 });
      vi.advanceTimersByTime(3000);
      expect((el as unknown as { firstConnectLate: boolean }).firstConnectLate).toBe(true);
      // The unscoped drain is still waiting for the connection, so no banner yet.
      expect(g(el).stale).toBe(false);
      vi.useRealTimers();
      stateManager.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      g(el).setProjectFilter('p-1');
      await settle(el);
      // The aborted unscoped drain page never reached the server.
      expect(h.sent.map((r) => r.url)).toEqual([ALL_PAGE()]);
      expect(h.sent[0].signal?.aborted).toBe(true);
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(g(el).stale).toBe(false);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('live changes during the probe are kept, a resync during it shows the stale banner, and its epoch closes', async () => {
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('fit'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const add = vi.spyOn(stateManager, 'addEventListener');
      const remove = vi.spyOn(stateManager, 'removeEventListener');
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      // Wait for the live connection, so the reconnect below is a resync.
      await stateManager.sseConnected(stateManager.scopeGeneration);
      const statusId = fake.agents[0].id;
      const victim = fake.agents[1].id;
      liveUpdate(`agent.${statusId}.status`, { agentId: statusId, phase: 'stopped' });
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      liveUpdate('agent.n-p1.created', makeAgent(9100, { id: 'n-p1', projectId: 'p-1' }));
      reconnect();
      h.release();
      await settle(el);
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(stateManager.getAgent(statusId)?.phase).toBe('stopped');
      expect(g(el).agents.find((a) => a.id === statusId)?.phase).toBe('stopped');
      expect(g(el).agents.some((a) => a.id === victim)).toBe(false);
      expect(stateManager.getAgent(victim)).toBeUndefined();
      expect(g(el).visibleAgents.some((a) => a.id === 'n-p1')).toBe(true);
      expect(bannerText(el, 'stale')).toBe(STALE);
      const openEpochs = (stateManager as unknown as { seedEpochs: Map<unknown, unknown> })
        .seedEpochs.size;
      expect(openEpochs).toBe(0);
      el.remove();
      for (const name of ['agent-created', 'agents-changed', 'agents-resync']) {
        expect(listenerBalance(add, remove, name)).toBe(0);
      }
    });

    it('a probe answered with a 500 still closes its seed epoch', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, (u) => u.searchParams.has('fit'))));
      const add = vi.spyOn(stateManager, 'addEventListener');
      const remove = vi.spyOn(stateManager, 'removeEventListener');
      const el = await mountGraph('?project=p-1');
      expect(
        (stateManager as unknown as { seedEpochs: Map<unknown, unknown> }).seedEpochs.size
      ).toBe(0);
      el.remove();
      for (const name of ['agent-created', 'agents-changed', 'agents-resync']) {
        expect(listenerBalance(add, remove, name)).toBe(0);
      }
    });
  });

  describe('live changes during a drain', () => {
    it('a resync during an unscoped drain shows the stale banner with no extra request; a status during it is kept', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.get('cursor') === '500');
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const id = fake.agents[7].id;
      liveUpdate(`agent.${id}.status`, { agentId: id, phase: 'stopped' });
      reconnect();
      h.release();
      await settle(el);
      expect(graphRequests(fake)).toEqual([ALL_PAGE(), ALL_PAGE(500), ALL_PAGE(1000)]);
      expect(g(el).agents.find((a) => a.id === id)?.phase).toBe('stopped');
      expect(bannerText(el, 'stale')).toBe(STALE);
      expectOnlyAgentRequests(fake);
    });

    it('a resync during a project drain shows the stale banner with no extra request', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      reconnect();
      h.release();
      await settle(el);
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(bannerText(el, 'stale')).toBe(STALE);
    });
  });

  describe('kept capped set', () => {
    it('keeps its stale state: a resync, a project, then all again shows the stale banner with no request', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      reconnect();
      await el.updateComplete;
      expect(bannerText(el, 'stale')).toBe(STALE);
      await pick(el, 'p-1');
      expect(banner(el, 'stale')).toBeNull();
      await pick(el, '');
      expect(graphRequests(fake, 4)).toEqual([PROJECT_PAGE('p-1')]);
      expect(g(el).visibleAgents).toHaveLength(2000);
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
      expect(bannerText(el, 'stale')).toBe(STALE);
    });

    it('a capped drain whose live connection came up late keeps that stale state', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      stateManager.setScope({ type: 'agent-detail', agentId: 'x' });
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('', { connectTimeoutMs: 5 });
      expect(fake.requests).toHaveLength(4);
      expect(g(el).cappedAll?.stale).toBe(true);
      expect(bannerText(el, 'stale')).toBe(STALE);
    });

    it('receives live creates and deletes while a project is shown', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      await pick(el, 'p-1');
      const victim = fake.agents.find((a) => a.projectId === 'p-2')!.id;
      liveUpdate('agent.n-p3.created', makeAgent(9200, { id: 'n-p3', projectId: 'p-3' }));
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      await el.updateComplete;
      await pick(el, '');
      expect(fake.requests).toHaveLength(5);
      const shown = ids(g(el).visibleAgents);
      expect(shown).toContain('n-p3');
      expect(shown).not.toContain(victim);
      expect(shown).toHaveLength(2000);
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
    });

    it('an agent beyond the cap that changes live never joins it, even once loaded by a project drain', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const beyond = fake.agents[2000];
      expect(g(el).agents.some((a) => a.id === beyond.id)).toBe(false);
      await pick(el, beyond.projectId!);
      expect(g(el).agents.some((a) => a.id === beyond.id)).toBe(true);
      await pick(el, '');
      liveUpdate(`agent.${beyond.id}.status`, { agentId: beyond.id, phase: 'stopped' });
      await el.updateComplete;
      expect(stateManager.getAgent(beyond.id)?.phase).toBe('stopped');
      expect(g(el).agents.some((a) => a.id === beyond.id)).toBe(false);
      expect(g(el).agents).toHaveLength(2000);
    });

    it('picking all while a project drain is in flight aborts it and reuses the capped set', async () => {
      const fake = newFake(2001);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mountGraph();
      h.hold(1);
      g(el).setProjectFilter('p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      g(el).setProjectFilter('');
      expect(h.sent[0].signal?.aborted).toBe(true);
      await settle(el);
      expect(fake.requests).toHaveLength(4);
      expect(h.sent).toHaveLength(1);
      expect(g(el).visibleAgents).toHaveLength(2000);
      expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
    });

    for (const [where, fails, retried] of [
      [
        'the first page',
        (u: URL) => !u.searchParams.has('cursor'),
        [ALL_PAGE(), ALL_PAGE(), ALL_PAGE()],
      ],
      [
        'page 3',
        (u: URL) => u.searchParams.get('cursor') === '1000',
        [ALL_PAGE(), ALL_PAGE(500), ALL_PAGE(1000), ALL_PAGE(1000), ALL_PAGE(1000)],
      ],
    ] as const) {
      it(`a Retry failing on ${where} keeps the same capped set and its banner`, async () => {
        const fake = newFake(2001);
        let fail = false;
        vi.stubGlobal('fetch', vi.fn(failingFetch(fake, (u) => fail && fails(u))));
        const el = await mountGraph();
        const shown = ids(g(el).visibleAgents);
        fail = true;
        await clickBanner(el, 'incomplete');
        fail = false;
        expect(graphRequests(fake, 4)).toEqual(retried);
        expect(g(el).error).toBeNull();
        expect(ids(g(el).visibleAgents)).toEqual(shown);
        expect(ids([...g(el).cappedAll!.members.values()])).toEqual(shown);
        expect(bannerText(el, 'incomplete')).toBe(`${CAPPED_ALL} · showing the previous graph`);
        expect(treeView(el)?.markMissingAncestors).toBe(true);
        // Back to all through a project: the same set again.
        await pick(el, 'p-1');
        await pick(el, '');
        expect(ids(g(el).visibleAgents)).toEqual(shown);
        expect(bannerText(el, 'incomplete')).toBe(CAPPED_ALL);
      });
    }

    it('a capped project drain shows the capped banner without the project-filter hint', async () => {
      const fake = newFake(2001);
      for (const a of fake.agents) a.projectId = 'p-1';
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([
        PROBE,
        PROJECT_PAGE('p-1'),
        PROJECT_PAGE('p-1', 500),
        PROJECT_PAGE('p-1', 1000),
        PROJECT_PAGE('p-1', 1500),
      ]);
      expect(bannerText(el, 'incomplete')).toBe(
        'Graph incomplete: 2,000 loaded (newest 2,000 checked), more exist'
      );
    });

    it('the capped banner counts the rows loaded when fewer than those checked are readable', async () => {
      const fake = newFake(2001);
      const inner = fakeFetch(fake);
      // The server drops every agent whose number is a multiple of 3 (not readable).
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
          const resp = await inner(input, init);
          if (new URL(rawUrl(input), 'http://x').pathname !== '/api/v1/agents') return resp;
          const body = (await resp.json()) as { agents: Agent[] };
          body.agents = body.agents.filter((a) => Number(a.id.slice(2)) % 3 !== 0);
          return jsonResponse(body);
        })
      );
      const el = await mountGraph();
      expect(fake.requests).toHaveLength(4);
      expect(g(el).visibleAgents).toHaveLength(1333);
      expect(bannerText(el, 'incomplete')).toBe(
        'Graph incomplete: 1,333 loaded (newest 2,000 checked), more exist · narrow with a project filter'
      );
    });
  });

  describe('failed re-drains', () => {
    it('a partial graph whose Retry fails again keeps its own banner and rows', async () => {
      const fake = newFake(1201);
      let failing = '500';
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('cursor') === failing))
      );
      const el = await mountGraph();
      expect(bannerText(el, 'incomplete')).toBe('Incomplete: loaded 500');
      const shown = ids(g(el).visibleAgents);
      const before = fake.requests.length;
      // This time page 2 answers and page 3 fails: more rows arrive, but the
      // drain still failed.
      failing = '1000';
      await clickBanner(el, 'incomplete');
      expect(graphRequests(fake, before)).toEqual([
        ALL_PAGE(),
        ALL_PAGE(500),
        ALL_PAGE(1000),
        ALL_PAGE(1000),
        ALL_PAGE(1000),
      ]);
      expect(ids(g(el).visibleAgents)).toEqual(shown);
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 500 · showing the previous graph'
      );
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
    });

    it('a complete graph whose Refresh fails on the first page keeps it, never saying loaded 0', async () => {
      const fake = newFake(1201);
      let fail = false;
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, () => fail)));
      const el = await mountGraph();
      reconnect();
      await el.updateComplete;
      fail = true;
      await clickBanner(el, 'stale');
      expect(g(el).error).toBeNull();
      expect(g(el).visibleAgents).toHaveLength(1201);
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 1,201 · showing the previous graph'
      );
    });

    it('a picker change after a failed Refresh of a held set clears the kept-previous banner', async () => {
      const fake = newFake(25);
      let fail = false;
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, () => fail)));
      holdInState(fake.agents, true);
      const el = await mountGraph();
      await new Promise((resolve) => setTimeout(resolve, 0));
      reconnect();
      await el.updateComplete;
      fail = true;
      await clickBanner(el, 'stale');
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 25 · showing the previous graph'
      );
      const before = fake.requests.length;
      await pick(el, 'p-1');
      expect(fake.requests).toHaveLength(before);
      expect(banner(el, 'incomplete')).toBeNull();
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('after an error view, a failed first page for the earlier project shows the error, not a previous graph', async () => {
      const fake = newFake(1200);
      const failing = new Set(['p-2']);
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => failing.has(u.searchParams.get('projectId') ?? '')))
      );
      const el = await mountGraph('?project=p-1');
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      await pick(el, 'p-2');
      expect(g(el).error).toBe('HTTP 500');
      expect(g(el).agents).toEqual([]);
      expect(g(el).memberScope).toBeNull();
      failing.add('p-1');
      await pick(el, 'p-1');
      expect(g(el).error).toBe('HTTP 500');
      expect(g(el).agents).toEqual([]);
      expect(banner(el, 'incomplete')).toBeNull();
      expect(treeView(el)).toBeNull();
    });

    it('a first-page failure for another project shows the error, not the previous graph', async () => {
      const fake = newFake(1200);
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('projectId') === 'p-2'))
      );
      const el = await mountGraph('?project=p-1');
      await pick(el, 'p-2');
      expect(graphRequests(fake, 2)).toEqual([
        PROJECT_PAGE('p-2'),
        PROJECT_PAGE('p-2'),
        PROJECT_PAGE('p-2'),
      ]);
      expect(g(el).error).toBe('HTTP 500');
      expect(banner(el, 'incomplete')).toBeNull();
      expect(treeView(el)).toBeNull();
    });

    it('a failed multi-page unscoped drain counts as large: a project choice drains with no probe', async () => {
      const fake = newFake(1201);
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('cursor') === '1000'))
      );
      const el = await mountGraph();
      expect(bannerText(el, 'incomplete')).toBe('Incomplete: loaded 1,000');
      const before = fake.requests.length;
      await pick(el, 'p-1');
      expect(graphRequests(fake, before)).toEqual([PROJECT_PAGE('p-1')]);
    });
  });

  describe('Retry and Refresh', () => {
    it('keep the graph shown with a loading button, and a second click while reloading sends nothing', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mountGraph('?project=p-1');
      reconnect();
      await el.updateComplete;
      h.hold(1);
      (banner(el, 'stale')?.querySelector('sl-button') as HTMLElement).click();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      await el.updateComplete;
      expect(g(el).loading).toBe(false);
      expect(g(el).reloading).toBe(true);
      expect(treeView(el)).not.toBeNull();
      expect(el.shadowRoot?.querySelector('sl-spinner')).toBeNull();
      const button = banner(el, 'stale')?.querySelector('sl-button');
      expect(button?.hasAttribute('loading')).toBe(true);
      expect(button?.hasAttribute('disabled')).toBe(true);
      g(el).onReload();
      expect(h.sent).toHaveLength(2);
      expect(h.sent[1].signal?.aborted).toBe(false);
      h.release();
      await settle(el);
      expect(graphRequests(fake, 2)).toEqual([PROJECT_PAGE('p-1')]);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('a picker change during a Refresh of a held set clears the reloading state and leaves Refresh enabled', async () => {
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), () => true);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      holdInState(fake.agents, true);
      const el = await mountGraph();
      await new Promise((resolve) => setTimeout(resolve, 0));
      reconnect();
      await el.updateComplete;
      h.hold(1);
      (banner(el, 'stale')?.querySelector('sl-button') as HTMLElement).click();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      expect(g(el).reloading).toBe(true);
      g(el).setProjectFilter('p-2');
      await el.updateComplete;
      expect(h.sent[0].signal?.aborted).toBe(true);
      expect(g(el).reloading).toBe(false);
      await settle(el);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-2'));
      const button = banner(el, 'stale')?.querySelector('sl-button');
      expect(button?.hasAttribute('disabled')).toBe(false);
      expect(button?.hasAttribute('loading')).toBe(false);
      // A later Refresh still drains.
      await clickBanner(el, 'stale');
      expect(h.sent.map((r) => r.url)).toEqual([ALL_PAGE(), ALL_PAGE()]);
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('the Retry button of the incomplete banner shows loading and is disabled while it drains', async () => {
      const fake = newFake(2001);
      const h = holdable(fakeFetch(fake), (u) => !u.searchParams.has('cursor'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mountGraph();
      h.hold(1);
      (banner(el, 'incomplete')?.querySelector('sl-button') as HTMLElement).click();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      await el.updateComplete;
      expect(banner(el, 'incomplete')?.querySelector('sl-button')?.hasAttribute('loading')).toBe(
        true
      );
      expect(banner(el, 'incomplete')?.querySelector('sl-button')?.hasAttribute('disabled')).toBe(
        true
      );
      expect(treeView(el)).not.toBeNull();
      h.release();
      await settle(el);
      expect(banner(el, 'incomplete')?.querySelector('sl-button')?.hasAttribute('loading')).toBe(
        false
      );
      expect(banner(el, 'incomplete')?.querySelector('sl-button')?.hasAttribute('disabled')).toBe(
        false
      );
    });
  });

  describe('ancestor-not-loaded marker', () => {
    it('is off for a complete graph, even for an agent whose parent is gone', async () => {
      const fake = newFake(25);
      fake.agents[3] = { ...fake.agents[3], ancestry: ['user-1', 'deleted-parent'] } as Agent;
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(treeView(el)?.markMissingAncestors).toBe(false);
      await vi.waitFor(() => expect(treeNode(el, fake.agents[3].id)).not.toBeNull());
      expect(treeNode(el, fake.agents[3].id)?.querySelector('.ancestor-missing')).toBeNull();
    });

    it('is off for a complete project graph whose agent has a parent in another project', async () => {
      const fake = newFake(1200);
      const child = fake.agents.find((a) => a.projectId === 'p-1')!;
      const parent = fake.agents.find((a) => a.projectId === 'p-2')!;
      child.ancestry = ['user-1', parent.id];
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(treeView(el)?.markMissingAncestors).toBe(false);
      await vi.waitFor(() => expect(treeNode(el, child.id)).not.toBeNull());
      expect(treeNode(el, child.id)?.querySelector('.ancestor-missing')).toBeNull();
    });

    it('is on while the graph is capped or failed, and off again once it is complete', async () => {
      const fake = newFake(2001);
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('cursor') === '500'))
      );
      const el = await mountGraph();
      expect(bannerText(el, 'incomplete')).toBe('Incomplete: loaded 500');
      expect(treeView(el)?.markMissingAncestors).toBe(true);
      el.remove();

      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el2 = await mountGraph();
      expect(bannerText(el2, 'incomplete')).toBe(CAPPED_ALL);
      expect(treeView(el2)?.markMissingAncestors).toBe(true);
      await pick(el2, 'p-1');
      expect(banner(el2, 'incomplete')).toBeNull();
      expect(treeView(el2)?.markMissingAncestors).toBe(false);
    });

    it('stays off on a complete project graph whose Refresh failed, for a parent the project filter hides', async () => {
      const fake = newFake(25);
      const child = fake.agents.find((a) => a.projectId === 'p-1')!;
      const parent = fake.agents.find((a) => a.projectId === 'p-2')!;
      child.ancestry = ['user-1', parent.id];
      let fail = false;
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, () => fail)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      await vi.waitFor(() => expect(treeNode(el, child.id)).not.toBeNull());
      expect(treeNode(el, child.id)?.querySelector('.ancestor-missing')).toBeNull();
      reconnect();
      await el.updateComplete;
      fail = true;
      await clickBanner(el, 'stale');
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 25 · showing the previous graph'
      );
      expect(g(el).agents.some((a) => a.id === parent.id)).toBe(true);
      expect(treeView(el)?.markMissingAncestors).toBe(false);
      await vi.waitFor(() => expect(treeNode(el, child.id)).not.toBeNull());
      expect(treeNode(el, child.id)?.querySelector('.ancestor-missing')).toBeNull();
    });

    it('stays off on a complete unscoped graph whose Refresh failed, for an agent whose parent is gone', async () => {
      const fake = newFake(25);
      fake.agents[3] = { ...fake.agents[3], ancestry: ['user-1', 'deleted-parent'] } as Agent;
      let fail = false;
      vi.stubGlobal('fetch', vi.fn(failingFetch(fake, () => fail)));
      const el = await mountGraph();
      reconnect();
      await el.updateComplete;
      fail = true;
      await clickBanner(el, 'stale');
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 25 · showing the previous graph'
      );
      // A second failure keeps it marked complete too.
      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 25 · showing the previous graph'
      );
      expect(treeView(el)?.markMissingAncestors).toBe(false);
      await vi.waitFor(() => expect(treeNode(el, fake.agents[3].id)).not.toBeNull());
      expect(treeNode(el, fake.agents[3].id)?.querySelector('.ancestor-missing')).toBeNull();
    });

    it('stays on for a partial or capped graph whose Retry failed', async () => {
      const fake = newFake(1201);
      fake.agents[3] = { ...fake.agents[3], ancestry: ['user-1', 'deleted-parent'] } as Agent;
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('cursor') === '500'))
      );
      const el = await mountGraph();
      expect(bannerText(el, 'incomplete')).toBe('Incomplete: loaded 500');
      await clickBanner(el, 'incomplete');
      expect(bannerText(el, 'incomplete')).toBe(
        'Incomplete: loaded 500 · showing the previous graph'
      );
      expect(treeView(el)?.markMissingAncestors).toBe(true);
      await vi.waitFor(() => expect(treeNode(el, fake.agents[3].id)).not.toBeNull());
      expect(treeNode(el, fake.agents[3].id)?.querySelector('.ancestor-missing')).not.toBeNull();
      el.remove();

      const capped = newFake(2001);
      let fail = false;
      vi.stubGlobal('fetch', vi.fn(failingFetch(capped, () => fail)));
      const el2 = await mountGraph();
      fail = true;
      await clickBanner(el2, 'incomplete');
      expect(bannerText(el2, 'incomplete')).toBe(`${CAPPED_ALL} · showing the previous graph`);
      expect(treeView(el2)?.markMissingAncestors).toBe(true);
      // The kept capped set, reused for all projects, still marks.
      fail = false;
      await pick(el2, 'p-1');
      expect(treeView(el2)?.markMissingAncestors).toBe(false);
      await pick(el2, '');
      expect(bannerText(el2, 'incomplete')).toBe(CAPPED_ALL);
      expect(treeView(el2)?.markMissingAncestors).toBe(true);
    });
  });

  describe('detach and re-attach', () => {
    it('a detached page aborts its drain, drops its listeners and ignores live changes', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.get('cursor') === '500');
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const add = vi.spyOn(stateManager, 'addEventListener');
      const remove = vi.spyOn(stateManager, 'removeEventListener');
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      el.remove();
      expect(h.sent[0].signal?.aborted).toBe(true);
      await vi.waitFor(() => expect(fetchState().pending).toBe(0));
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(fake.requests).toEqual([ALL_PAGE()]);
      for (const name of ['agent-created', 'agents-changed', 'agents-resync']) {
        expect(listenerBalance(add, remove, name)).toBe(0);
      }
      liveUpdate('agent.n-late.created', makeAgent(9300, { id: 'n-late', projectId: 'p-1' }));
      expect(g(el).agents).toEqual([]);
    });

    it('a re-attached page starts clean: it probes again and shows no stale banner or capped set', async () => {
      const fake = newFake(1200);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      reconnect();
      await el.updateComplete;
      expect(bannerText(el, 'stale')).toBe(STALE);
      el.remove();
      expect(g(el).probed).toBe(false);
      expect(g(el).memberScope).toBeNull();
      expect(g(el).stale).toBe(false);
      expect(g(el).agents).toEqual([]);
      document.body.appendChild(el);
      await el.updateComplete;
      await settle(el);
      expect(graphRequests(fake, 2)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(banner(el, 'stale')).toBeNull();
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('a re-attached page drops a capped set it kept, and drains again', async () => {
      const fake = newFake(2001);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(g(el).cappedAll).not.toBeNull();
      el.remove();
      expect(g(el).cappedAll).toBeNull();
      // Missed while detached.
      const victim = fake.agents[0].id;
      liveUpdate(`agent.${victim}.deleted`, { agentId: victim });
      fake.agents.shift();
      document.body.appendChild(el);
      await el.updateComplete;
      await settle(el);
      expect(graphRequests(fake, 4)).toEqual([
        ALL_PAGE(),
        ALL_PAGE(500),
        ALL_PAGE(1000),
        ALL_PAGE(1500),
      ]);
      expect(g(el).agents.some((a) => a.id === victim)).toBe(false);
    });

    it('a re-attached page ignores the first-connect timer of its earlier attachment', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled();
      vi.advanceTimersByTime(1000);
      el.remove();
      document.body.appendChild(el);
      await el.updateComplete;
      // The earlier attachment's 3 s would end here.
      vi.advanceTimersByTime(2000);
      await el.updateComplete;
      expect(g(el).stale).toBe(false);
      stateManager.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
      await stateManager.sseConnected(stateManager.scopeGeneration);
      vi.advanceTimersByTime(3000);
      await el.updateComplete;
      expect(g(el).stale).toBe(false);
      expect(banner(el, 'stale')).toBeNull();
      expect(fake.requests).toEqual([]);
    });

    it('a page re-attached after a failed multi-page drain forgets the large set and probes again', async () => {
      const fake = newFake(1200);
      vi.stubGlobal(
        'fetch',
        vi.fn(failingFetch(fake, (u) => u.searchParams.get('cursor') === '1000'))
      );
      const el = await mountGraph();
      expect(g(el).knownLarge).toBe(true);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);
      el.remove();
      expect(g(el).knownLarge).toBe(false);
      window.history.replaceState({}, '', '/agents/graph?project=p-1');
      const before = fake.requests.length;
      document.body.appendChild(el);
      await el.updateComplete;
      await settle(el);
      expect(graphRequests(fake, before)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
    });

    it('a re-attached page does not carry a late first connect of its earlier attachment into its probe', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      stateManager.setScope({ type: 'agent-detail', agentId: 'x' });
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), (u) => !u.searchParams.has('sort'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled('', { connectTimeoutMs: 60_000 });
      vi.advanceTimersByTime(3000);
      expect((el as unknown as { firstConnectLate: boolean }).firstConnectLate).toBe(true);
      vi.useRealTimers();
      el.remove();
      window.history.replaceState({}, '', '/agents/graph?project=p-1');
      const hp = holdable(fakeFetch(fake), (u) => u.searchParams.has('sort'));
      vi.stubGlobal('fetch', vi.fn(hp.fn));
      hp.hold(1);
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(hp.heldCount).toBe(1));
      // The connection comes up after re-attach, within the new attachment's
      // connect wait, while its probe is still in flight.
      stateManager.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
      await stateManager.sseConnected(stateManager.scopeGeneration);
      hp.release();
      await settle(el);
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(g(el).stale).toBe(false);
      expect(banner(el, 'stale')).toBeNull();
    });

    it('a re-attached probe stays unconnected when its earlier attachment connects late, and its answer after the connect timeout shows the stale banner', async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      const connects = deferredConnects();
      const fake = newFake(25);
      const h1 = holdable(fakeFetch(fake), (u) => u.searchParams.has('sort'));
      vi.stubGlobal('fetch', vi.fn(h1.fn));
      h1.hold(1);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h1.heldCount).toBe(1));
      el.remove();
      const h2 = holdable(fakeFetch(fake), (u) => u.searchParams.has('sort'));
      vi.stubGlobal('fetch', vi.fn(h2.fn));
      h2.hold(1);
      document.body.appendChild(el);
      await el.updateComplete;
      await vi.waitFor(() => expect(h2.heldCount).toBe(1));
      expect(connects).toHaveLength(2);
      // The first attachment's wait resolves while the second is still unconnected.
      connects[0].resolve();
      await connects[0].promise;
      expect((el as unknown as { firstConnected: boolean }).firstConnected).toBe(false);
      vi.advanceTimersByTime(3000);
      expect((el as unknown as { firstConnectLate: boolean }).firstConnectLate).toBe(true);
      expect((el as unknown as { firstConnected: boolean }).firstConnected).toBe(false);
      vi.useRealTimers();
      h2.release();
      await settle(el);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(g(el).stale).toBe(true);
      expect(bannerText(el, 'stale')).toBe(STALE);
    });

    it("an earlier attachment's late connect does not cancel the re-attached page's connect timer", async () => {
      vi.stubGlobal('EventSource', SilentEventSource);
      const connects = deferredConnects();
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const el = await mountUnsettled();
      vi.advanceTimersByTime(1000);
      el.remove();
      document.body.appendChild(el);
      await el.updateComplete;
      expect(connects).toHaveLength(2);
      connects[0].resolve();
      await connects[0].promise;
      vi.advanceTimersByTime(3000);
      await el.updateComplete;
      expect(g(el).stale).toBe(true);
      expect(bannerText(el, 'stale')).toBe(STALE);
      expect(fake.requests).toEqual([]);
    });
  });

  /** Sets the deletion view the fake server lists on the row for `id`. */
  function setServerDeletion(fake: Fake, id: string, deletion: DeletionInfo | null): void {
    fake.agents = fake.agents.map((a) => (a.id === id ? { ...a, deletion } : a));
  }

  describe('an agent whose delete the hub accepted', () => {
    const deletingView = (): DeletionInfo => ({
      state: 'deleting',
      soft: false,
      claim: 1,
      startedAt: new Date().toISOString(),
      leaseExpiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
    });

    function member(el: TestEl, id: string): Agent | undefined {
      return g(el).agents.find((a) => a.id === id);
    }

    it('stays in the graph with its deletion view through a re-drain of compact rows; the live delete removes it and a later drain keeps it out', async () => {
      // The hub keeps listing an agent whose delete it accepted, with the
      // deleting view, until the delete finishes; every other row is null.
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      const id = fake.agents[4].id;

      expect(stateManager.applyDeleteAccepted(id, deletingView())).toBe(true);
      setServerDeletion(fake, id, deletingView());
      (stateManager as unknown as { flush(): void }).flush();
      await el.updateComplete;
      expect(member(el, id)?.deletion?.state).toBe('deleting');
      expect(g(el).agents).toHaveLength(25);
      expect(treeNode(el, id)).not.toBeNull();

      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(graphRequests(fake, 1)).toEqual([ALL_PAGE()]);
      expect(g(el).agents).toHaveLength(25);
      expect(member(el, id)?.deletion?.state).toBe('deleting');
      // The re-drained row carries the deleting view; every other row stays clear.
      expect(stateManager.getAgent(id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(fake.agents[5].id)?.deletion).toBeNull();

      liveUpdate(`agent.${id}.deleted`, { agentId: id });
      await el.updateComplete;
      expect(member(el, id)).toBeUndefined();
      expect(g(el).agents).toHaveLength(24);
      expect(treeNode(el, id)).toBeNull();

      // The server still lists the row: a drain after the delete leaves it out.
      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(graphRequests(fake, 2)).toEqual([ALL_PAGE()]);
      expect(member(el, id)).toBeUndefined();
      expect(g(el).agents).toHaveLength(24);
      expect(stateManager.getAgent(id)).toBeUndefined();
    });

    it('a deletion view that arrives live during an unscoped drain is kept on the row; the live delete then removes it', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.get('cursor') === '500');
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const id = fake.agents[3].id;
      liveUpdate(`agent.${id}.status`, { agentId: id, deletion: deletingView() });
      h.release();
      await settle(el);
      expect(g(el).agents).toHaveLength(1200);
      expect(member(el, id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(id)?.deletion?.state).toBe('deleting');

      liveUpdate(`agent.${id}.deleted`, { agentId: id });
      await el.updateComplete;
      expect(member(el, id)).toBeUndefined();
      expect(g(el).agents).toHaveLength(1199);
    });

    it('a deletion view that arrives live during a project drain is kept on the row', async () => {
      const fake = newFake(1200);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('projectId'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const id = projectIds(fake, 'p-1')[2];
      liveUpdate(`agent.${id}.status`, { agentId: id, deletion: deletingView() });
      h.release();
      await settle(el);
      expect(ids(g(el).agents)).toEqual(projectIds(fake, 'p-1'));
      expect(member(el, id)?.deletion?.state).toBe('deleting');
    });

    it('a deletion view that arrives live during the fit probe is kept on the row; the live delete then removes it', async () => {
      const fake = newFake(25);
      const h = holdable(fakeFetch(fake), (u) => u.searchParams.has('fit'));
      vi.stubGlobal('fetch', vi.fn(h.fn));
      h.hold(1);
      const el = await mountUnsettled('?project=p-1');
      await vi.waitFor(() => expect(h.heldCount).toBe(1));
      const id = projectIds(fake, 'p-1')[0];
      liveUpdate(`agent.${id}.status`, { agentId: id, deletion: deletingView() });
      h.release();
      await settle(el);
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      expect(member(el, id)?.deletion?.state).toBe('deleting');

      liveUpdate(`agent.${id}.deleted`, { agentId: id });
      await el.updateComplete;
      expect(member(el, id)).toBeUndefined();
      expect(g(el).visibleAgents.some((a) => a.id === id)).toBe(false);
    });

    it('a deletion view and full fields the store held before the fit probe survive its compact seed', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const [id, fullId] = projectIds(fake, 'p-1');
      // An earlier page loaded full objects (with their applied config); the set is not marked complete.
      holdInState(
        fake.agents.map((a) =>
          a.id === fullId ? ({ ...a, appliedConfig: { image: 'img:full' } } as Agent) : a
        ),
        false
      );
      expect(stateManager.applyDeleteAccepted(id, deletingView())).toBe(true);
      // After the 202 the server lists the row with the deleting view.
      setServerDeletion(fake, id, deletingView());
      (stateManager as unknown as { flush(): void }).flush();
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(member(el, id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(fullId)?.deletion).toBeNull();
      expect(member(el, fullId)?.appliedConfig?.image).toBe('img:full');
      expect(stateManager.getAgent(fullId)?.appliedConfig?.image).toBe('img:full');
    });

    it("an older hub's compact row without the deletion key keeps the store's view through the fit probe", async () => {
      const fake = newFake(25);
      for (const a of fake.agents) delete (a as { deletion?: unknown }).deletion;
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const [id, otherId] = projectIds(fake, 'p-1');
      holdInState(fake.agents, false);
      expect(stateManager.applyDeleteAccepted(id, deletingView())).toBe(true);
      (stateManager as unknown as { flush(): void }).flush();
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect('deletion' in fake.agents[0]).toBe(false);
      expect(member(el, id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(id)?.deletion?.state).toBe('deleting');
      expect(stateManager.getAgent(otherId)?.deletion).toBeUndefined();
    });

    it('a held complete set shows the row with its deletion view, with no request', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      holdInState(fake.agents, true);
      const id = fake.agents[6].id;
      expect(stateManager.applyDeleteAccepted(id, deletingView())).toBe(true);
      (stateManager as unknown as { flush(): void }).flush();
      const el = await mountGraph();
      expect(fake.requests).toEqual([]);
      expect(g(el).agents).toHaveLength(25);
      expect(member(el, id)?.deletion?.state).toBe('deleting');
    });
  });

  describe('a cold-loaded compact set with deletion views', () => {
    const deletingView = (): DeletionInfo => ({
      state: 'deleting',
      soft: false,
      claim: 2,
      startedAt: new Date().toISOString(),
      leaseExpiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
    });
    const failedView = (): DeletionInfo => ({
      state: 'failed',
      code: 'runtime_error',
      error: 'broker unreachable',
      soft: false,
      claim: 1,
      startedAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
    });

    /**
     * Gives `failedId` a failed view and `deletingId` a deleting one, and
     * every other row an explicit null, as the hub's compact rows carry.
     */
    function withDeletions(fake: Fake, failedId: string, deletingId: string): void {
      fake.agents = fake.agents.map((a) => ({
        ...a,
        deletion: a.id === failedId ? failedView() : a.id === deletingId ? deletingView() : null,
      }));
    }

    /** The compact deletion badge's text and title on the graph node for `id`. */
    async function nodeBadge(
      el: TestEl,
      id: string
    ): Promise<{ text: string; title: string } | null> {
      const badge = treeNode(el, id)?.querySelector<
        HTMLElement & { updateComplete: Promise<boolean> }
      >('scion-deletion-badge');
      await badge?.updateComplete;
      const inner = badge?.shadowRoot?.querySelector<HTMLElement>('.badge');
      return inner ? { text: inner.textContent?.trim() ?? '', title: inner.title } : null;
    }

    async function expectBadges(el: TestEl, failedId: string, deletingId: string, plainId: string) {
      expect(await nodeBadge(el, failedId)).toEqual({
        text: 'Delete failed',
        title: 'Delete failed: broker unreachable',
      });
      expect(await nodeBadge(el, deletingId)).toEqual({ text: 'Deleting…', title: 'Deleting…' });
      expect(await nodeBadge(el, plainId)).toBeNull();
    }

    it('the unscoped drain renders a failed and a deleting row with the compact badge at once', async () => {
      const fake = newFake(25);
      const [failedId, deletingId, plainId] = [
        fake.agents[2].id,
        fake.agents[5].id,
        fake.agents[7].id,
      ];
      withDeletions(fake, failedId, deletingId);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      expect(graphRequests(fake)).toEqual([ALL_PAGE()]);
      expect(g(el).agents).toHaveLength(25);
      await expectBadges(el, failedId, deletingId, plainId);
    });

    it('the fit probe renders a failed and a deleting row with the compact badge at once', async () => {
      const fake = newFake(25);
      const [failedId, deletingId, plainId] = projectIds(fake, 'p-1');
      withDeletions(fake, failedId, deletingId);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      expect(ids(g(el).visibleAgents)).toEqual(projectIds(fake, 'p-1'));
      await expectBadges(el, failedId, deletingId, plainId);
    });

    it("the server's deletion value replaces the store's on the fit probe and on a re-drain, null included", async () => {
      const fake = newFake(25);
      const [clearedId, retriedId, plainId] = projectIds(fake, 'p-1');
      // The store holds a failed view and an older deleting view; the server
      // has since cleared the first and lists a retried delete (claim 2) for
      // the second.
      holdInState(
        fake.agents.map((a) =>
          a.id === clearedId
            ? { ...a, deletion: failedView() }
            : a.id === retriedId
              ? { ...a, deletion: { ...deletingView(), claim: 1 } }
              : a
        ),
        false
      );
      setServerDeletion(fake, retriedId, deletingView());
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE]);
      const expectServerViews = async (): Promise<void> => {
        expect(stateManager.getAgent(clearedId)?.deletion).toBeNull();
        expect(stateManager.getAgent(retriedId)?.deletion).toMatchObject({
          state: 'deleting',
          claim: 2,
        });
        expect(await nodeBadge(el, clearedId)).toBeNull();
        expect(await nodeBadge(el, retriedId)).toEqual({ text: 'Deleting…', title: 'Deleting…' });
        expect(await nodeBadge(el, plainId)).toBeNull();
      };
      await expectServerViews();

      // Live views that the server later cleared or replaced, whose clearing
      // events the dropped connection missed; the re-drain restores the
      // server's values.
      liveUpdate(`agent.${clearedId}.status`, { agentId: clearedId, deletion: failedView() });
      liveUpdate(`agent.${retriedId}.status`, {
        agentId: retriedId,
        deletion: { ...deletingView(), claim: 1 },
      });
      await el.updateComplete;
      expect(await nodeBadge(el, clearedId)).toEqual({
        text: 'Delete failed',
        title: 'Delete failed: broker unreachable',
      });
      expect(stateManager.getAgent(retriedId)?.deletion?.claim).toBe(1);
      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(graphRequests(fake, 1)).toEqual([ALL_PAGE()]);
      await expectServerViews();
    });

    it('the project drain renders a failed and a deleting row with the compact badge at once', async () => {
      const fake = newFake(1200);
      const [failedId, deletingId, plainId] = projectIds(fake, 'p-1');
      withDeletions(fake, failedId, deletingId);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph('?project=p-1');
      expect(graphRequests(fake)).toEqual([PROBE, PROJECT_PAGE('p-1')]);
      expect(ids(g(el).agents)).toEqual(projectIds(fake, 'p-1'));
      await expectBadges(el, failedId, deletingId, plainId);
    });
  });

  describe('a live delete followed by a live create of the same ID', () => {
    it('the store drops the create, and a drain whose server still lists the row keeps it out', async () => {
      const fake = newFake(25);
      vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
      const el = await mountGraph();
      const victim = fake.agents[4];
      liveUpdate(`agent.${victim.id}.deleted`, { agentId: victim.id });
      liveUpdate(`agent.${victim.id}.created`, { ...victim, phase: 'stopped' });
      await el.updateComplete;
      expect(stateManager.getAgent(victim.id)).toBeUndefined();
      expect(g(el).agents.some((a) => a.id === victim.id)).toBe(false);

      reconnect();
      await el.updateComplete;
      await clickBanner(el, 'stale');
      expect(graphRequests(fake, 1)).toEqual([ALL_PAGE()]);
      expect(g(el).agents.some((a) => a.id === victim.id)).toBe(false);
      expect(g(el).agents).toHaveLength(24);
      expect(stateManager.getAgent(victim.id)).toBeUndefined();
    });
  });
});
