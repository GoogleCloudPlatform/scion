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
 * The global agents page through the agent list window: the sorted fit
 * request, the reuse branch and the completeness flag it sets, the
 * complete-needing views (tree, mode filter), refresh costs per window
 * state, the error path, the empty states, and count-only mode above
 * 2,000 agents. Uses the real `stateManager`; only `fetch`, `EventSource`
 * and `localStorage` are faked.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';
import type { AgentListWindow } from '../../client/agent-list-window.js';
import { AgentDrainRunner } from '../../client/agent-drain.js';
import {
  FakeEventSource,
  SCOPE_CAPS,
  fakeFetch,
  holdable,
  isGlobalAgentsList,
  isMine,
  isShared,
  makeAgent,
  type Fake,
} from './__fixtures__/global-agents-endpoint.js';

interface Internals {
  agentWindow: AgentListWindow;
  agents: Agent[];
  error: string | null;
  loading: boolean;
  agentsLoading: boolean;
  committedLabel: string;
  toggleSort(field: string): void;
  setPhaseFilter(phase: string): void;
  setModeFilter(mode: string): void;
  setScope(scope: 'all' | 'mine' | 'shared'): void;
  backgroundRefresh(trigger: string): void;
  handleAgentAction(id: string, action: string, event?: MouseEvent): Promise<void>;
}

type TestEl = HTMLElement & { updateComplete: Promise<boolean> };

function internals(el: TestEl): Internals {
  return el as unknown as Internals;
}

/**
 * Waits until the page is idle (no page load, no agents load, no window
 * page fetch), runs any pending live flush, re-renders, and checks the page
 * is still idle, so a zero-request assertion never races a late request.
 */
async function settle(el: TestEl): Promise<void> {
  const idle = (): void => {
    expect(internals(el).loading).toBe(false);
    expect(internals(el).agentsLoading).toBe(false);
    expect(internals(el).agentWindow.loading).toBe(false);
  };
  await vi.waitFor(idle);
  (stateManager as unknown as { flush(): void }).flush();
  await el.updateComplete;
  await vi.waitFor(idle);
}

/** Mounts the page and returns once its first request is sent, without waiting for it. */
async function mountUnsettled(): Promise<TestEl> {
  const el = document.createElement('scion-page-agents') as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: '/agents',
    title: 'Agents',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  // Drain retries without a delay.
  (el as unknown as { drainRunner: AgentDrainRunner }).drainRunner = new AgentDrainRunner({
    retryDelayMs: 0,
  });
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function mount(): Promise<TestEl> {
  const el = await mountUnsettled();
  await settle(el);
  return el;
}

/** The query of a request URL. */
function query(url: string): URLSearchParams {
  return new URL(url, 'http://x').searchParams;
}

function unmount(el: TestEl): void {
  el.remove();
}

function text(el: TestEl): string {
  return el.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';
}

function labelInput(el: TestEl): HTMLElement & { value: string } {
  const inputs = Array.from(el.shadowRoot?.querySelectorAll('sl-input') ?? []);
  return inputs.find(
    (i) => i.getAttribute('placeholder') === 'Filter by label (key=value)'
  ) as HTMLElement & { value: string };
}

function typeLabel(el: TestEl, value: string): void {
  const input = labelInput(el);
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
}

function commitLabel(el: TestEl, value: string): void {
  typeLabel(el, value);
  labelInput(el).dispatchEvent(new Event('sl-change'));
}

function setView(el: TestEl, view: string): void {
  el.shadowRoot
    ?.querySelector('scion-view-toggle')
    ?.dispatchEvent(new CustomEvent('view-change', { detail: { view } }));
}

function pager(el: TestEl): HTMLElement & { showChip: boolean; chipText: string } {
  return el.shadowRoot?.querySelector('scion-agent-pager') as HTMLElement & {
    showChip: boolean;
    chipText: string;
  };
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

/**
 * Drops and reopens the live connection through the state store's own
 * SSE client, so the resync goes through the real state path.
 */
function reconnect(): void {
  const sse = stateManager.sseClientInstance;
  sse.dispatchEvent(new CustomEvent('disconnected'));
  sse.dispatchEvent(new CustomEvent('connected'));
}

function stubFake(fake: Fake): void {
  vi.stubGlobal('fetch', vi.fn(fakeFetch(fake)));
}

function hasStopAll(el: TestEl): boolean {
  return Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).some((b) =>
    b.textContent?.includes('Stop All')
  );
}

describe('scion-page-agents — agent list window', () => {
  beforeAll(async () => {
    await import('./agents.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // A real scope change clears the state store and the completeness flag.
    stateManager.setScope({ type: 'brokers-list' });
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'list');
    vi.setConfig({ testTimeout: 30_000 });
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-agents').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  describe('the first request', () => {
    it('is one sorted fit request with stats; the scope is sent only when not all, the label only with =, and the phase when set', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-filter-agents-phase', 'running');
      const el = await mount();
      expect(fake.requests.length).toBe(1);
      const q = new URL(fake.requests[0], 'http://x').searchParams;
      expect(q.get('sort')).toBe('updated');
      expect(q.get('dir')).toBe('desc');
      expect(q.get('limit')).toBe('25');
      expect(q.get('fit')).toBe('500');
      expect(q.get('stats')).toBe('1');
      expect(q.get('phase')).toBe('running');
      expect(q.has('scope')).toBe(false);
      expect(q.has('label')).toBe(false);

      commitLabel(el, 'env');
      await settle(el);
      // A bare key is a complete-needing label: a drain, sent without the label.
      const drainQ = new URL(fake.requests.at(-1)!, 'http://x').searchParams;
      expect(drainQ.has('sort')).toBe(false);
      expect(drainQ.has('label')).toBe(false);
      commitLabel(el, 'env=prod');
      await settle(el);
      expect(new URL(fake.requests.at(-1)!, 'http://x').searchParams.get('label')).toBe('env=prod');
      internals(el).setScope('mine');
      await settle(el);
      expect(new URL(fake.requests.at(-1)!, 'http://x').searchParams.get('scope')).toBe('mine');
    });

    it('a complete response renders every agent with the scope capabilities; above 500 the first page is shown', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 30 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agentWindow.stats.total).toBe(30);
      expect(el.shadowRoot?.querySelectorAll('tbody tr').length).toBe(25);
      expect(text(el)).toContain('New Agent');

      unmount(el);
      stateManager.setScope({ type: 'brokers-list' });
      fake.agents = Array.from({ length: 1200 }, (_, i) => makeAgent(i));
      fake.requests.length = 0;
      const el2 = await mount();
      expect(fake.requests.length).toBe(1);
      expect(internals(el2).agentWindow.state).toBe('paged');
      expect(internals(el2).agents).toEqual([]);
      expect(internals(el2).agentWindow.stats.total).toBe(1200);
      expect(el2.shadowRoot?.querySelectorAll('tbody tr').length).toBe(25);
      expect(internals(el2).agentWindow.items[0].id).toBe('g-01199');
    });

    it('a legacy server (no sorted mode) that ignores limit is drained from its first page', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      const legacy = (input: string | URL | Request, init?: RequestInit) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const u = new URL(raw, 'http://localhost');
        for (const k of ['sort', 'dir', 'fit', 'stats', 'limit', 'phase']) u.searchParams.delete(k);
        return fakeFetch(fake)(u.pathname + (u.search || ''), init);
      };
      vi.stubGlobal('fetch', vi.fn(legacy));
      const el = await mount();
      // The drain continues from the legacy first page and its cursor:
      // the first request plus pages 2 and 3, with no refetch of page 1.
      expect(fake.requests.length).toBe(3);
      expect(new URL(fake.requests[1], 'http://localhost').searchParams.get('cursor')).toBe('500');
      expect(internals(el).agentWindow.state).toBe('held');
      expect(internals(el).agents.length).toBe(1200);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
    });

    it('1,600 agents at page size 25 on a legacy server that honours limit end held and complete in five requests', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1600 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      // An old server: no sorted mode, but `limit` is honoured.
      const legacy = (input: string | URL | Request, init?: RequestInit) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const u = new URL(raw, 'http://localhost');
        for (const k of ['sort', 'dir', 'fit', 'stats', 'phase']) u.searchParams.delete(k);
        return fakeFetch(fake)(u.pathname + (u.search || ''), init);
      };
      vi.stubGlobal('fetch', vi.fn(legacy));
      const el = await mount();
      expect(query(fake.requests[0]).get('limit')).toBe('25');
      // The 25-row answer, then four drain pages of 500 from its cursor.
      expect(fake.requests.length).toBeLessThanOrEqual(5);
      expect(fake.requests.slice(1).map((r) => query(r).get('cursor'))).toEqual([
        '25',
        '525',
        '1025',
        '1525',
      ]);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('held');
      expect(win.banner).toBeNull();
      expect(internals(el).agents).toHaveLength(1600);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(text(el)).not.toContain('more exist');
    });
  });

  describe('a legacy fallback and drain failures', () => {
    it('a legacy server that honours phase answers a phased first request with part of the set: the whole set is drained, not marked complete from that page', async () => {
      const agents = Array.from({ length: 1200 }, (_, i) =>
        makeAgent(i, { phase: i % 3 === 0 ? 'stopped' : 'running' })
      );
      const requests: string[] = [];
      const full: Fake = { agents, requests };
      const stopped: Fake = { agents: agents.filter((a) => a.phase === 'stopped'), requests };
      const legacy = (input: string | URL | Request, init?: RequestInit) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const u = new URL(raw, 'http://localhost');
        const phase = u.searchParams.get('phase');
        for (const k of ['sort', 'dir', 'fit', 'stats', 'limit', 'phase']) u.searchParams.delete(k);
        const target = phase === 'stopped' ? stopped : full;
        return fakeFetch(target)(u.pathname + (u.search || ''), init);
      };
      vi.stubGlobal('fetch', vi.fn(legacy));
      localStorage.setItem('scion-filter-agents-phase', 'stopped');
      const el = await mount();
      // The phased first request, then three unphased legacy pages.
      expect(requests.length).toBe(1 + 3);
      expect(query(requests[1]).has('cursor')).toBe(false);
      expect(query(requests[1]).has('phase')).toBe(false);
      expect(internals(el).agents).toHaveLength(1200);
      expect(internals(el).agentWindow.state).toBe('held');
      expect(internals(el).agentWindow.total).toBe(400);
    });

    it('an empty readable first drain page then a failing page is an incomplete set, not the error path', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      const inner = fakeFetch(fake);
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(raw, 'http://localhost');
          if (isGlobalAgentsList(u) && !u.searchParams.has('sort')) {
            fake.requests.push(raw);
            if (!u.searchParams.has('cursor')) {
              return new Response(
                JSON.stringify({ agents: [], nextCursor: '500', _capabilities: SCOPE_CAPS }),
                { status: 200, headers: { 'Content-Type': 'application/json' } }
              );
            }
            return new Response('{}', { status: 502 });
          }
          return inner(input, init);
        })
      );
      localStorage.setItem('scion-filter-agents-mode', 'project');
      const el = await mount();
      expect(internals(el).error).toBeNull();
      expect(internals(el).agentWindow.state).toBe('capped');
      expect(internals(el).agentWindow.banner?.kind).toBe('failed');
      expect(text(el)).toContain('Incomplete: loaded 0');
    });
  });

  describe('full-view pages replace stored agents', () => {
    it('a field missing from a later full-view page is gone from the store', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i, { taskSummary: 'old task' })),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('paged');
      const id = internals(el).agentWindow.items[0].id;
      expect(stateManager.getAgent(id)?.taskSummary).toBe('old task');
      fake.agents = fake.agents.map((a) => {
        const { taskSummary: _dropped, ...rest } = a;
        return rest as Agent;
      });
      await internals(el).agentWindow.refresh();
      await el.updateComplete;
      expect(stateManager.getAgent(id)).toBeDefined();
      expect(stateManager.getAgent(id)?.taskSummary).toBeUndefined();
    });
  });

  describe('a view change while a request is in flight', () => {
    const phased = (n: number) =>
      Array.from({ length: n }, (_, i) =>
        makeAgent(i, { phase: i % 3 === 0 ? 'stopped' : 'running' })
      );
    const byUpdated = (dir: 'asc' | 'desc') => (a: Agent, b: Agent) =>
      (dir === 'asc' ? 1 : -1) * (a.updated ?? '').localeCompare(b.updated ?? '');

    for (const change of ['phase', 'dir'] as const) {
      it(`a ${change} change during the first load supersedes it and loads the new view`, async () => {
        const fake: Fake = { agents: phased(1200), requests: [] };
        const h = holdable(fakeFetch(fake), isGlobalAgentsList);
        h.hold();
        vi.stubGlobal('fetch', vi.fn(h.fn));
        const el = await mountUnsettled();
        await vi.waitFor(() => expect(h.sent).toHaveLength(1));
        if (change === 'phase') internals(el).setPhaseFilter('stopped');
        else internals(el).toggleSort('updated');
        h.release();
        await settle(el);

        expect(h.sent[0].signal?.aborted).toBe(true);
        expect(h.sent).toHaveLength(2);
        const q = query(h.sent[1].url);
        if (change === 'phase') expect(q.get('phase')).toBe('stopped');
        else expect(q.get('dir')).toBe('asc');
        const win = internals(el).agentWindow;
        expect(win.state).toBe('paged');
        expect(win.planRequest('view-change', '')).toBe('none');
        const expected = fake.agents
          .filter((a) => change !== 'phase' || a.phase === 'stopped')
          .sort(byUpdated(change === 'dir' ? 'asc' : 'desc'))
          .slice(0, 25)
          .map((a) => a.id);
        expect(win.items.map((a) => a.id)).toEqual(expected);
      });
    }

    it('A to B to A while paged: the B request is aborted and the A page stays, with no request', async () => {
      const fake: Fake = { agents: phased(1200), requests: [] };
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mount();
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      const before = win.items.map((a) => a.id);
      h.hold();
      internals(el).toggleSort('updated'); // desc to asc
      await vi.waitFor(() => expect(h.sent).toHaveLength(2));
      internals(el).toggleSort('updated'); // back to desc
      h.release();
      await settle(el);

      expect(h.sent[1].signal?.aborted).toBe(true);
      expect(h.sent).toHaveLength(2);
      expect(win.state).toBe('paged');
      expect(win.items.map((a) => a.id)).toEqual(before);
      expect(win.planRequest('view-change', '')).toBe('none');
    });

    it('a switch to name sort during the first load drains the set instead of dead-ending', async () => {
      const fake: Fake = { agents: phased(1200), requests: [] };
      const h = holdable(fakeFetch(fake), isGlobalAgentsList);
      h.hold();
      vi.stubGlobal('fetch', vi.fn(h.fn));
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.sent).toHaveLength(1));
      internals(el).toggleSort('name');
      h.release();
      await settle(el);

      expect(h.sent[0].signal?.aborted).toBe(true);
      // The aborted fit request, then the three legacy drain pages.
      expect(h.sent).toHaveLength(4);
      expect(query(h.sent[1].url).has('sort')).toBe(false);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('held');
      expect(win.items).toHaveLength(25);
      expect(text(el)).not.toContain('Could not load every agent');
      expect(text(el)).not.toContain('Loading agents');
    });
  });

  describe('a view change while a drain is in flight', () => {
    /** Mounts 2,001 agents in the tree view with the first drain page held, and switches to the list view. */
    async function switchDuringDrain(
      fail: boolean
    ): Promise<{ el: TestEl; sent: Array<{ url: string }> }> {
      const fake: Fake = {
        agents: Array.from({ length: 2001 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      const inner = fakeFetch(fake);
      const failing = (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (
          fail &&
          isGlobalAgentsList(new URL(raw, 'http://localhost')) &&
          query(raw).has('sort')
        ) {
          return Promise.resolve(new Response('{}', { status: 500 }));
        }
        return inner(input, init);
      };
      const h = holdable(failing, isGlobalAgentsList);
      h.hold();
      vi.stubGlobal('fetch', vi.fn(h.fn));
      localStorage.setItem('scion-view-agents', 'graph');
      const el = await mountUnsettled();
      await vi.waitFor(() => expect(h.sent).toHaveLength(1));
      expect(query(h.sent[0].url).has('sort')).toBe(false);
      setView(el, 'list');
      await el.updateComplete;
      h.release();
      await settle(el);
      return { el, sent: h.sent };
    }

    it('a drain that lands capped after a switch to an eligible view sends one sorted request and ends paged', async () => {
      const { el, sent } = await switchDuringDrain(false);
      // Four drain pages, then one sorted request for the list view.
      expect(sent).toHaveLength(5);
      expect(sent.slice(0, 4).every((r) => !query(r.url).has('sort'))).toBe(true);
      expect(query(sent[4].url).get('sort')).toBe('updated');
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      expect(win.planRequest('view-change', '')).toBe('none');
      expect(internals(el).error).toBeNull();
    });

    it('a failed sorted request after the drain landed keeps the drained rows with no page error', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      const { el, sent } = await switchDuringDrain(true);
      expect(sent).toHaveLength(5);
      expect(query(sent[4].url).has('sort')).toBe(true);
      const win = internals(el).agentWindow;
      expect(internals(el).error).toBeNull();
      expect(win.state).toBe('capped');
      expect(internals(el).agents).toHaveLength(2000);
      expect(el.shadowRoot?.querySelectorAll('tbody tr').length).toBe(25);
      expect(el.shadowRoot?.querySelector('.error-details')).toBeNull();
      expect(warn).toHaveBeenCalled();
    });
  });

  describe('the completeness flag and the reuse branch', () => {
    it('an unlabelled scope-all complete load sets full; revisiting /agents then issues no request', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      unmount(el);
      const el2 = await mount();
      expect(fake.requests.length).toBe(1);
      expect(internals(el2).agentWindow.state).toBe('small');
      expect(internals(el2).agentWindow.display.length).toBe(25);
      expect(text(el2)).toContain('New Agent');
      // A lifecycle refresh after the reuse costs one request, as today.
      internals(el2).backgroundRefresh('lifecycle-refresh');
      await settle(el2);
      expect(fake.requests.length).toBe(2);
    });

    it('a label commit after a complete load keeps the flag, and /agents then issues no request', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      commitLabel(el, 'env=prod');
      await settle(el);
      expect(fake.requests.length).toBe(2);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      unmount(el);
      const el2 = await mount();
      expect(fake.requests.length).toBe(2);
      expect(internals(el2).committedLabel).toBe('');
      expect(internals(el2).agentWindow.display.length).toBe(25);
    });

    it('mine, then a label, then all with the label still committed: no flag; a dashboard-scope page in between, then /agents issues one unlabelled load that sets it', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-scope-agents', 'mine');
      const el = await mount();
      expect(fake.requests.length).toBe(1);
      commitLabel(el, 'env=prod');
      await settle(el);
      internals(el).setScope('all');
      await settle(el);
      expect(fake.requests.length).toBe(3);
      expect(new URL(fake.requests[2], 'http://x').searchParams.get('label')).toBe('env=prod');
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      unmount(el);

      // A dashboard-scope page that loads no agents (the projects list).
      stateManager.setScope({ type: 'dashboard' });
      const el2 = await mount();
      expect(fake.requests.length).toBe(4);
      const q = new URL(fake.requests[3], 'http://x').searchParams;
      expect(q.has('scope')).toBe(false);
      expect(q.has('label')).toBe(false);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(internals(el2).agentWindow.display.length).toBe(25);
    });

    it('a compact flag does not satisfy the reuse branch', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      stateManager.setScope({ type: 'dashboard' });
      stateManager.seedAgents(fake.agents);
      stateManager.seedScopeCapabilities('agent', SCOPE_CAPS);
      stateManager.markAgentSetComplete('compact');
      const el = await mount();
      expect(fake.requests.length).toBe(1);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(internals(el).agentWindow.display.length).toBe(25);
    });

    it('the flag is not set by a paged, labelled, mine or shared load, and none of them clears it', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-scope-agents', 'shared');
      const el = await mount();
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      commitLabel(el, 'env=prod');
      await settle(el);
      internals(el).setScope('all');
      await settle(el);
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      commitLabel(el, '');
      await settle(el);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);

      internals(el).setScope('mine');
      await settle(el);
      commitLabel(el, 'env=prod');
      await settle(el);
      internals(el).setScope('all');
      await settle(el);
      fake.agents = Array.from({ length: 1200 }, (_, i) => makeAgent(i));
      commitLabel(el, '');
      await settle(el);
      expect(internals(el).agentWindow.state).toBe('paged');
      await internals(el).agentWindow.next();
      await settle(el);
      const n = fake.requests.length;
      expect(pager(el).showChip).toBe(false);
      reconnect();
      await settle(el);
      expect(pager(el).showChip).toBe(true);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(fake.requests.length).toBe(n);
    });

    it('a reconnect of the live connection in the held state shows the stale banner, keeps the flag and sends nothing', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-view-agents', 'graph');
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('held');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(internals(el).agentWindow.banner).toBeNull();
      const n = fake.requests.length;
      reconnect();
      await settle(el);
      expect(internals(el).agentWindow.banner?.kind).toBe('stale');
      expect(text(el)).toContain('may be stale');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
      expect(fake.requests.length).toBe(n);
    }, 30_000);

    it('a paged first load does not set the flag; a drain that completes does', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      setView(el, 'graph');
      await settle(el);
      expect(internals(el).agentWindow.state).toBe('held');
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
    });
  });

  describe('complete-needing views', () => {
    it('a mode filter while paged drains once, then filters the held set locally', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      internals(el).setModeFilter('branch');
      await settle(el);
      expect(fake.requests.length).toBe(1 + 3);
      expect(internals(el).agentWindow.state).toBe('held');
      expect(internals(el).agentWindow.total).toBe(600);
      expect(internals(el).agentWindow.items.every((a) => a.messageMode === 'branch')).toBe(true);
      internals(el).setModeFilter('');
      await settle(el);
      expect(fake.requests.length).toBe(4);
    });

    it('a persisted mode filter drains from the first request', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-filter-agents-mode', 'project');
      const el = await mount();
      expect(fake.requests.length).toBe(3);
      expect(new URL(fake.requests[0], 'http://x').searchParams.has('sort')).toBe(false);
      expect(internals(el).agentWindow.total).toBe(600);
    });

    it('a held mine set does not add a live create, marks the set as possibly stale, and issues no request', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-scope-agents', 'mine');
      localStorage.setItem('scion-view-agents', 'graph');
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('held');
      const n = fake.requests.length;
      expect(internals(el).agentWindow.banner).toBeNull();
      handleUpdate('agent.new-1.created', { ...makeAgent(5000), id: 'new-1', agentId: 'new-1' });
      await flushLive(el);
      expect(internals(el).agents.some((a) => a.id === 'new-1')).toBe(false);
      expect(internals(el).agentWindow.banner?.kind).toBe('stale');
      expect(text(el)).toContain('may be stale');
      expect(fake.requests.length).toBe(n);
    });

    for (const scope of ['mine', 'shared'] as const) {
      it(`a live create during an in-flight ${scope} drain is not added and marks the set stale, with no request`, async () => {
        const fake: Fake = {
          agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
          requests: [],
        };
        const inner = fakeFetch(fake);
        let armed = false;
        vi.stubGlobal(
          'fetch',
          vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
            const raw =
              typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
            const u = new URL(raw, 'http://localhost');
            if (armed && isGlobalAgentsList(u) && !u.searchParams.has('sort')) {
              // The first legacy page of the drain: a create lands and is
              // flushed before the page replies.
              armed = false;
              handleUpdate('agent.new-7.created', {
                ...makeAgent(5001),
                id: 'new-7',
                agentId: 'new-7',
              });
              (stateManager as unknown as { flush(): void }).flush();
            }
            return inner(input, init);
          })
        );
        localStorage.setItem('scion-view-agents', 'graph');
        const el = await mount();
        expect(internals(el).agentWindow.state).toBe('held');
        const before = fake.requests.length;
        armed = true;
        internals(el).setScope(scope);
        await settle(el);
        await flushLive(el);
        const members = fake.agents.filter(scope === 'mine' ? isMine : isShared).length;
        expect(fake.requests.length).toBe(before + Math.ceil(members / 500));
        expect(internals(el).agents).toHaveLength(members);
        expect(internals(el).agents.some((a) => a.id === 'new-7')).toBe(false);
        expect(internals(el).agentWindow.banner?.kind).toBe('stale');
        expect(text(el)).toContain('may be stale');
        expect(el.shadowRoot?.querySelector('.agent-window-banner')).not.toBeNull();
      }, 30_000);
    }
  });

  describe('a capped global set', () => {
    it('2,001 agents in the tree view: four requests end capped with the banner, the flag is not set, a lifecycle refresh is free and the banner Refresh drains four again', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 2001 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-view-agents', 'graph');
      const el = await mount();
      const win = internals(el).agentWindow;
      expect(fake.requests.length).toBe(4);
      expect(fake.requests.every((u) => !query(u).has('sort'))).toBe(true);
      expect(win.state).toBe('capped');
      expect(win.incompleteReason).toBe('capped');
      expect(internals(el).agents).toHaveLength(2000);
      const banner = () => el.shadowRoot?.querySelector('.agent-window-banner');
      expect(banner()?.textContent).toContain('2,000 loaded (newest 2,000 checked), more exist');
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
      expect(stateManager.isAgentSetComplete('compact')).toBe(false);

      internals(el).backgroundRefresh('lifecycle-refresh');
      await settle(el);
      expect(fake.requests.length).toBe(4);
      expect(win.state).toBe('capped');

      (banner()?.querySelector('sl-tag') as HTMLElement).click();
      await settle(el);
      expect(fake.requests.length).toBe(8);
      expect(win.state).toBe('capped');
      expect(banner()?.textContent).toContain('2,000 loaded (newest 2,000 checked), more exist');
      expect(stateManager.isAgentSetComplete('full')).toBe(false);
    }, 30_000);
  });

  describe('live membership while paged', () => {
    it('mine paged: a live create raises the chip and issues no request', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 1200 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      localStorage.setItem('scion-scope-agents', 'mine');
      const el = await mount();
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(pager(el).showChip).toBe(false);
      handleUpdate('agent.new-2.created', { ...makeAgent(5002), id: 'new-2', agentId: 'new-2' });
      await flushLive(el);
      expect(pager(el).showChip).toBe(true);
      expect(fake.requests.length).toBe(1);
    });
  });

  describe('the error path', () => {
    it('a failed page load sets the error and Retry loads again', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
        failAll: true,
      };
      stubFake(fake);
      const el = await mount();
      expect(internals(el).error).toBeTruthy();
      expect(text(el)).toContain('Failed to Load Agents');
      fake.failAll = false;
      const retry = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
        b.textContent?.includes('Retry')
      ) as HTMLElement;
      retry.click();
      await settle(el);
      expect(internals(el).error).toBeNull();
      expect(internals(el).agentWindow.display.length).toBe(25);
    });

    it('a label 400 sets the error, as today', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
        badLabels: new Set(['bad=1']),
      };
      stubFake(fake);
      const el = await mount();
      commitLabel(el, 'bad=1');
      await settle(el);
      expect(internals(el).error).toBe('invalid label');
    });

    it('a failed lifecycle refresh keeps the current data', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      fake.failAll = true;
      vi.spyOn(console, 'warn').mockImplementation(() => {});
      internals(el).backgroundRefresh('lifecycle-refresh');
      await settle(el);
      expect(internals(el).error).toBeNull();
      expect(internals(el).agentWindow.display.length).toBe(25);
    });
  });

  describe('empty states', () => {
    const cases: Array<[scope: string, expected: string]> = [
      ['all', 'No Agents Found'],
      ['mine', "You haven't created any agents yet."],
      ['shared', 'No Shared Agents'],
    ];
    for (const [scope, expected] of cases) {
      it(`scope ${scope} with no agents shows "${expected}"`, async () => {
        stubFake({ agents: [], requests: [] });
        if (scope !== 'all') localStorage.setItem('scion-scope-agents', scope);
        const el = await mount();
        expect(text(el)).toContain(expected);
      });
    }

    it('a phase filter matching nothing shows "No Matching Agents", locally and while paged', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      internals(el).setPhaseFilter('stopped');
      await settle(el);
      expect(text(el)).toContain('No Matching Agents');
      unmount(el);

      stateManager.setScope({ type: 'brokers-list' });
      fake.agents = Array.from({ length: 1200 }, (_, i) => makeAgent(i));
      const el2 = await mount();
      expect(internals(el2).agentWindow.state).toBe('paged');
      expect(text(el2)).toContain('No Matching Agents');
    });

    for (const scope of ['mine', 'shared'] as const) {
      it(`scope ${scope}: a phase filter matching nothing shows "No Matching Agents", locally and while paged`, async () => {
        const fake: Fake = {
          agents: Array.from({ length: 25 }, (_, i) => makeAgent(i)),
          requests: [],
        };
        stubFake(fake);
        localStorage.setItem('scion-scope-agents', scope);
        const el = await mount();
        expect(internals(el).agentWindow.state).toBe('small');
        expect(internals(el).agentWindow.stats.total).toBeGreaterThan(0);
        internals(el).setPhaseFilter('stopped');
        await settle(el);
        expect(text(el)).toContain('No Matching Agents');
        expect(text(el)).not.toContain('No Shared Agents');
        expect(text(el)).not.toContain("You haven't created any agents yet.");
        unmount(el);

        stateManager.setScope({ type: 'brokers-list' });
        fake.agents = Array.from({ length: 3000 }, (_, i) => makeAgent(i));
        const el2 = await mount();
        expect(internals(el2).agentWindow.state).toBe('paged');
        expect(query(fake.requests.at(-1)!).get('scope')).toBe(scope);
        expect(query(fake.requests.at(-1)!).get('phase')).toBe('stopped');
        expect(text(el2)).toContain('No Matching Agents');
        expect(text(el2)).not.toContain('No Shared Agents');
        expect(text(el2)).not.toContain("You haven't created any agents yet.");
      }, 30_000);
    }
  });

  describe('count-only mode above 2,000 agents', () => {
    it('shows the counts as of last refresh; a delete, a create and a phase change leave them unchanged and raise the chip; the chip issues one request with stats and updates them; Stop All stays visible', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 2002 }, (_, i) =>
          makeAgent(i, { phase: i < 10 ? 'running' : 'stopped' })
        ),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      const win = internals(el).agentWindow;
      expect(fake.requests.length).toBe(1);
      expect(win.state).toBe('paged');
      expect(win.memberIndex.countOnly).toBe(true);
      expect(text(el)).toContain('2,002 agents · 10 running, as of last refresh');
      expect(pager(el).chipText).toBe('counts may have changed · Refresh');
      expect(hasStopAll(el)).toBe(true);

      const clickChip = async () => {
        pager(el).dispatchEvent(new CustomEvent('chip-click'));
        await settle(el);
      };

      // A delete off the page.
      handleUpdate('agent.g-00005.deleted', {});
      await flushLive(el);
      expect(text(el)).toContain('2,002 agents · 10 running, as of last refresh');
      expect(pager(el).showChip).toBe(true);
      fake.agents = fake.agents.filter((a) => a.id !== 'g-00005');
      await clickChip();
      expect(fake.requests.length).toBe(2);
      expect(new URL(fake.requests[1], 'http://x').searchParams.get('stats')).toBe('1');
      expect(text(el)).toContain('2,001 agents · 9 running, as of last refresh');
      expect(pager(el).showChip).toBe(false);

      // A create.
      const created = makeAgent(9000, { phase: 'running' });
      handleUpdate(`agent.${created.id}.created`, { ...created, agentId: created.id });
      await flushLive(el);
      expect(text(el)).toContain('2,001 agents · 9 running, as of last refresh');
      expect(pager(el).showChip).toBe(true);
      fake.agents = [...fake.agents, created];
      await clickChip();
      expect(fake.requests.length).toBe(3);
      expect(text(el)).toContain('2,002 agents · 10 running, as of last refresh');

      // A phase change off the page.
      handleUpdate('agent.g-00001.status', { agentId: 'g-00001', phase: 'stopped' });
      await flushLive(el);
      expect(text(el)).toContain('2,002 agents · 10 running, as of last refresh');
      expect(pager(el).showChip).toBe(true);
      expect(hasStopAll(el)).toBe(true);
    });

    it('Stop All is hidden when the snapshot has no running agent', async () => {
      stubFake({
        agents: Array.from({ length: 2001 }, (_, i) => makeAgent(i, { phase: 'stopped' })),
        requests: [],
      });
      const el = await mount();
      expect(internals(el).agentWindow.memberIndex.countOnly).toBe(true);
      expect(hasStopAll(el)).toBe(false);
    });
  });

  describe('request counts per interaction at 25, 500 and 1,200 agents', () => {
    /**
     * A mixed fixture: every third agent stopped, alternating env=prod and
     * env=dev, and every fifth agent also carries a bare `team` key, so the
     * phase and label steps really narrow the set.
     */
    const mixedAgent = (i: number): Agent =>
      makeAgent(i, {
        phase: i % 3 === 0 ? 'stopped' : 'running',
        labels: { env: i % 2 === 0 ? 'prod' : 'dev', ...(i % 5 === 0 ? { team: 'core' } : {}) },
      });
    const byUpdated = (dir: 'asc' | 'desc') => (a: Agent, b: Agent) =>
      (dir === 'asc' ? 1 : -1) * (a.updated ?? '').localeCompare(b.updated ?? '');

    /** After a step: the window's total and the rendered rows match the fixture under this filter and dir. */
    const expectRows =
      (keep: (a: Agent) => boolean, dir: 'asc' | 'desc') =>
      (el: TestEl, fake: Fake): void => {
        const expected = fake.agents.filter(keep).sort(byUpdated(dir));
        const win = internals(el).agentWindow;
        expect(win.total).toBe(expected.length);
        const page = expected.slice(0, 25).map((a) => a.id);
        expect(win.items.map((a) => a.id)).toEqual(page);
        expect(el.shadowRoot?.querySelectorAll('tbody tr').length).toBe(page.length);
      };

    type Step = [
      label: string,
      run: (el: TestEl) => void | Promise<void>,
      check?: (el: TestEl, fake: Fake) => void,
    ];
    const steps: Step[] = [
      ['grid', (el) => setView(el, 'grid')],
      ['list', (el) => setView(el, 'list')],
      ['dir flip', (el) => internals(el).toggleSort('updated')],
      [
        'phase',
        (el) => internals(el).setPhaseFilter('running'),
        expectRows((a) => a.phase === 'running', 'asc'),
      ],
      ['phase clear', (el) => internals(el).setPhaseFilter('')],
      ['next page', (el) => internals(el).agentWindow.next()],
      ['prev page', (el) => internals(el).agentWindow.prev()],
      ['label typing', (el) => typeLabel(el, 'env=pr')],
      [
        'label commit',
        (el) => commitLabel(el, 'env=prod'),
        expectRows((a) => a.labels?.env === 'prod', 'asc'),
      ],
      ['lifecycle refresh', (el) => internals(el).backgroundRefresh('lifecycle-refresh')],
      ['label clear', (el) => commitLabel(el, '')],
      [
        'bare-key label commit',
        (el) => commitLabel(el, 'team'),
        expectRows((a) => !!a.labels && 'team' in a.labels, 'asc'),
      ],
      ['bare-key label clear', (el) => commitLabel(el, '')],
      ['scope mine', (el) => internals(el).setScope('mine')],
      ['scope all', (el) => internals(el).setScope('all')],
      ['tree', (el) => setView(el, 'graph')],
      ['list again', (el) => setView(el, 'list')],
      ['mode filter', (el) => internals(el).setModeFilter('branch')],
      ['mode clear', (el) => internals(el).setModeFilter('')],
      ['name sort', (el) => internals(el).toggleSort('name')],
      ['updated sort', (el) => internals(el).toggleSort('updated')],
      ['phase again', (el) => internals(el).setPhaseFilter('running')],
      ['next page again', (el) => internals(el).agentWindow.next()],
      ['lifecycle refresh again', (el) => internals(el).backgroundRefresh('lifecycle-refresh')],
    ];

    // A bare-key label is complete-needing: it drains (one legacy request
    // at 500 or fewer agents, three at 1,200), and clearing it sends one
    // fit request.
    const small = {
      // prettier-ignore
      costs: [0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1],
      states: steps.map(() => 'small'),
    };
    const cases: Array<{ count: number; costs: number[]; states: string[] }> = [
      { count: 25, ...small },
      { count: 500, ...small },
      {
        count: 1200,
        // prettier-ignore
        costs:  [0, 0, 1, 1, 1, 1, 1, 0, 1, 1, 1, 3, 1, 1, 1, 3, 0, 0, 0, 0, 0, 0, 0, 0],
        // prettier-ignore
        states: ['paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged',
          'paged', 'paged', 'held', 'paged', 'paged', 'paged', 'held', 'held', 'held', 'held',
          'held', 'held', 'held', 'held', 'held'],
      },
    ];

    for (const c of cases) {
      it(`${c.count} agents: one request on page load, then the documented cost per interaction`, async () => {
        const fake: Fake = {
          agents: Array.from({ length: c.count }, (_, i) => mixedAgent(i)),
          requests: [],
        };
        stubFake(fake);
        const el = await mount();
        expect(fake.requests.length).toBe(1);
        expect(internals(el).agentWindow.state).toBe(c.count > 500 ? 'paged' : 'small');

        const costs: number[] = [];
        const states: string[] = [];
        for (const [name, run, check] of steps) {
          const n = fake.requests.length;
          await run(el);
          await settle(el);
          costs.push(fake.requests.length - n);
          states.push(internals(el).agentWindow.state);
          if (check) {
            try {
              check(el, fake);
            } catch (err) {
              throw new Error(`after "${name}": ${(err as Error).message}`);
            }
          }
        }
        const named = (xs: Array<number | string>) => steps.map(([name], i) => `${name}: ${xs[i]}`);
        expect(named(costs)).toEqual(named(c.costs));
        expect(named(states)).toEqual(named(c.states));

        // Client navigation back to /agents: the flag is held (small or a
        // completed drain), so the reuse branch issues no request. The
        // persisted running filter still applies.
        unmount(el);
        const n = fake.requests.length;
        const el2 = await mount();
        expect(fake.requests.length - n).toBe(0);
        expect(internals(el2).agentWindow.total).toBe(
          fake.agents.filter((a) => a.phase === 'running').length
        );
      }, 60_000);
    }

    it('2,001 agents: the counts chip is one paged request with stats', async () => {
      const fake: Fake = {
        agents: Array.from({ length: 2001 }, (_, i) => makeAgent(i)),
        requests: [],
      };
      stubFake(fake);
      const el = await mount();
      handleUpdate('agent.g-00003.deleted', {});
      await flushLive(el);
      expect(pager(el).showChip).toBe(true);
      const n = fake.requests.length;
      pager(el).dispatchEvent(new CustomEvent('chip-click'));
      await settle(el);
      expect(fake.requests.length - n).toBe(1);
      const q = new URL(fake.requests.at(-1)!, 'http://x').searchParams;
      expect(q.get('stats')).toBe('1');
      expect(q.get('limit')).toBe('25');
      expect(q.has('fit')).toBe(false);
    });
  });
});
