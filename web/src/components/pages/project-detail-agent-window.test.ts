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
 * Covers the small and paged window states (design §9): off-page upsert,
 * reconnect chip with no request, label typing/commit, a label 400 keeping
 * previous data, lifecycle refresh issuing exactly one request, and the 422
 * fallback with its per-label memory. Small-state display being identical
 * to the pre-change `displayAgents` is covered via `agent-sort.test.ts`'s
 * parity checks and the list/grid-identity assertions below.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, Capabilities, PageData } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import { stateManager } from '../../client/state.js';
import { PROJECT_AGENTS_FIT_THRESHOLD } from '../../client/agent-list-window.js';

// Phase 2 (ptone/scion#2483): the shared delete helper, spied but running
// for real (pass-through), so tests can prove the page delegates to it; the
// confirm dialog and toasts are stubbed (they need real Shoelace elements).
vi.mock('../../client/agent-delete.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/agent-delete.js')>();
  return { ...actual, runAgentDelete: vi.fn(actual.runAgentDelete) };
});
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));
import { runAgentDelete } from '../../client/agent-delete.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { showToast } from '../../utils/toast.js';
import { START_BLOCKED_BY_DELETE_MESSAGE } from '../../shared/agent-deletion.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * Re-stubs fetch so a sorted agents GET answers with a body that has no
 * `agents` field; every other request still reaches `inner`.
 */
function stubSortedAgentsWithoutList(projectId: string, inner: typeof fetch): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const rawUrl =
        typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const u = new URL(rawUrl, 'http://localhost');
      if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
        return Promise.resolve(jsonResponse({ totalCount: 0, complete: true }));
      }
      return inner(input, init);
    })
  );
}

function makeAgent(i: number, overrides: Partial<Agent> = {}): Agent {
  return {
    id: `a-${i}`,
    name: `agent-${i}`,
    projectId: 'p-win',
    template: 't',
    phase: 'running',
    created: `2026-01-01T00:00:${String(i % 60).padStart(2, '0')}Z`,
    updated: `2026-01-02T00:00:${String(i % 60).padStart(2, '0')}Z`,
    messageMode: 'project',
    _capabilities: { actions: ['read', 'update', 'delete', 'stop_all'] },
    ...overrides,
  } as Agent;
}

interface AgentsRequest {
  url: string;
}

/** A minimal fake project-agents endpoint implementing enough of the design §4 contract for these tests. */
function createFetchHandler(opts: {
  projectId: string;
  projectCaps: Capabilities;
  agents: Agent[];
  requests: AgentsRequest[];
  refuseSortedForLabel?: string;
}) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const rawUrl =
      typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const u = new URL(rawUrl, 'http://localhost');
    const path = u.pathname;
    const method = (
      init?.method ?? (input instanceof Request ? input.method : 'GET')
    ).toUpperCase();

    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(jsonResponse({ projects: [] }));
    }

    if (path === `/api/v1/projects/${opts.projectId}/agents` && method === 'GET') {
      opts.requests.push({ url: rawUrl });
      const sort = u.searchParams.get('sort');
      const label = u.searchParams.get('label') ?? '';

      if (sort) {
        if (opts.refuseSortedForLabel !== undefined && label === opts.refuseSortedForLabel) {
          return Promise.resolve(
            jsonResponse(
              {
                error: {
                  code: 'sorted_view_unavailable',
                  message: 'too many agents for a sorted view',
                  details: { reason: 'too_many_candidates' },
                },
              },
              422
            )
          );
        }
        let list = opts.agents;
        if (label.includes('=')) {
          const [k, v] = label.split('=');
          list = list.filter((a) => a.labels?.[k] === v);
        }
        const dir = u.searchParams.get('dir') === 'asc' ? 1 : -1;
        const sorted = [...list].sort(
          (a, b) => dir * (a.updated ?? '').localeCompare(b.updated ?? '')
        );
        return Promise.resolve(
          jsonResponse({
            agents: sorted,
            totalCount: sorted.length,
            complete: true,
            stats: {
              total: sorted.length,
              running: sorted.filter((a) => a.phase === 'running').length,
              agents: sorted.map((a) => [a.id, a.phase]),
            },
            _capabilities: opts.projectCaps,
          })
        );
      }

      // Legacy mode.
      let list = opts.agents;
      if (label.includes('=')) {
        const [k, v] = label.split('=');
        list = list.filter((a) => a.labels?.[k] === v);
      }
      return Promise.resolve(jsonResponse({ agents: list, _capabilities: opts.projectCaps }));
    }

    if (path === `/api/v1/projects/${opts.projectId}` && method === 'GET') {
      return Promise.resolve(
        jsonResponse({
          id: opts.projectId,
          name: 'Window Project',
          slug: 'window-project',
          _capabilities: opts.projectCaps,
        })
      );
    }

    if (/^\/api\/v1\/agents\/[^/]+\/(start|stop|suspend)$/.test(path) && method === 'POST') {
      return Promise.resolve(jsonResponse({}));
    }

    if (path === `/api/v1/projects/${opts.projectId}/agents/stop-all` && method === 'POST') {
      return Promise.resolve(jsonResponse({ stopped: 0, failed: 0 }));
    }

    return Promise.resolve(jsonResponse({}, 404));
  };
}

type TestEl = HTMLElement & {
  updateComplete: Promise<boolean>;
  pageData: PageData | null;
  projectId: string;
};

async function createComponent(projectId: string): Promise<TestEl> {
  const el = document.createElement('scion-page-project-detail') as TestEl;
  el.projectId = projectId;
  el.pageData = {
    path: `/projects/${projectId}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function labelInput(el: TestEl): (HTMLElement & { value: string }) | null {
  const inputs = Array.from(el.shadowRoot?.querySelectorAll('sl-input') ?? []);
  return (inputs.find((i) => i.getAttribute('placeholder') === 'Filter by label (key=value)') ??
    null) as (HTMLElement & { value: string }) | null;
}

function viewToggle(el: TestEl): HTMLElement | null {
  return el.shadowRoot?.querySelector('scion-view-toggle') ?? null;
}

interface Internals {
  toggleSort(field: string): void;
  setPhaseFilter(phase: string): void;
  handleAgentAction(id: string, action: string): Promise<void>;
  backgroundRefresh(trigger: string): void;
  onPagerSizeChange(size: number): void;
  committedLabel: string;
  pagerPageSize: number;
  agents: Agent[];
  agentWindow: {
    next(): Promise<void>;
    prev(): Promise<void>;
    refresh(): Promise<void>;
    state: 'small' | 'paged';
    items: Agent[];
    pageIndex: number;
    updatesAvailable: boolean;
    error: string | null;
  };
  agentStats: { total: number; running: number };
  listViewUsesWindow: boolean;
}

function internals(el: TestEl): Internals {
  return el as unknown as Internals;
}

function pager(el: TestEl): (HTMLElement & { pageSize: number }) | null {
  return el.shadowRoot?.querySelector('scion-agent-pager') as
    | (HTMLElement & { pageSize: number })
    | null;
}

/** A deferred promise, for controlling fetch resolution order explicitly. */
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/**
 * A more realistic sorted-mode handler than `createFetchHandler`'s:
 * `complete` is decided by `agents.length` vs
 * `fit`, cursor pages are real continuations of the same sorted list, and
 * `legacyTruncated` simulates a >500-candidate legacy response without
 * needing a 501-agent fixture.
 */
function createRealisticFetchHandler(opts: {
  projectId: string;
  projectCaps: Capabilities;
  agents: Agent[];
  requests: AgentsRequest[];
  legacyTruncated?: boolean;
}) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const rawUrl =
      typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const u = new URL(rawUrl, 'http://localhost');
    const path = u.pathname;
    const method = (
      init?.method ?? (input instanceof Request ? input.method : 'GET')
    ).toUpperCase();

    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(jsonResponse({ projects: [] }));
    }

    if (path === `/api/v1/projects/${opts.projectId}` && method === 'GET') {
      return Promise.resolve(
        jsonResponse({
          id: opts.projectId,
          name: 'Window Project',
          slug: 'window-project',
          _capabilities: opts.projectCaps,
        })
      );
    }

    if (/^\/api\/v1\/agents\/[^/]+\/(start|stop|suspend)$/.test(path) && method === 'POST') {
      return Promise.resolve(jsonResponse({}));
    }
    if (path === `/api/v1/projects/${opts.projectId}/agents/stop-all` && method === 'POST') {
      return Promise.resolve(jsonResponse({ stopped: 0, failed: 0 }));
    }

    if (path === `/api/v1/projects/${opts.projectId}/agents` && method === 'GET') {
      opts.requests.push({ url: rawUrl });
      const sort = u.searchParams.get('sort');
      const label = u.searchParams.get('label') ?? '';
      const phase = u.searchParams.get('phase') ?? '';
      let list = opts.agents;
      if (label.includes('=')) {
        const eq = label.indexOf('=');
        const k = label.slice(0, eq);
        const v = label.slice(eq + 1);
        list = list.filter((a) => a.labels?.[k] === v);
      }

      if (sort) {
        const dir = u.searchParams.get('dir') === 'asc' ? 1 : -1;
        const sorted = [...list].sort(
          (a, b) => dir * (a.updated ?? '').localeCompare(b.updated ?? '')
        );
        const cursor = u.searchParams.get('cursor');
        const limit = Number(u.searchParams.get('limit') ?? '25');
        const statsOf = (set: Agent[]) => ({
          total: set.length,
          running: set.filter((a) => a.phase === 'running').length,
          agents: set.map((a) => [a.id, a.phase]) as Array<[string, string]>,
        });

        // Paged (incomplete) responses are phase-filtered server-side; a
        // complete response is always the whole unphased set (design §4.3).
        const phased = phase ? sorted.filter((a) => a.phase === phase) : sorted;

        if (cursor !== null) {
          const startIdx = Number(cursor);
          const page = phased.slice(startIdx, startIdx + limit);
          const nextIdx = startIdx + limit;
          return Promise.resolve(
            jsonResponse({
              agents: page,
              totalCount: phased.length,
              complete: false,
              nextCursor: nextIdx < phased.length ? String(nextIdx) : undefined,
              stats: u.searchParams.get('stats') ? statsOf(sorted) : undefined,
            })
          );
        }

        // The server only reports a complete set when fit was sent; a sorted
        // request without fit (the window's own page fetches) is always a page.
        const fitParam = u.searchParams.get('fit');
        if (fitParam !== null && sorted.length <= Number(fitParam)) {
          return Promise.resolve(
            jsonResponse({
              agents: sorted,
              totalCount: sorted.length,
              complete: true,
              stats: statsOf(sorted),
              _capabilities: opts.projectCaps,
            })
          );
        }
        const page = phased.slice(0, limit);
        return Promise.resolve(
          jsonResponse({
            agents: page,
            totalCount: phased.length,
            complete: false,
            nextCursor: limit < phased.length ? String(limit) : undefined,
            stats: statsOf(sorted),
          })
        );
      }

      // Legacy mode.
      if (opts.legacyTruncated) {
        return Promise.resolve(
          jsonResponse({
            agents: list.slice(0, 20),
            nextCursor: 'truncated',
            _capabilities: opts.projectCaps,
          })
        );
      }
      return Promise.resolve(jsonResponse({ agents: list, _capabilities: opts.projectCaps }));
    }

    return Promise.resolve(jsonResponse({}, 404));
  };
}

describe('project-detail — agent list window', () => {
  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
    // Mounting a 100-agent grid/list page is slow under the default 5s
    // per-test timeout on a loaded machine; these tests do real work
    // (fetch handling, multiple Lit render passes) rather than looping.
    vi.setConfig({ testTimeout: 20_000 });
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-project-detail').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  describe('sorted/paged mode — list view, updated sort, at the fit threshold', () => {
    it('page load issues exactly one agents request (the fit request); every client-only interaction issues zero; label commit and lifecycle refresh issue exactly one each', async () => {
      const projectId = 'p-w10-list';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: PROJECT_AGENTS_FIT_THRESHOLD }, (_, i) =>
        makeAgent(i, { projectId })
      );
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(requests.length).toBe(1);
      expect(requests[0].url).toContain('sort=updated');
      expect(requests[0].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(internals(el).agentWindow.state).toBe('small');

      const toggle = viewToggle(el)!;

      // list -> grid -> list: 0 requests (complete set already held).
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'grid' } }));
      await el.updateComplete;
      expect(requests.length).toBe(1);
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await el.updateComplete;
      expect(requests.length).toBe(1);

      // list -> tree ('graph' view mode): 0 requests.
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'graph' } }));
      await el.updateComplete;
      expect(requests.length).toBe(1);
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await el.updateComplete;
      expect(requests.length).toBe(1);

      // Sort dir flip, sort -> name, phase change: 0 requests (small state is local).
      internals(el).toggleSort('updated'); // flips dir
      expect(requests.length).toBe(1);
      internals(el).toggleSort('name');
      expect(requests.length).toBe(1);
      internals(el).toggleSort('updated'); // back to updated for the page-nav check below
      internals(el).setPhaseFilter('running');
      internals(el).setPhaseFilter('');
      expect(requests.length).toBe(1);

      // Page navigation: 0 requests (local slice of the held set).
      await internals(el).agentWindow.next();
      await internals(el).agentWindow.prev();
      expect(requests.length).toBe(1);

      // Label typing: 0 requests per keystroke.
      const input = labelInput(el)!;
      input.value = 'e';
      input.dispatchEvent(new Event('sl-input'));
      input.value = 'env';
      input.dispatchEvent(new Event('sl-input'));
      expect(requests.length).toBe(1);

      // Label commit (sl-change): exactly one request.
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length).toBe(2);

      // Lifecycle refresh: exactly one request.
      await internals(el).handleAgentAction('a-1', 'stop');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length).toBe(3);
    }, 20_000);

    it('the same page opened in grid view issues exactly one legacy agents request, and grid -> list issues zero', async () => {
      // Grid always uses the legacy load, which returns the complete set at
      // any size up to 500, so this holds above the fit threshold too.
      const projectId = 'p-w10-grid';
      localStorage.setItem('scion-view-project-agents', 'grid');
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(requests.length).toBe(1);
      expect(requests[0].url).not.toContain('sort=');

      const toggle = viewToggle(el)!;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await el.updateComplete;
      expect(requests.length).toBe(1);
    });
  });

  describe('above the fit threshold the list view pages from the first request', () => {
    const mountList = async (projectId: string, count: number, pageSize?: number) => {
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      if (pageSize !== undefined) {
        localStorage.setItem('scion-pagesize-project-agents', String(pageSize));
      }
      // Every agent carries env=prod, so committing that label keeps the
      // candidate set above the threshold and the window stays paged.
      const agents = Array.from({ length: count }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: 'prod' } })
      );
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })
        )
      );
      const el = await createComponent(projectId);
      return { el, requests };
    };

    it('one agent above the threshold: the first request carries the threshold as fit and the window is paged', async () => {
      const { el, requests } = await mountList('p-fit-boundary', PROJECT_AGENTS_FIT_THRESHOLD + 1);
      expect(requests.length).toBe(1);
      expect(requests[0].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(requests[0].url).toContain('limit=25');
      expect(internals(el).agentWindow.state).toBe('paged');
    });

    it('100 agents: one request on load; each paged interaction costs exactly one; list -> grid is one legacy load that completes the set, after which toggles are free', async () => {
      const { el, requests } = await mountList('p-fit-100', 100);
      expect(requests.length).toBe(1);
      expect(requests[0].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(internals(el).agentWindow.state).toBe('paged');

      const settle = () => new Promise((r) => setTimeout(r, 10));
      let n = requests.length;
      await internals(el).agentWindow.next();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('cursor=');

      n = requests.length;
      await internals(el).agentWindow.prev();
      expect(requests.length - n).toBe(1);

      n = requests.length;
      internals(el).toggleSort('updated'); // flips dir
      await settle();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('dir=asc');
      expect(internals(el).agentWindow.state).toBe('paged');

      n = requests.length;
      internals(el).setPhaseFilter('running');
      await settle();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('phase=running');
      n = requests.length;
      internals(el).setPhaseFilter('');
      await settle();
      expect(requests.length - n).toBe(1);
      expect(internals(el).agentWindow.state).toBe('paged');

      // Label typing is free; the commit is one request.
      const input = labelInput(el)!;
      n = requests.length;
      input.value = 'env';
      input.dispatchEvent(new Event('sl-input'));
      expect(requests.length - n).toBe(0);
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('label=env%3Dprod');
      expect(internals(el).agentWindow.state).toBe('paged');

      n = requests.length;
      await internals(el).handleAgentAction('a-1', 'stop');
      await settle();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(internals(el).agentWindow.state).toBe('paged');

      // Clear the label so the grid load is unfiltered.
      n = requests.length;
      input.value = '';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle();
      expect(requests.length - n).toBe(1);
      expect(internals(el).agentWindow.state).toBe('paged');

      const toggle = viewToggle(el)!;
      n = requests.length;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'grid' } }));
      await settle();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).not.toContain('sort=');

      n = requests.length;
      for (const v of ['list', 'grid', 'list']) {
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await settle();
      }
      expect(requests.length - n).toBe(0);
      expect(internals(el).agentWindow.state).toBe('small');
    }, 20_000);

    it('page size 100: fit is raised to the page size, so 100 agents is small and 101 is paged', async () => {
      const small = await mountList('p-fit-ps100-small', 100, 100);
      expect(small.requests.length).toBe(1);
      expect(small.requests[0].url).toContain('fit=100');
      expect(small.requests[0].url).toContain('limit=100');
      expect(internals(small.el).agentWindow.state).toBe('small');
      small.el.remove();
      localStorage.clear();

      const paged = await mountList('p-fit-ps100-paged', 101, 100);
      expect(paged.requests.length).toBe(1);
      expect(paged.requests[0].url).toContain('fit=100');
      expect(internals(paged.el).agentWindow.state).toBe('paged');
    });

    it('a page-size change while paged sends fit raised to the new page size, never below limit', async () => {
      const { el, requests } = await mountList('p-fit-ps-change', 100);
      expect(internals(el).agentWindow.state).toBe('paged');

      const n = requests.length;
      internals(el).onPagerSizeChange(100);
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1);
      const url = requests[requests.length - 1].url;
      expect(url).toContain('limit=100');
      expect(url).toContain('fit=100');
      expect(internals(el).agentWindow.state).toBe('small');
    });
  });

  describe('422 fallback and per-label memory', () => {
    it('a 422 falls back to the legacy load and is remembered for that label; a new label retries once', async () => {
      const projectId = 'p-422';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 10 }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: 'prod' } })
      );
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            refuseSortedForLabel: '', // the initial unlabelled load is refused
          })
        )
      );

      const el = await createComponent(projectId);
      // Page load: sorted request refused (422), falls back to the legacy load.
      expect(requests.length).toBe(2);
      expect(requests[0].url).toContain('sort=updated');
      expect(requests[1].url).not.toContain('sort=');

      // A lifecycle refresh with the SAME (still refused) label does not retry sorted mode.
      await internals(el).handleAgentAction('a-1', 'stop');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length).toBe(3);
      expect(requests[2].url).not.toContain('sort=');

      // A new label commit retries sorted mode once.
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length).toBe(4);
      expect(requests[3].url).toContain('sort=updated');
      expect(requests[3].url).toContain('label=env%3Dprod');
    });
  });

  describe('label 400 keeps previous data', () => {
    it('a non-OK label-commit response keeps the previously loaded agents', async () => {
      const projectId = 'p-label-400';
      localStorage.setItem('scion-view-project-agents', 'grid'); // not sorted-eligible: exercises the legacy path directly
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      let failNext = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          if (rawUrl.includes(`/api/v1/projects/${projectId}/agents?label=`) && failNext) {
            requests.push({ url: rawUrl });
            return Promise.resolve(jsonResponse({ error: { message: 'bad label' } }, 400));
          }
          return createFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(input, init);
        })
      );

      const el = await createComponent(projectId);
      const before = (el as unknown as { agents: Agent[] }).agents;
      expect(before.length).toBe(5);

      failNext = true;
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));

      const after = (el as unknown as { agents: Agent[] }).agents;
      expect(after.length).toBe(5); // previous data kept, not cleared to []
    });
  });

  describe('paged state: live updates (design §6.2 table), and reconnect', () => {
    /** 30 agents, forced paged (via `legacyTruncated` plus a `fit` below the count — see individual tests), 25/page: page 0 holds the 25 highest `updated`, page 1 holds the rest. */
    function mountForcedPaged(
      projectId: string,
      agents: Agent[],
      requests: AgentsRequest[]
    ): Promise<TestEl> {
      localStorage.setItem('scion-view-project-agents', 'list');
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            // `fit=0` forces the candidate count to always exceed it, so the
            // first response is paged regardless of how small the fixture is.
            u.searchParams.set('fit', '0');
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );
      return createComponent(projectId);
    }

    it('while paged, an SSE create and status for a project agent go through the window only: this.agents stays empty', async () => {
      const projectId = 'p-paged-gate';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect((el as unknown as { agents: Agent[] }).agents.length).toBe(0);
      expect(internals(el).agentStats.total).toBe(5);

      // A brand-new SSE-created agent for this project.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: 'a-gate',
          id: 'a-gate',
          name: 'a-gate',
          projectId,
          template: 't',
          phase: 'running',
          created: '2026-03-01T00:00:00Z',
          updated: '2026-03-01T00:00:00Z',
          messageMode: 'project',
        },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // A status delta for an already-known project agent too.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: agents[0].id, phase: 'stopped' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // `mergeAgentsChanged` (the small/held-state merge) must never run
      // while paged — the window's own `applyChanges` is the only path.
      // If the gate were skipped, `this.agents` would have picked up the
      // new agent here instead of staying at its paged-state empty value.
      expect((el as unknown as { agents: Agent[] }).agents.length).toBe(0);
      // `agentStats` comes from the member index (paged state), not from
      // `this.agents`: 5 original members plus the new create.
      expect(internals(el).agentStats.total).toBe(6);
    });

    it('an off-page phase change with no phase filter is counts-only — stats update live, no chip, no request', async () => {
      const projectId = 'p-paged-counts';
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');

      await internals(el).agentWindow.next(); // page 1
      await el.updateComplete;
      const before = requests.length;
      expect(internals(el).agentStats.running).toBe(30);

      // agents[29] (highest `updated`, on page 0) goes from running to stopped.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: agents[29].id, phase: 'stopped' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // A pure phase change off-page, with no active phase filter, does not
      // change which agent belongs on which page — it only affects the
      // live "Running" count, which the design table says shows no chip for.
      expect(internals(el).agentStats.running).toBe(29);
      expect(internals(el).agentWindow.updatesAvailable).toBe(false);
      expect(requests.length).toBe(before);
    });

    it("a paged refetch's stats do not resurrect an agent already removed by an SSE delete", async () => {
      const projectId = 'p-paged-stats-tombstone';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentStats.total).toBe(5);

      // The hub tells this client one member is gone.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: agents[0].id },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;
      expect(internals(el).agentStats.total).toBe(4);

      // A view-state change (sort direction flip) triggers a fresh
      // `loadAgentsForView` request, which always asks for `stats=1`. The
      // fixture's fetch handler still returns `stats.agents` built from the
      // full, static fixture list — it has no knowledge of the delete,
      // exactly like a REST response that was already in flight, or served
      // from a stale read replica, when the delete happened.
      internals(el).toggleSort('updated');
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      // The already-removed agent must not be re-counted.
      expect(internals(el).agentStats.total).toBe(4);
    });

    it('a paged refetch via a view-state change drops an agent already removed by an SSE delete', async () => {
      // Same race as the stats-only test above, through `loadAgentsForView`'s
      // own paged branch (the response's `agents` field, not just `stats`).
      const projectId = 'p-paged-fit-tombstone';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.items.some((a) => a.id === agents[0].id)).toBe(true);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: agents[0].id },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // A view-state change (sort direction flip) issues a fresh
      // `loadAgentsForView` request, landing in the paged branch again
      // (`fit` is forced to 0). The fixture still lists the already-deleted
      // agent.
      internals(el).toggleSort('updated');
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      expect(internals(el).agentWindow.items.some((a) => a.id === agents[0].id)).toBe(false);
    });

    it("the window's own page fetcher drops an agent already removed by an SSE delete", async () => {
      // Same race as the two tests above, but through the window's own
      // per-page fetcher (`fetchAgentsPage`, used by `next`/`prev`/
      // `refresh`) rather than `loadAgentsForView`'s own request.
      const projectId = 'p-paged-fetcher-tombstone';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.items.some((a) => a.id === agents[0].id)).toBe(true);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: agents[0].id },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // The chip-click refresh re-fetches page 0 through `fetchAgentsPage`;
      // the fixture still lists the already-deleted agent.
      await internals(el).agentWindow.refresh();
      await el.updateComplete;

      expect(internals(el).agentWindow.items.some((a) => a.id === agents[0].id)).toBe(false);
      // Page 0 also carries `stats` (`wantStats` is true at index 0), so
      // this also pins `fetchAgentsPage`'s own `freshStats` call, not just
      // its `agents` field.
      expect(internals(el).agentStats.total).toBe(4);
    });

    it("the window's own page fetcher tolerates a response with no agents field", async () => {
      // With a tombstone recorded, dropTombstoned iterates its input, so a
      // missing `agents` field must fall back to an empty list, not throw.
      const projectId = 'p-paged-fetcher-missing-agents';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: agents[0].id },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      stubSortedAgentsWithoutList(projectId, globalThis.fetch);
      await internals(el).agentWindow.refresh();
      await el.updateComplete;

      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.items).toEqual([]);
    });

    it('an off-page change that newly passes the active phase filter raises the chip', async () => {
      const projectId = 'p-paged-newly-passes';
      const agents = Array.from({ length: 30 }, (_, i) =>
        makeAgent(i, { projectId, phase: i === 29 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 10)); // the phase change's own one paged refetch
      await el.updateComplete;
      expect(internals(el).agentWindow.state).toBe('paged');

      await internals(el).agentWindow.next();
      await el.updateComplete;
      const before = requests.length;

      // agents[29] (off-page, currently filtered out) starts running.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: agents[29].id, phase: 'running' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(requests.length).toBe(before); // still zero-cost (design §6.2)
    });

    it('an off-page status change for a non-member under a different committed label never inflates stats.total and never raises the chip', async () => {
      const projectId = 'p-paged-label';
      const members = Array.from({ length: 30 }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: 'prod' } })
      );
      const nonMember = makeAgent(999, { projectId, labels: { env: 'dev' }, phase: 'stopped' });
      const agents = [...members, nonMember];
      const requests: AgentsRequest[] = [];
      localStorage.setItem('scion-view-project-agents', 'list');
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0');
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      internals(el).committedLabel = 'env=prod'; // simulate having committed this label (bypasses UI for brevity)
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentStats.total).toBe(30); // only the 30 env=prod members

      const before = requests.length;
      // The non-member (env=dev, a different project-scope agent) sends a
      // status update. It is not on any page and was never a member, so the
      // off-page add rule (design §6.2's add rule, mirroring the server's
      // stats population) must not add it.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: nonMember.id, phase: 'running' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(internals(el).agentStats.total).toBe(30); // unchanged
      expect(internals(el).agentWindow.updatesAvailable).toBe(false); // ignored outright: no chip
      expect(requests.length).toBe(before);
    });

    it('on-page delete and phase-filter-failure still show the chip (backfill)', async () => {
      const projectId = 'p-paged-backfill';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.items.length).toBe(5);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: agents[0].id },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(internals(el).agentWindow.items.find((a) => a.id === agents[0].id)).toBeUndefined();
    });

    it('reconnect (agents-resync) raises the chip with no request (plumbing only — state.ts owns resync detection itself)', async () => {
      const projectId = 'p-paged-resync';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      const before = requests.length;

      stateManager.dispatchEvent(new Event('agents-resync'));
      await el.updateComplete;

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(requests.length).toBe(before);
    });
  });

  describe('small state: live updates', () => {
    it('an SSE status delta updates the rendered list row, and an SSE create appears, with no re-adoption and no page reset', async () => {
      const projectId = 'p-small-live';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agentWindow.items.find((a) => a.id === 'a-4')?.phase).toBe('running');
      const beforeAgents = (el as unknown as { agents: Agent[] }).agents;
      const untouched = beforeAgents.find((a) => a.id === 'a-3');

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: 'a-4', phase: 'stopped' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      // The small-state list view must see the same live update grid/tree/
      // stats already saw via `this.agents` — no re-adoption step, and no
      // extra request.
      const afterAgents = (el as unknown as { agents: Agent[] }).agents;
      expect(afterAgents.find((a) => a.id === 'a-4')?.phase).toBe('stopped');
      expect(internals(el).agentWindow.items.find((a) => a.id === 'a-4')?.phase).toBe('stopped');
      expect(requests.length).toBe(1);
      // mergeChanged (design §7): the array identity changed (a-4 changed),
      // but every untouched agent is carried over by reference.
      expect(afterAgents).not.toBe(beforeAgents);
      expect(afterAgents.find((a) => a.id === 'a-3')).toBe(untouched);

      // A brand-new SSE-created agent, sorted to the top by `updated` desc, appears.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: 'a-new',
          id: 'a-new',
          name: 'brand-new',
          projectId,
          template: 't',
          phase: 'running',
          created: '2026-02-01T00:00:00Z',
          updated: '2026-02-01T00:00:00Z',
          messageMode: 'project',
        },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(internals(el).agentWindow.items.some((a) => a.id === 'a-new')).toBe(true);
      expect(requests.length).toBe(1); // still no request
    });

    it('a created event with a foreign projectId is not added (the project add rule)', async () => {
      const projectId = 'p-small-foreign-create';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 3 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small');
      const before = (el as unknown as { agents: Agent[] }).agents.length;

      // Arrives on this project's own subject, but the payload names a
      // different project — the add rule gates on the agent object's own
      // `projectId`, not the subject it arrived on.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: 'a-foreign',
          id: 'a-foreign',
          name: 'a-foreign',
          projectId: 'some-other-project',
          template: 't',
          phase: 'running',
          created: '2026-04-01T00:00:00Z',
          updated: '2026-04-01T00:00:00Z',
          messageMode: 'project',
        },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      const after = (el as unknown as { agents: Agent[] }).agents;
      expect(after.length).toBe(before);
      expect(after.some((a) => a.id === 'a-foreign')).toBe(false);
    });

    it('an SSE-created agent keeps its inherited capabilities across its next status delta', async () => {
      const projectId = 'p-small-caps';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 2 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: 'a-caps',
          id: 'a-caps',
          name: 'a-caps',
          projectId,
          template: 't',
          phase: 'running',
          created: '2026-02-01T00:00:00Z',
          updated: '2026-02-01T00:00:00Z',
          messageMode: 'project',
          // No `_capabilities` of its own, as a real SSE create carries.
        },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      const afterCreate = (el as unknown as { agents: Agent[] }).agents.find(
        (a) => a.id === 'a-caps'
      );
      expect(afterCreate?._capabilities).toBeTruthy();

      // Its next delta carries no `_capabilities` either, same as any real
      // status update.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: 'a-caps', phase: 'stopped' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      const afterStatus = (el as unknown as { agents: Agent[] }).agents.find(
        (a) => a.id === 'a-caps'
      );
      expect(afterStatus?.phase).toBe('stopped');
      expect(afterStatus?._capabilities).toBe(afterCreate?._capabilities);
    });

    it('a REST response landing after an SSE delete does not resurrect the deleted agent', async () => {
      const projectId = 'p-small-tombstone';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 3 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agentWindow.items.some((a) => a.id === 'a-1')).toBe(true);

      // The hub tells this client 'a-1' is gone.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: 'a-1' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;
      expect(internals(el).agentWindow.items.some((a) => a.id === 'a-1')).toBe(false);

      // A lifecycle refresh re-fetches, and the fixture's fetch handler still
      // returns the original fixture list — 'a-1' included — because it has
      // no knowledge of the delete (exactly like a REST response that was
      // already in flight, or served from a stale read replica, when the
      // delete happened). The already-tombstoned ID must not reappear.
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect((el as unknown as { agents: Agent[] }).agents.some((a) => a.id === 'a-1')).toBe(false);
      expect(internals(el).agentWindow.items.some((a) => a.id === 'a-1')).toBe(false);
    });

    it('a legacy-mode reload drops an agent already removed by an SSE delete', async () => {
      // Same race, through `loadLegacyAgentsImpl` instead of the sorted-mode
      // load above: grid view (the default) is not sorted-mode eligible, so
      // every load here goes through the legacy path.
      const projectId = 'p-legacy-tombstone';
      const agents = Array.from({ length: 3 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect((el as unknown as { agents: Agent[] }).agents.some((a) => a.id === 'a-1')).toBe(true);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: 'a-1' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect((el as unknown as { agents: Agent[] }).agents.some((a) => a.id === 'a-1')).toBe(false);
    });

    it('a page-level load tolerates a response with no agents field', async () => {
      // With a tombstone recorded, dropTombstoned iterates its input, so a
      // missing `agents` field must fall back to an empty list, not throw.
      const projectId = 'p-small-missing-agents';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 3 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({ projectId, projectCaps: { actions: ['read'] }, agents, requests })
        )
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small');

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.deleted`,
        data: { agentId: 'a-1' },
      });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      stubSortedAgentsWithoutList(projectId, globalThis.fetch);
      const warn = vi.spyOn(console, 'warn');
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(warn).not.toHaveBeenCalledWith('Background refresh failed:', expect.anything());
      expect((el as unknown as { agents: Agent[] }).agents).toEqual([]);
      expect(internals(el).agentWindow.items).toEqual([]);
    });
  });

  describe('paged -> legacy transitions issue exactly one request each', () => {
    it('sort -> name (legacy) and name -> updated (paged again) each issue exactly one request; a phase change while paged issues exactly one', async () => {
      const projectId = 'p-b2b3';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged for every sorted request in this test
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true, // the legacy fallback is also "large" (truncated)
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');

      let n = requests.length;
      internals(el).toggleSort('name');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1); // exactly one legacy load, not two
      expect(requests[requests.length - 1].url).not.toContain('sort=');
      expect(internals(el).agentWindow.state).toBe('small'); // out of 'paged' even though truncated

      n = requests.length;
      internals(el).toggleSort('updated');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1); // exactly one fit request, not two
      expect(requests[requests.length - 1].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('paged'); // forced paged again (fit=0)

      n = requests.length;
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1); // phase change while paged: exactly one (design row 6)
    });

    it('grid<->list toggles cost exactly the documented interim requests, then zero, once the set is complete and promoted to small (not the always-paged case — see below)', async () => {
      const projectId = 'p-b3-toggles';
      localStorage.setItem('scion-view-project-agents', 'list');
      // 30 agents; the sorted endpoint reports complete once asked again
      // with the real fit threshold, so after one legacy load and one
      // fit retry the window settles into 'small' and further toggles are free.
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          // Force only the FIRST sorted request (the initial page load) to be
          // paged, by capping `fit` there; subsequent requests use the real
          // threshold and come back complete.
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true,
          })(input, init);
        })
      );
      // The mount's own first request uses the real fit threshold, and 30
      // is below it, so it is actually complete — use a dedicated
      // paged-first-load harness instead: start already paged via a forced
      // page-1 navigation isn't meaningful here, so this test instead
      // begins in list+paged by making the window ineligible at mount
      // (grid).
      localStorage.setItem('scion-view-project-agents', 'grid');
      const el = await createComponent(projectId);
      // Grid load is legacy (not P1-eligible); truncated per legacyTruncated.
      expect(requests.length).toBe(1);
      expect(internals(el).listViewUsesWindow).toBe(false);

      const toggle = viewToggle(el)!;
      let n = requests.length;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await new Promise((r) => setTimeout(r, 10));
      // grid(truncated) -> list(updated sort): one fit request (design §11 interim cost).
      expect(requests.length - n).toBe(1);
      expect(internals(el).listViewUsesWindow).toBe(true); // 30 is below the fit threshold: complete this time

      n = requests.length;
      for (const v of ['grid', 'list', 'grid', 'list', 'grid']) {
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await new Promise((r) => setTimeout(r, 10));
      }
      expect(requests.length - n).toBe(0); // held: this.agents already has the complete set
    }, 20_000);

    it('always-paged (fit=0): a dir flip costs one request, and list<->grid toggles cost exactly one each, alternating legacy and fit', async () => {
      const projectId = 'p-b3-always-paged';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // every sorted request in this test stays paged
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true, // the legacy fallback is also "large" (truncated), so it never promotes to small either
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');

      let n = requests.length;
      internals(el).toggleSort('updated'); // same field: flips dir only
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1);
      // Assert the actual dir, not just the count.
      expect(requests[requests.length - 1].url).toContain('sort=updated');
      expect(requests[requests.length - 1].url).toContain('dir=asc');

      const toggle = viewToggle(el)!;
      const perToggleCosts: number[] = [];
      const perToggleKinds: Array<'legacy' | 'fit'> = [];
      for (const v of ['grid', 'list', 'grid', 'list', 'grid', 'list']) {
        n = requests.length;
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await new Promise((r) => setTimeout(r, 10));
        perToggleCosts.push(requests.length - n);
        const issued = requests.slice(n);
        perToggleKinds.push(issued.every((r) => !r.url.includes('sort=')) ? 'legacy' : 'fit');
      }
      // Every toggle costs exactly one request: ->grid is the legacy load
      // (exiting 'paged'), ->list is the fit request (re-entering 'paged',
      // since this fixture never reports complete) — design §11's two
      // interim-cost bullets, paid on every toggle while the project stays
      // above the fit threshold.
      expect(perToggleCosts).toEqual([1, 1, 1, 1, 1, 1]);
      // Assert the actual alternation the title claims.
      expect(perToggleKinds).toEqual(['legacy', 'fit', 'legacy', 'fit', 'legacy', 'fit']);
    }, 20_000);
  });

  describe('label typing while paged', () => {
    it('does not reset pageIndex or issue a request, and DOES apply the live preview filter to the page (design §6.3)', async () => {
      const projectId = 'p-b1-typing';
      localStorage.setItem('scion-view-project-agents', 'list');
      // Every 5th agent carries the env=prod label, so the current page has
      // a predictable, non-trivial filtered subset.
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, ...(i % 5 === 0 ? { labels: { env: 'prod' } } : {}) })
      );
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0');
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      await internals(el).agentWindow.next();
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(1);
      const itemsBefore = internals(el).agentWindow.items;
      const expectedFiltered = itemsBefore.filter((a) => a.labels?.env === 'prod').map((a) => a.id);
      expect(expectedFiltered.length).toBeGreaterThan(0);
      expect(expectedFiltered.length).toBeLessThan(itemsBefore.length);

      const before = requests.length;
      const input = labelInput(el)!;
      input.value = 'env';
      input.dispatchEvent(new Event('sl-input'));
      await el.updateComplete;

      expect(internals(el).agentWindow.pageIndex).toBe(1); // unchanged
      // The preview filter IS applied to the page.
      expect(internals(el).agentWindow.items.map((a) => a.id)).toEqual(expectedFiltered);
      expect(requests.length).toBe(before); // still zero
      const pagerEl = el.shadowRoot?.querySelector('scion-agent-pager') as unknown as {
        pageIndex: number;
        hasPrev: boolean;
      } | null;
      expect(pagerEl?.pageIndex).toBe(1);
      expect(pagerEl?.hasPrev).toBe(true);
    });
  });

  describe('a paged -> small transition resets pageIndex to 0', () => {
    function fixture60(projectId: string) {
      return Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, labels: i < 10 ? { env: 'prod' } : { env: 'dev' } })
      );
    }

    function stubAlwaysPagedUnlessLabelled(
      projectId: string,
      agents: Agent[],
      requests: AgentsRequest[]
    ) {
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            // Only the unlabelled candidate set (60) is forced paged; the
            // env=prod subset (10) fits and comes back complete.
            if (!u.searchParams.get('label')) u.searchParams.set('fit', '0');
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );
    }

    it('a k=v label commit whose set fits (paged -> small via the fit path) lands on page 0', async () => {
      const projectId = 'p-r3a1';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = fixture60(projectId);
      const requests: AgentsRequest[] = [];
      stubAlwaysPagedUnlessLabelled(projectId, agents, requests);

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      await internals(el).agentWindow.next();
      await internals(el).agentWindow.next();
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(2);

      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('small'); // 10 agents is below the fit threshold
      expect(internals(el).agentWindow.pageIndex).toBe(0); // reset
      expect(internals(el).agentWindow.items.length).toBe(10); // all 10 env=prod agents visible
    });

    it('a bare-key label commit (legacy path, paged -> small) lands on page 0', async () => {
      const projectId = 'p-r3a2';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = fixture60(projectId);
      const requests: AgentsRequest[] = [];
      stubAlwaysPagedUnlessLabelled(projectId, agents, requests);

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      await internals(el).agentWindow.next();
      await internals(el).agentWindow.next();
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(2);

      const input = labelInput(el)!;
      input.value = 'env'; // bare key: not P1-eligible, goes through the legacy path
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('small'); // legacy load, no nextCursor
      expect(internals(el).agentWindow.pageIndex).toBe(0); // reset
    });
  });

  describe('persisted page size', () => {
    it('is read once in connectedCallback, used for the first request, and matches the rendered pager', async () => {
      const projectId = 'p-b4';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem('scion-pagesize-project-agents', '50');
      const agents = Array.from({ length: 80 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })
        )
      );

      const el = await createComponent(projectId);
      expect(requests[0]?.url).toContain('limit=50');
      expect(internals(el).pagerPageSize).toBe(50);
      expect(internals(el).agentWindow.items.length).toBe(50);
      const pagerEl = pager(el);
      expect(pagerEl?.pageSize).toBe(50);
    });

    it('ignores an invalid stored value and falls back to the default', async () => {
      const projectId = 'p-b4-invalid';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem('scion-pagesize-project-agents', '17');
      const agents = Array.from({ length: 10 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })
        )
      );
      const el = await createComponent(projectId);
      expect(internals(el).pagerPageSize).toBe(25);
      expect(requests[0]?.url).toContain('limit=25');
    });
  });

  describe('stale-response guard', () => {
    it('an older trigger whose response resolves later never overwrites a newer one', async () => {
      const projectId = 'p-b5';
      localStorage.setItem('scion-view-project-agents', 'list');
      const mountAgents = [makeAgent(0, { projectId, name: 'mount' })];
      const firstTriggerAgents = [makeAgent(1, { projectId, name: 'first-trigger' })];
      const secondTriggerAgents = [makeAgent(2, { projectId, name: 'second-trigger' })];
      const requests: AgentsRequest[] = [];

      let sortedCallCount = 0;
      const pending: Array<{ resolve: (r: Response) => void }> = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            sortedCallCount++;
            const call = sortedCallCount;
            if (call === 1) {
              // The mount's own load resolves immediately, as normal.
              return createRealisticFetchHandler({
                projectId,
                projectCaps: { actions: ['read'] },
                agents: mountAgents,
                requests,
              })(input, init);
            }
            // Calls 2 and 3 (the two manual triggers below) are held open
            // until the test resolves them explicitly, out of order, with
            // the response body supplied by the test at resolve time.
            requests.push({ url: rawUrl });
            const { promise, resolve } = deferred<Response>();
            pending[call] = { resolve };
            return promise;
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents: mountAgents,
            requests,
          })(input, init);
        })
      );

      const el = await createComponent(projectId);
      expect((el as unknown as { agents: Agent[] }).agents.map((a) => a.name)).toEqual(['mount']);

      // Trigger #1 (older), then #2 (newer), both now in flight.
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 5));
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 5));

      // Resolve the NEWER one first.
      pending[3].resolve(
        jsonResponse({
          agents: secondTriggerAgents,
          totalCount: 1,
          complete: true,
          stats: { total: 1, running: 1, agents: [[secondTriggerAgents[0].id, 'running']] },
        })
      );
      await new Promise((r) => setTimeout(r, 10));
      expect((el as unknown as { agents: Agent[] }).agents.map((a) => a.name)).toEqual([
        'second-trigger',
      ]);

      // Resolve the OLDER one afterward — it must be discarded.
      pending[2].resolve(
        jsonResponse({
          agents: firstTriggerAgents,
          totalCount: 1,
          complete: true,
          stats: { total: 1, running: 1, agents: [[firstTriggerAgents[0].id, 'running']] },
        })
      );
      await new Promise((r) => setTimeout(r, 10));
      expect((el as unknown as { agents: Agent[] }).agents.map((a) => a.name)).toEqual([
        'second-trigger',
      ]);
    });
  });

  describe('pager navigation is disabled while a page-level load is in flight', () => {
    /**
     * A Next/Prev/chip click must not race a page-level view-state request:
     * if it did, the window's own cursor stack would still belong to the
     * *old* phase/dir/label, so the resulting request would bind a stale
     * cursor to new params and the server would legitimately 400 it (design
     * §4.4). The fix is to disable navigation (and the chip) outright while
     * either the window's own fetch or, while paged, a page-level load is
     * in flight, via `.loading=${agentWindow.loading || (agentWindow.state
     * === 'paged' && agentsLoading)}` — the page-level half only applies in
     * the paged state, so a held refresh never blocks small-state local
     * paging. `onPagerNav` is a defense-in-depth guard with the same
     * condition, for anything that bypasses the pager's own click handler.
     */
    function pagerLoading(el: TestEl): boolean {
      return (el.shadowRoot!.querySelector('scion-agent-pager') as unknown as { loading: boolean })
        .loading;
    }

    function clickNext(el: TestEl): void {
      // Exercises the pager's own real guard (`onNext`'s `this.loading`
      // check) rather than dispatching the bare 'next' event, which would
      // bypass that guard entirely.
      (el.shadowRoot!.querySelector('scion-agent-pager') as unknown as { onNext(): void }).onNext();
    }

    it('a Next click while a lifecycle refresh is in flight is a no-op; the refresh then lands on page 0 as intended', async () => {
      const projectId = 'p-n1-prime';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      let holdNextPageZeroFit = false;
      let heldResolve: ((r: Response) => void) | null = null;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged
            if (holdNextPageZeroFit && !u.searchParams.has('cursor')) {
              holdNextPageZeroFit = false;
              requests.push({ url: rawUrl });
              return new Promise<Response>((resolve) => {
                heldResolve = resolve;
              });
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(pagerLoading(el)).toBe(false);

      // Start a lifecycle refresh (a page-0 fit request) and hold its response.
      holdNextPageZeroFit = true;
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(pagerLoading(el)).toBe(true); // the pager is disabled during the gap

      const requestsBeforeClick = requests.length;
      clickNext(el); // the pager's own guard makes this a no-op
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(0); // never navigated
      expect(requests.length).toBe(requestsBeforeClick); // no mismatched-cursor request (design §4.4)

      // Now let the refresh finally resolve.
      heldResolve!(
        jsonResponse({
          agents: agents.slice(0, 25),
          totalCount: 30,
          complete: false,
          nextCursor: '25',
          stats: {
            total: 30,
            running: 30,
            agents: agents.map((a) => [a.id, a.phase]),
          },
        })
      );
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(internals(el).agentWindow.pageIndex).toBe(0); // lands on page 0, as the refresh intended
      expect(pagerLoading(el)).toBe(false); // re-enabled
      // Next now works normally.
      clickNext(el);
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(1);
    });

    it('small-state local Next/Prev stay enabled, and work, during a held lifecycle refresh', async () => {
      // Small-state pagination is a pure local slice of `display` (design
      // §6.3) and sends no request, so it has nothing to race with a
      // page-level load — the `.loading` gate (and `onPagerNav`'s guard)
      // only apply the page-level half in the paged state, so this isn't
      // disabled for the duration of every agent action.
      const projectId = 'p-n2-prime-small';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      let holdNextSorted = false;
      let heldResolve: ((r: Response) => void) | null = null;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            if (holdNextSorted) {
              holdNextSorted = false;
              requests.push({ url: rawUrl });
              return new Promise<Response>((resolve) => {
                heldResolve = resolve;
              });
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small'); // 30 agents is below the fit threshold

      const requestsBeforeRefresh = requests.length;
      holdNextSorted = true;
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(pagerLoading(el)).toBe(false); // unlike the paged state, the gate does not fire here
      expect(requests.length).toBe(requestsBeforeRefresh + 1); // the held refresh request was sent

      clickNext(el); // the pager's own real guard — must not be disabled
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(1); // local paging worked
      expect(requests.length).toBe(requestsBeforeRefresh + 1); // Next sent nothing

      heldResolve!(
        jsonResponse({
          agents,
          totalCount: agents.length,
          complete: true,
          stats: {
            total: agents.length,
            running: agents.filter((a) => a.phase === 'running').length,
            agents: agents.map((a) => [a.id, a.phase]),
          },
        })
      );
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('small');
      expect(pagerLoading(el)).toBe(false);
      expect(internals(el).agentWindow.pageIndex).toBe(1); // small -> small: unaffected by the refresh
      // Paging continues to work normally once the refresh has landed.
      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onPrev(): void;
      };
      pg.onPrev();
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(0);
      clickNext(el);
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(1);
    });

    for (const kind of ['phase', 'dir', 'label', 'pagesize'] as const) {
      it(`a ${kind} change while paged disables Next until its own fit response lands, so no mismatched-cursor request is ever sent`, async () => {
        const projectId = `p-r3b-${kind}`;
        localStorage.setItem('scion-view-project-agents', 'list');
        const agents = Array.from({ length: 60 }, (_, i) =>
          makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running', labels: { env: 'dev' } })
        );
        const requests: AgentsRequest[] = [];
        let holdNextPageZeroFit = false;
        let heldResolve: ((r: Response) => void) | null = null;
        vi.stubGlobal(
          'fetch',
          vi.fn((input: string | URL | Request, init?: RequestInit) => {
            const rawUrl =
              typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
            const u = new URL(rawUrl, 'http://localhost');
            if (
              u.pathname === `/api/v1/projects/${projectId}/agents` &&
              u.searchParams.get('sort')
            ) {
              u.searchParams.set('fit', '0'); // always paged
              if (holdNextPageZeroFit && !u.searchParams.has('cursor')) {
                holdNextPageZeroFit = false;
                requests.push({ url: rawUrl });
                return new Promise<Response>((resolve) => {
                  heldResolve = resolve;
                });
              }
            }
            return createRealisticFetchHandler({
              projectId,
              projectCaps: { actions: ['read'] },
              agents,
              requests,
            })(u.toString(), init);
          })
        );

        const el = await createComponent(projectId);
        expect(internals(el).agentWindow.state).toBe('paged');

        holdNextPageZeroFit = true;
        if (kind === 'phase') internals(el).setPhaseFilter('stopped');
        else if (kind === 'dir') internals(el).toggleSort('updated');
        else if (kind === 'label') {
          const input = labelInput(el)!;
          input.value = 'env=dev';
          input.dispatchEvent(new Event('sl-input'));
          input.dispatchEvent(new Event('sl-change'));
        } else {
          (el as unknown as { onPagerSizeChange(n: number): void }).onPagerSizeChange(50);
        }
        await new Promise((r) => setTimeout(r, 10));
        await el.updateComplete;
        expect(pagerLoading(el)).toBe(true);

        const requestsBeforeClick = requests.length;
        clickNext(el);
        await new Promise((r) => setTimeout(r, 10));
        expect(requests.length).toBe(requestsBeforeClick); // no cursor request sent at all
        expect(internals(el).agentWindow.error).toBeNull(); // in particular, no 400

        heldResolve!(
          jsonResponse({
            agents: agents.slice(0, 25),
            totalCount: 60,
            complete: false,
            nextCursor: '25',
            stats: { total: 60, running: 30, agents: agents.map((a) => [a.id, a.phase]) },
          })
        );
        await new Promise((r) => setTimeout(r, 20));
        await el.updateComplete;

        expect(internals(el).agentWindow.pageIndex).toBe(0); // the trigger's own page 0 landed
        expect(internals(el).agentWindow.error).toBeNull();
      });
    }
  });

  describe("a failed view-change request while paged invalidates cursors, so Next can't replay a stale one", () => {
    it('a phase change that 500s leaves the window paged with no error; Next then no-ops instead of sending a mismatched cursor', async () => {
      const projectId = 'p-n1-triple-prime';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let failNextSorted = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged
            if (failNextSorted && !u.searchParams.has('cursor')) {
              failNextSorted = false;
              requests.push({ url: rawUrl });
              return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(true);

      failNextSorted = true;
      internals(el).setPhaseFilter('stopped');
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      // The failed view-change request keeps the previous page (design §6.3)
      // but must no longer claim Next is possible: the stored cursor was
      // minted under the old (unfiltered) phase, and a Next now would bind
      // it to `phase=stopped` and get a 400 (design §4.4).
      expect(internals(el).agentWindow.error).toBeNull(); // the failure itself is silent (previous data kept)
      expect(internals(el).agentWindow.hasNext).toBe(false); // invalidated

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext(); // the pager's own guard refuses: hasNext is false
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request was ever sent
      expect(internals(el).agentWindow.error).toBeNull(); // in particular, no 400
      expect(internals(el).agentWindow.pageIndex).toBe(0);

      // A subsequent successful view-change restores navigation via
      // setPaged — this is a *different* phase, not a retry of 'stopped',
      // because any successful view-change restores it, not just a retry
      // of the one that failed.
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.hasNext).toBe(true);
    });

    it('a phase change that fails with a NETWORK error also invalidates cursors', async () => {
      const projectId = 'p-n1-quad-prime-net';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let failNextSorted = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged
            if (failNextSorted && !u.searchParams.has('cursor')) {
              failNextSorted = false;
              requests.push({ url: rawUrl });
              return Promise.reject(new TypeError('Failed to fetch'));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(true);

      failNextSorted = true;
      internals(el).setPhaseFilter('stopped');
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      // The rejected fetch (not a non-OK response) must still invalidate:
      // a thrown network error must not fall through the catch and leave
      // the stale cursor armed.
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.hasNext).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();
    });

    it('a phase change whose response is ok but json() rejects also invalidates cursors, with no unhandled rejection', async () => {
      const projectId = 'p-n1-sorted-parse-error';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let failNextSorted = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged
            if (failNextSorted && !u.searchParams.has('cursor')) {
              failNextSorted = false;
              requests.push({ url: rawUrl });
              // ok: true, but the body is not valid JSON — response.json()
              // rejects even though the request itself succeeded (the bug
              // this test proves is fixed: that rejection used to be
              // unhandled, since response.json() ran outside a try/catch).
              return Promise.resolve(new Response('not json', { status: 200 }));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(true);
      const itemsBefore = internals(el).agentWindow.items;

      failNextSorted = true;
      internals(el).setPhaseFilter('stopped');
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      // A parse failure on an otherwise-ok response must be treated exactly
      // like the existing network-error failure exit for this path: the
      // previous page is kept and the stale cursor is invalidated exactly
      // once (not left armed, and not invalidated a second time by some
      // other path).
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.items).toBe(itemsBefore);
      expect(internals(el).agentWindow.hasNext).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext(); // the pager's own guard refuses: hasNext is false
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();

      // A subsequent successful view-change still restores navigation, so
      // the invalidation above was not a permanent, leaked failure state.
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.hasNext).toBe(true);
    });

    it('a view-change that 422s, whose legacy fallback also fails, invalidates cursors', async () => {
      const projectId = 'p-n1-quad-prime-legacy';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let fail422NextSorted = false;
      let failNextLegacy = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents`) {
            if (u.searchParams.get('sort')) {
              u.searchParams.set('fit', '0'); // always paged when sorted
              if (fail422NextSorted && !u.searchParams.has('cursor')) {
                fail422NextSorted = false;
                requests.push({ url: rawUrl });
                return Promise.resolve(
                  jsonResponse({ error: { code: 'sorted_view_unavailable' } }, 422)
                );
              }
            } else if (failNextLegacy) {
              failNextLegacy = false;
              requests.push({ url: rawUrl });
              return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(true);

      // A phase change (trigger 'view-change') whose sorted request 422s;
      // the legacy fallback it triggers then itself fails (500) — the
      // window must still be left with invalidated navigation, not a stale
      // cursor armed under the old (pre-change) params.
      fail422NextSorted = true;
      failNextLegacy = true;
      internals(el).setPhaseFilter('stopped');
      await new Promise((r) => setTimeout(r, 30));
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('paged'); // the legacy failure doesn't change window state
      expect(internals(el).agentWindow.hasNext).toBe(false);
      expect(internals(el).agentWindow.hasPrev).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();
    });

    it('a view-change that 422s, whose legacy fallback rejects with a NETWORK error, also invalidates cursors', async () => {
      const projectId = 'p-legacy-network-error';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let fail422NextSorted = false;
      let failNextLegacy = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents`) {
            if (u.searchParams.get('sort')) {
              u.searchParams.set('fit', '0'); // always paged when sorted
              if (fail422NextSorted && !u.searchParams.has('cursor')) {
                fail422NextSorted = false;
                requests.push({ url: rawUrl });
                return Promise.resolve(
                  jsonResponse({ error: { code: 'sorted_view_unavailable' } }, 422)
                );
              }
            } else if (failNextLegacy) {
              failNextLegacy = false;
              requests.push({ url: rawUrl });
              return Promise.reject(new TypeError('Failed to fetch'));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(true);

      // Same as the 500 case above, but the legacy fallback's fetch itself
      // rejects rather than resolving with a non-OK response — this
      // exercises the legacy path's catch branch specifically, which a
      // non-OK response never reaches.
      fail422NextSorted = true;
      failNextLegacy = true;
      internals(el).setPhaseFilter('stopped');
      await new Promise((r) => setTimeout(r, 30));
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(false);
      expect(internals(el).agentWindow.hasPrev).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();
    });
  });

  describe('a paged label commit that fails reverts the committed label on every failure path, not just a non-OK response', () => {
    it('a label commit whose sorted request rejects with a NETWORK error reverts committedLabel, so Next does not send the new label against an old cursor', async () => {
      const projectId = 'p-label-commit-network-error';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running', labels: { env: 'dev' } })
      );
      const requests: AgentsRequest[] = [];
      let failNextSorted = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged
            if (failNextSorted && !u.searchParams.has('cursor')) {
              failNextSorted = false;
              requests.push({ url: rawUrl });
              return Promise.reject(new TypeError('Failed to fetch'));
            }
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).committedLabel).toBe('');
      expect(internals(el).agentWindow.hasNext).toBe(true);

      // Commit a label; its request rejects with a network error. Without
      // reverting committedLabel here (the gap this test closes), the next
      // change would resend the new label bound to the cursor minted under
      // the OLD (pre-commit) label and get a 400 (design §4.4).
      failNextSorted = true;
      const input = labelInput(el)!;
      input.value = 'env=dev';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 30));
      await el.updateComplete;

      expect(internals(el).committedLabel).toBe(''); // reverted, not left at the rejected label
      expect(internals(el).agentWindow.state).toBe('paged'); // previous data kept
      // A label-commit failure doesn't invalidate navigation the way a
      // view-change failure does: the stored cursors are still correctly
      // bound to the label that's back in effect (the empty one), so they
      // remain usable.
      expect(internals(el).agentWindow.hasNext).toBe(true);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      // Exactly one legitimate request, bound to the reverted (empty)
      // label, with no 400: this is what reverting committedLabel on a
      // network error (not just a non-OK response) prevents from
      // mismatching.
      expect(requests.length).toBe(requestsBeforeNext + 1);
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.pageIndex).toBe(1);
    });

    it('a label commit routed to the legacy path whose response is ok but json() rejects reverts committedLabel, with no unhandled rejection', async () => {
      const projectId = 'p-legacy-parse-error-label-commit';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 5 }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: 'dev' } })
      );
      const requests: AgentsRequest[] = [];
      let failNextLegacy = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (
            failNextLegacy &&
            u.pathname === `/api/v1/projects/${projectId}/agents` &&
            !u.searchParams.get('sort')
          ) {
            failNextLegacy = false;
            requests.push({ url: rawUrl });
            // ok: true, but the body is not valid JSON — response.json()
            // rejects even though the request itself succeeded (the bug
            // this test proves is fixed: that rejection used to be
            // unhandled, since response.json() ran outside a try/catch).
            return Promise.resolve(new Response('not json', { status: 200 }));
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small'); // P1-eligible, complete (5 is below the fit threshold)
      expect(internals(el).committedLabel).toBe('');

      // First, a successful commit on the sorted (P1-eligible) path, so the
      // revert below has a non-trivial value to land back on rather than
      // coincidentally landing on the initial empty label.
      const input = labelInput(el)!;
      input.value = 'env=dev';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(internals(el).committedLabel).toBe('env=dev');
      const agentsAfterFirstCommit = internals(el).agents;

      // A label with no "=" is not P1-eligible (design §11), so this commit
      // is routed to the legacy path — the second of the two call sites
      // this test suite covers.
      failNextLegacy = true;
      const requestsBeforeFailingCommit = requests.length;
      input.value = 'badlabel';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeFailingCommit + 1); // exactly one request for the failing commit
      expect(internals(el).committedLabel).toBe('env=dev'); // reverted to the prior commit, not left at 'badlabel'
      expect(internals(el).agents).toBe(agentsAfterFirstCommit); // previous rows kept
    });
  });

  describe('fit-path label 400 restores the previous committedLabel', () => {
    it('a 400 on the sorted (fit) path keeps the previous data and reverts committedLabel', async () => {
      const projectId = 'p-n3-fit-400';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      let failLabelOnFitPath = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (
            failLabelOnFitPath &&
            u.pathname === `/api/v1/projects/${projectId}/agents` &&
            u.searchParams.get('sort') &&
            u.searchParams.get('label') === 'env=prod'
          ) {
            requests.push({ url: rawUrl });
            return Promise.resolve(jsonResponse({ error: { message: 'bad label' } }, 400));
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('small'); // P1-eligible, complete (5 is below the fit threshold)
      const before = (el as unknown as { agents: Agent[] }).agents;
      expect(before.length).toBe(5);
      expect(internals(el).committedLabel).toBe('');

      failLabelOnFitPath = true;
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await new Promise((r) => setTimeout(r, 10));

      // The sorted (fit) request, not the legacy path, received the 400.
      expect(requests.some((r) => r.url.includes('sort=') && r.url.includes('label=env'))).toBe(
        true
      );
      const after = (el as unknown as { agents: Agent[] }).agents;
      expect(after.length).toBe(5); // previous data kept (design §6.3)
      expect(internals(el).committedLabel).toBe(''); // reverted, not left at the rejected label
    });
  });

  describe('a loading indicator replaces the empty-filter message during the paged -> grid gap', () => {
    it('shows "Loading agents…" while the legacy fallback is in flight, then the grid once it lands', async () => {
      const projectId = 'p-n5-loading';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      let holdLegacy = false;
      let heldResolve: ((r: Response) => void) | null = null;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            u.searchParams.set('fit', '0'); // always paged at mount
          } else if (
            holdLegacy &&
            u.pathname === `/api/v1/projects/${projectId}/agents` &&
            !u.searchParams.get('sort')
          ) {
            holdLegacy = false;
            requests.push({ url: rawUrl });
            return new Promise<Response>((resolve) => {
              heldResolve = resolve;
            });
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(u.toString(), init);
        })
      );

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');

      holdLegacy = true;
      const toggle = viewToggle(el)!;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'grid' } }));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      expect(el.shadowRoot?.textContent).toContain('Loading agents');
      expect(el.shadowRoot?.textContent).not.toContain('No agents match the current filter');

      heldResolve!(jsonResponse({ agents, _capabilities: { actions: ['read'] } }));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      expect(el.shadowRoot?.textContent).not.toContain('Loading agents');
    });

    it('a lifecycle refresh in grid with a phase filter matching nothing shows the filter-empty message, not a loading flicker', async () => {
      const projectId = 'p-n2-prime';
      localStorage.setItem('scion-view-project-agents', 'grid'); // not P1-eligible: this.agents is populated via the legacy path
      const agents = Array.from({ length: 5 }, (_, i) =>
        makeAgent(i, { projectId, phase: 'running' })
      );
      const requests: AgentsRequest[] = [];
      let holdNext = false;
      let heldResolve: ((r: Response) => void) | null = null;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          if (
            holdNext &&
            rawUrl.includes(`/api/v1/projects/${projectId}/agents`) &&
            !rawUrl.includes('sort=')
          ) {
            holdNext = false;
            requests.push({ url: rawUrl });
            return new Promise<Response>((resolve) => {
              heldResolve = resolve;
            });
          }
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(input, init);
        })
      );

      const el = await createComponent(projectId);
      expect((el as unknown as { agents: Agent[] }).agents.length).toBe(5); // this.agents already holds data

      // Filter to a phase nothing matches, then trigger a lifecycle refresh
      // (held) while that filter is active — this.agents stays non-empty
      // (the OLD data) throughout the gap.
      internals(el).setPhaseFilter('stopped');
      await el.updateComplete;
      expect(el.shadowRoot?.textContent).toContain('No agents match the current filter');

      holdNext = true;
      internals(el).backgroundRefresh('lifecycle-refresh');
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      // Must NOT flicker to "Loading agents…" here — this.agents already
      // has data, it's just phase-filtered to nothing.
      expect(el.shadowRoot?.textContent).not.toContain('Loading agents');
      expect(el.shadowRoot?.textContent).toContain('No agents match the current filter');

      heldResolve!(jsonResponse({ agents, _capabilities: { actions: ['read'] } }));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
      expect(el.shadowRoot?.textContent).toContain('No agents match the current filter');
    });
  });

  describe('backend-driven delete (ptone/scion#2483 phase 1b)', () => {
    it('202 keeps the card with Deleting… and no Delete button; SSE deleted removes it', async () => {
      const projectId = 'p-del-202';
      localStorage.setItem('scion-view-project-agents', 'grid');
      const agents = [0, 1, 2].map((i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const inner = createFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests,
      });
      const deletion = {
        state: 'deleting',
        soft: false,
        claim: 1,
        startedAt: new Date().toISOString(),
        leaseExpiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      };
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          if (init?.method === 'DELETE') {
            return Promise.resolve(jsonResponse({ agentId: 'a-1', deletion }, 202));
          }
          return inner(input, init);
        })
      );

      const el = await createComponent(projectId);
      const trashButtons = (): number =>
        el.shadowRoot?.querySelectorAll('sl-icon[name="trash"]').length ?? 0;
      expect(trashButtons()).toBe(3);

      const page = el as unknown as {
        handleAgentAction(id: string, action: string, event?: MouseEvent): Promise<void>;
      };
      await page.handleAgentAction('a-1', 'delete', { altKey: true } as MouseEvent);
      await new Promise((r) => setTimeout(r, 150)); // state.ts flush fallback
      await el.updateComplete;

      const ids = (): string[] =>
        internals(el)
          .agents.map((a) => a.id)
          .sort();
      expect(ids()).toEqual(['a-0', 'a-1', 'a-2']);
      const badges = [...(el.shadowRoot?.querySelectorAll('scion-deletion-badge') ?? [])] as Array<
        HTMLElement & { updateComplete: Promise<boolean> }
      >;
      await Promise.all(badges.map((b) => b.updateComplete));
      const labels = badges
        .map((b) => b.shadowRoot?.querySelector('.badge')?.textContent?.trim())
        .filter(Boolean);
      expect(labels).toEqual(['Deleting…']);
      expect(trashButtons()).toBe(2);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({ subject: 'agent.a-1.deleted', data: {} });
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;
      expect(ids()).toEqual(['a-0', 'a-2']);
    });

    const sseUpdate = (subject: string, data: unknown): void =>
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({ subject, data });
    const waitMs = (ms: number) => new Promise((r) => setTimeout(r, ms));

    /** Badge labels and icon counts inside table rows only (list view). */
    async function rowState(
      el: TestEl
    ): Promise<{ badges: string[]; banners: string[]; icons: (n: string) => number }> {
      const rows = [...(el.shadowRoot?.querySelectorAll('tbody tr') ?? [])];
      const badges = rows.flatMap((r) => [
        ...(r.querySelectorAll('scion-deletion-badge') as NodeListOf<
          HTMLElement & { updateComplete: Promise<boolean> }
        >),
      ]);
      const banners = rows.flatMap((r) => [
        ...(r.querySelectorAll('scion-deletion-banner') as NodeListOf<
          HTMLElement & { updateComplete: Promise<boolean> }
        >),
      ]);
      await Promise.all([...badges, ...banners].map((b) => b.updateComplete));
      return {
        badges: badges
          .map((b) => b.shadowRoot?.querySelector('.badge')?.textContent?.trim() ?? '')
          .filter(Boolean),
        // Phase 2: a failed view shows the compact failure banner instead.
        banners: banners
          .map((b) => b.shadowRoot?.querySelector('.title')?.textContent?.trim() ?? '')
          .filter(Boolean),
        icons: (n: string) =>
          rows.reduce((sum, r) => sum + r.querySelectorAll(`sl-icon[name="${n}"]`).length, 0),
      };
    }

    const lifecycleCaps = { actions: ['read', 'update', 'delete', 'lifecycle'] };

    it('list view (table rows): Deleting… badge, and Stop/Delete hidden on that row only', async () => {
      const projectId = 'p-del-rows';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = [0, 1, 2].map((i) =>
        makeAgent(i, { projectId, _capabilities: lifecycleCaps })
      );
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests: [],
          })
        )
      );
      const el = await createComponent(projectId);
      let st = await rowState(el);
      expect(st.icons('trash')).toBe(3);
      expect(st.icons('stop-circle')).toBe(3);

      sseUpdate('agent.a-1.status', {
        deletion: {
          state: 'deleting',
          soft: false,
          claim: 1,
          startedAt: new Date().toISOString(),
          leaseExpiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
        },
      });
      await waitMs(150);
      await el.updateComplete;
      st = await rowState(el);
      expect(st.badges).toEqual(['Deleting…']);
      expect(st.icons('trash')).toBe(2);
      expect(st.icons('stop-circle')).toBe(2);
    });

    it('paged list: the lease timer watches agentWindow.items and flips the row to Delete interrupted', async () => {
      const projectId = 'p-del-paged';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: PROJECT_AGENTS_FIT_THRESHOLD + 1 }, (_, i) =>
        makeAgent(i, { projectId, _capabilities: lifecycleCaps })
      );
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests: [],
          })
        )
      );
      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agents).toEqual([]); // paged: rows come from the window only
      const target = internals(el).agentWindow.items[0].id;

      // Fake clock only after mount (mount waits on real timers). The lease
      // is far enough out that nothing but the controller's timer, driven
      // by advanceTimersByTime, can flip it.
      let st: Awaited<ReturnType<typeof rowState>>;
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
      try {
        sseUpdate(`agent.${target}.status`, {
          deletion: {
            state: 'deleting',
            soft: false,
            claim: 1,
            startedAt: new Date().toISOString(),
            leaseExpiresAt: new Date(Date.now() + 20_000).toISOString(),
          },
        });
        vi.advanceTimersByTime(150); // state.ts flush fallback
        await el.updateComplete;
        expect((await rowState(el)).badges).toEqual(['Deleting…']);

        vi.advanceTimersByTime(19_000);
        await el.updateComplete;
        expect((await rowState(el)).badges).toEqual(['Deleting…']);

        // Nothing else re-renders the page: only the controller's timer can.
        vi.advanceTimersByTime(1_000);
        await el.updateComplete;
        st = await rowState(el);
      } finally {
        vi.useRealTimers();
      }
      expect(st.badges).toEqual([]);
      expect(st.banners).toEqual(['Delete interrupted']);
      expect(st.icons('trash')).toBe(internals(el).agentWindow.items.length);
    });
  });

  describe('phase 2: shared delete helper, failure banner, stopping filter (ptone/scion#2483)', () => {
    const lifecycleCaps = { actions: ['read', 'update', 'delete', 'lifecycle'] };
    const base = { soft: false, claim: 1, startedAt: new Date().toISOString() };
    const sseUpdate = (subject: string, data: unknown): void =>
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({ subject, data });
    const flushState = (): Promise<unknown> => new Promise((r) => setTimeout(r, 150));
    type BannerEl = HTMLElement & { updateComplete: Promise<boolean> };
    type Page = {
      handleAgentAction(id: string, action: string, event?: MouseEvent): Promise<void>;
      displayAgents: Agent[];
    };

    async function banners(el: TestEl, scope: string): Promise<BannerEl[]> {
      const list = [
        ...(el.shadowRoot?.querySelectorAll(`${scope} scion-deletion-banner`) ?? []),
      ] as BannerEl[];
      await Promise.all(list.map((b) => b.updateComplete));
      return list.filter((b) => !b.hasAttribute('hidden'));
    }
    const title = (b: BannerEl): string =>
      b.shadowRoot?.querySelector('.title')?.textContent?.trim() ?? '';

    /** A small project; mutations (non-GET) are recorded and answered by `onMutate`. */
    async function mountProject(
      projectId: string,
      agents: Agent[],
      view: 'grid' | 'list',
      onMutate: (url: string) => Response = () => new Response(null, { status: 204 })
    ): Promise<{ el: TestEl; mutations: string[] }> {
      localStorage.setItem('scion-view-project-agents', view);
      const mutations: string[] = [];
      const inner = createFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests: [],
      });
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const url = typeof input === 'string' ? input : input.toString();
          if (init?.method && init.method !== 'GET') {
            mutations.push(`${init.method} ${url}`);
            return Promise.resolve(onMutate(url));
          }
          return inner(input, init);
        })
      );
      const el = await createComponent(projectId);
      return { el, mutations };
    }

    beforeEach(() => {
      vi.mocked(runAgentDelete).mockClear();
      vi.mocked(showConfirm).mockClear();
      vi.mocked(showToast).mockClear();
    });

    it('Delete delegates to the shared helper; the page sends no DELETE itself', async () => {
      const agents = [0, 1].map((i) =>
        makeAgent(i, { projectId: 'p2-deleg', _capabilities: lifecycleCaps })
      );
      const { el, mutations } = await mountProject('p2-deleg', agents, 'grid');
      vi.mocked(runAgentDelete).mockResolvedValueOnce({ kind: 'deleted', forced: false });
      const click = { altKey: true } as MouseEvent;
      await (el as unknown as Page).handleAgentAction('a-1', 'delete', click);
      await el.updateComplete;
      expect(runAgentDelete).toHaveBeenCalledTimes(1);
      expect(runAgentDelete).toHaveBeenCalledWith(
        expect.objectContaining({ agentId: 'a-1', agentName: 'agent-1', event: click })
      );
      expect(mutations).toEqual([]);
      expect(internals(el).agents.map((a) => a.id)).toEqual(['a-0']);
    });

    it('a 502 now offers the force fallback (the helper owns it), and a failure is toasted', async () => {
      const agents = [makeAgent(0, { projectId: 'p2-502', _capabilities: lifecycleCaps })];
      const { el, mutations } = await mountProject('p2-502', agents, 'grid', () =>
        jsonResponse({ error: { code: 'runtime_error', message: 'broker down' } }, 502)
      );
      vi.mocked(showConfirm).mockResolvedValueOnce(false); // decline force
      await (el as unknown as Page).handleAgentAction('a-0', 'delete', {
        altKey: true,
      } as MouseEvent);
      expect(showConfirm).toHaveBeenCalledTimes(1);
      expect(vi.mocked(showConfirm).mock.calls[0][0]).toMatch(/Force delete this agent\?/);
      expect(mutations).toEqual(['DELETE /api/v1/agents/a-0']);
      expect(showToast).toHaveBeenCalledWith('broker down');
    });

    const codes: Array<[string, Record<string, unknown>, string]> = [
      [
        'runtime_error',
        { code: 'runtime_error', error: 'broker refused' },
        'Delete failed: broker refused',
      ],
      ['conflict', { code: 'conflict' }, 'Delete failed: conflict'],
      ['abandoned', { code: 'abandoned' }, 'Delete interrupted'],
      [
        'revoke_failed',
        { code: 'revoke_failed', stage: 'finalizing' },
        'Delete failed: could not revoke credentials',
      ],
      [
        'finalize_failed',
        { code: 'finalize_failed', stage: 'finalizing' },
        'Delete failed: could not finalize',
      ],
      ['in_doubt', { code: 'in_doubt' }, 'Delete failed: outcome unknown'],
    ];
    for (const [name, extra, expected] of codes) {
      it(`${name}: the card (full) and the row (compact) show the failure banner with Retry and Force`, async () => {
        for (const view of ['grid', 'list'] as const) {
          const projectId = `p2-${name}-${view}`;
          const agents = [0, 1].map((i) =>
            makeAgent(i, { projectId, _capabilities: lifecycleCaps })
          );
          const { el } = await mountProject(projectId, agents, view);
          sseUpdate('agent.a-0.status', { deletion: { ...base, state: 'failed', ...extra } });
          await flushState();
          await el.updateComplete;
          const list = await banners(el, view === 'grid' ? '.agent-card' : 'tbody tr');
          expect(list.map(title)).toEqual([expected]);
          expect(list[0].hasAttribute('compact')).toBe(view === 'list');
          expect(list[0].shadowRoot?.querySelector('.retry')).not.toBeNull();
          expect(list[0].shadowRoot?.querySelector('.force')).not.toBeNull();
          el.remove();
        }
      });
    }

    it('a client-flipped abandoned view shows the banner; a live deleting view shows none (no Force)', async () => {
      const agents = [0, 1].map((i) =>
        makeAgent(i, { projectId: 'p2-flip', _capabilities: lifecycleCaps })
      );
      const { el } = await mountProject('p2-flip', agents, 'grid');
      sseUpdate('agent.a-0.status', {
        deletion: {
          ...base,
          state: 'deleting',
          leaseExpiresAt: new Date(Date.now() + 600_000).toISOString(),
        },
      });
      sseUpdate('agent.a-1.status', {
        deletion: {
          ...base,
          state: 'deleting',
          leaseExpiresAt: new Date(Date.now() - 1_000).toISOString(),
        },
      });
      await flushState();
      await el.updateComplete;
      const list = await banners(el, '.agent-card');
      expect(list.map(title)).toEqual(['Delete interrupted']);
    });

    it('Retry calls the helper without a confirm; Force with force:true and ?force=true', async () => {
      const agents = [makeAgent(0, { projectId: 'p2-btn', _capabilities: lifecycleCaps })];
      const { el, mutations } = await mountProject('p2-btn', agents, 'list');
      sseUpdate('agent.a-0.status', { deletion: { ...base, state: 'failed', code: 'in_doubt' } });
      await flushState();
      await el.updateComplete;
      const [banner] = await banners(el, 'tbody tr');

      (banner.shadowRoot?.querySelector('.retry') as HTMLElement).click();
      await vi.waitFor(() => expect(runAgentDelete).toHaveBeenCalledTimes(1));
      await vi.mocked(runAgentDelete).mock.results[0].value;
      expect(vi.mocked(runAgentDelete).mock.calls[0][0]).toMatchObject({
        agentId: 'a-0',
        confirm: false,
      });
      expect(vi.mocked(runAgentDelete).mock.calls[0][0].force).toBeUndefined();
      expect(showConfirm).not.toHaveBeenCalled();

      (banner.shadowRoot?.querySelector('.force') as HTMLElement).click();
      await vi.waitFor(() => expect(runAgentDelete).toHaveBeenCalledTimes(2));
      await vi.mocked(runAgentDelete).mock.results[1].value;
      expect(vi.mocked(runAgentDelete).mock.calls[1][0]).toMatchObject({
        agentId: 'a-0',
        force: true,
      });
      expect(showConfirm).toHaveBeenCalledTimes(1);
      expect(mutations).toEqual([
        'DELETE /api/v1/agents/a-0',
        'DELETE /api/v1/agents/a-0?force=true',
      ]);
    });

    it('Start answered 409 delete_in_progress shows the explanation', async () => {
      const agents = [
        makeAgent(0, { projectId: 'p2-start', phase: 'stopped', _capabilities: lifecycleCaps }),
      ];
      const { el } = await mountProject('p2-start', agents, 'grid', () =>
        jsonResponse({ error: { code: 'delete_in_progress', message: 'being deleted' } }, 409)
      );
      await (el as unknown as Page).handleAgentAction('a-0', 'start');
      expect(showToast).toHaveBeenCalledWith(START_BLOCKED_BY_DELETE_MESSAGE);
    });

    it('the stopping filter shows stopping agents, and live deltas move agents in and out', async () => {
      const projectId = 'p2-filter';
      const agents = [
        makeAgent(0, { projectId, phase: 'stopping' }),
        makeAgent(1, { projectId }),
        makeAgent(2, { projectId, phase: 'stopped' }),
      ];
      const { el } = await mountProject(projectId, agents, 'grid');
      const shown = (): string[] => (el as unknown as Page).displayAgents.map((a) => a.id).sort();
      const button = [...(el.shadowRoot?.querySelectorAll('.filter-bar button') ?? [])].find(
        (b) => b.textContent?.trim() === 'Stopping'
      ) as HTMLElement | undefined;
      expect(button).toBeDefined();
      button!.click();
      await el.updateComplete;
      expect(localStorage.getItem(`scion-filter-project-agents-phase-${projectId}`)).toBe(
        'stopping'
      );
      await vi.waitFor(() => expect(shown()).toEqual(['a-0']));

      sseUpdate('agent.a-1.status', { phase: 'stopping' });
      await flushState();
      await el.updateComplete;
      expect(shown()).toEqual(['a-0', 'a-1']);

      sseUpdate('agent.a-0.status', { phase: 'stopped' });
      await flushState();
      await el.updateComplete;
      expect(shown()).toEqual(['a-1']);
      expect(el.shadowRoot?.querySelectorAll('.agent-card').length).toBe(1);
    });
  });
});
