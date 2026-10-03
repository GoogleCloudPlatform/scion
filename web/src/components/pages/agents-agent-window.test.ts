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
import {
  FakeEventSource,
  SCOPE_CAPS,
  fakeFetch,
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

async function settle(el: TestEl): Promise<void> {
  await vi.waitFor(() => {
    expect(internals(el).loading).toBe(false);
    expect(internals(el).agentsLoading).toBe(false);
  });
  await new Promise((r) => setTimeout(r, 20));
  await el.updateComplete;
}

async function mount(): Promise<TestEl> {
  const el = document.createElement('scion-page-agents') as TestEl;
  (el as unknown as { pageData: unknown }).pageData = {
    path: '/agents',
    title: 'Agents',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await settle(el);
  return el;
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

/** state.ts flushes on rAF or after 100 ms without one. */
async function flushLive(el: TestEl): Promise<void> {
  await new Promise((r) => setTimeout(r, 150));
  await el.updateComplete;
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

    it('a legacy server (no sorted mode) is complete with no cursor, and is drained otherwise', async () => {
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
      expect(fake.requests.length).toBe(1 + 3);
      expect(internals(el).agentWindow.state).toBe('held');
      expect(internals(el).agents.length).toBe(1200);
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
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
      stateManager.dispatchEvent(new CustomEvent('agents-resync'));
      expect(stateManager.isAgentSetComplete('full')).toBe(true);
    });

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

    it('a drained mine set does not add a live create and issues no request', async () => {
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
      handleUpdate('agent.new-1.created', { ...makeAgent(5000), id: 'new-1', agentId: 'new-1' });
      await flushLive(el);
      expect(internals(el).agents.some((a) => a.id === 'new-1')).toBe(false);
      expect(fake.requests.length).toBe(n);
    });
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
    type Step = [label: string, run: (el: TestEl) => void | Promise<void>];
    const steps: Step[] = [
      ['grid', (el) => setView(el, 'grid')],
      ['list', (el) => setView(el, 'list')],
      ['dir flip', (el) => internals(el).toggleSort('updated')],
      ['phase', (el) => internals(el).setPhaseFilter('running')],
      ['phase clear', (el) => internals(el).setPhaseFilter('')],
      ['next page', (el) => internals(el).agentWindow.next()],
      ['prev page', (el) => internals(el).agentWindow.prev()],
      ['label typing', (el) => typeLabel(el, 'env=pr')],
      ['label commit', (el) => commitLabel(el, 'env=prod')],
      ['lifecycle refresh', (el) => internals(el).backgroundRefresh('lifecycle-refresh')],
      ['label clear', (el) => commitLabel(el, '')],
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

    const small = {
      // prettier-ignore
      costs: [0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1],
      states: steps.map(() => 'small'),
    };
    const cases: Array<{ count: number; costs: number[]; states: string[] }> = [
      { count: 25, ...small },
      { count: 500, ...small },
      {
        count: 1200,
        // prettier-ignore
        costs:  [0, 0, 1, 1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 3, 0, 0, 0, 0, 0, 0, 0, 0],
        // prettier-ignore
        states: ['paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged',
          'paged', 'paged', 'paged', 'paged', 'held', 'held', 'held', 'held', 'held', 'held',
          'held', 'held', 'held'],
      },
    ];

    for (const c of cases) {
      it(`${c.count} agents: one request on page load, then the documented cost per interaction`, async () => {
        const fake: Fake = {
          agents: Array.from({ length: c.count }, (_, i) => makeAgent(i)),
          requests: [],
        };
        stubFake(fake);
        const el = await mount();
        expect(fake.requests.length).toBe(1);
        expect(internals(el).agentWindow.state).toBe(c.count > 500 ? 'paged' : 'small');

        const costs: number[] = [];
        const states: string[] = [];
        for (const [, run] of steps) {
          const n = fake.requests.length;
          await run(el);
          await settle(el);
          costs.push(fake.requests.length - n);
          states.push(internals(el).agentWindow.state);
        }
        const named = (xs: Array<number | string>) => steps.map(([name], i) => `${name}: ${xs[i]}`);
        expect(named(costs)).toEqual(named(c.costs));
        expect(named(states)).toEqual(named(c.states));

        // Client navigation back to /agents: the flag is held (small or a
        // completed drain), so the reuse branch issues no request.
        unmount(el);
        const n = fake.requests.length;
        const el2 = await mount();
        expect(fake.requests.length - n).toBe(0);
        expect(internals(el2).agentWindow.total).toBe(c.count);
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
