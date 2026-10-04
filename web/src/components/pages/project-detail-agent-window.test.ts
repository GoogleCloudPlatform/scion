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
 * Covers the small and paged window states: off-page upsert,
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
import { AgentDrainRunner } from '../../client/agent-drain.js';
import { holdable } from './__fixtures__/global-agents-endpoint.js';

// Stop All asks for confirmation first; the tests confirm it.
vi.mock('../shared/confirm-dialog.js', () => ({ showConfirm: vi.fn(async () => true) }));

/**
 * happy-dom has no EventSource; setScope opens one. It opens on the next
 * tick, so a drain's wait for the live connection resolves at once.
 */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
    setTimeout(() => {
      if (this.readyState === FakeEventSource.CLOSED) return;
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }, 0);
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

/** A minimal fake project-agents endpoint implementing enough of the project agents endpoint contract for these tests. */
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

/** Whether the stubbed fetch has been asked for this project's agents list. */
function agentsListRequested(projectId: string): boolean {
  const mock = globalThis.fetch as unknown;
  if (!vi.isMockFunction(mock)) return true;
  return mock.mock.calls.some(([input]) => {
    const raw =
      typeof input === 'string'
        ? input
        : input instanceof URL
          ? input.href
          : (input as Request).url;
    return new URL(raw, 'http://localhost').pathname === `/api/v1/projects/${projectId}/agents`;
  });
}

/**
 * Mounts the page and waits until its first agents request was sent and,
 * unless the test holds that response open (`holdsFirstLoad`), until the
 * page is idle.
 */
async function createComponent(
  projectId: string,
  opts: { holdsFirstLoad?: boolean } = {}
): Promise<TestEl> {
  const el = document.createElement('scion-page-project-detail') as TestEl;
  el.projectId = projectId;
  // Drain retries without a delay.
  (el as unknown as { drainRunner: AgentDrainRunner }).drainRunner = new AgentDrainRunner({
    retryDelayMs: 0,
  });
  el.pageData = {
    path: `/projects/${projectId}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => expect(agentsListRequested(projectId)).toBe(true));
  if (!opts.holdsFirstLoad) await settle(el);
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
  handleStopAll(): Promise<void>;
  onPagerSizeChange(size: number): void;
  committedLabel: string;
  pagerPageSize: number;
  agents: Agent[];
  agentWindow: {
    next(): Promise<void>;
    prev(): Promise<void>;
    refresh(): Promise<void>;
    state: 'small' | 'paged' | 'held' | 'capped';
    items: Agent[];
    display: Agent[];
    pageIndex: number;
    updatesAvailable: boolean;
    error: string | null;
    loading: boolean;
    stale: boolean;
  };
  agentStats: { total: number; running: number };
  agentsLoading: boolean;
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
/** Runs state.ts's pending coalesced flush now, then lets the page re-render. */
async function flushLive(el: TestEl): Promise<void> {
  (stateManager as unknown as { flush(): void }).flush();
  await Promise.resolve();
  await el.updateComplete;
}

/**
 * Waits until no agents load is in flight (page-level or the window's own
 * page fetch), then runs the pending live flush and lets the page render.
 */
async function settle(el: TestEl): Promise<void> {
  await vi.waitFor(() => {
    expect(internals(el).agentsLoading).toBe(false);
    expect(internals(el).agentWindow.loading).toBe(false);
  });
  await flushLive(el);
}

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
  /** Sorted requests over more candidates than this answer 422 (the server's 2,000 candidate ceiling). */
  refuseSortedAbove?: number;
  /** Legacy pages return only the agents passing this (the server's per-item read filter). */
  readable?: (a: Agent) => boolean;
  /** A legacy request with this cursor answers 500. */
  failLegacyCursor?: string;
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
        if (opts.refuseSortedAbove !== undefined && list.length > opts.refuseSortedAbove) {
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
        // complete response is always the whole unphased set.
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

      // Legacy mode: cursor pages of the unsorted list. `legacyTruncated`
      // shrinks a legacy page to 20 items, so a 100-agent drain needs five
      // pages and is capped at four.
      const legacyLimit = opts.legacyTruncated ? 20 : Number(u.searchParams.get('limit') ?? '500');
      if (
        opts.failLegacyCursor !== undefined &&
        u.searchParams.get('cursor') === opts.failLegacyCursor
      ) {
        return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
      }
      const start = Number(u.searchParams.get('cursor') ?? '0');
      const end = start + legacyLimit;
      const readable = opts.readable;
      const pageRows = list.slice(start, end);
      return Promise.resolve(
        jsonResponse({
          agents: readable ? pageRows.filter(readable) : pageRows,
          nextCursor: end < list.length ? String(end) : undefined,
          _capabilities: opts.projectCaps,
        })
      );
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
      await settle(el);
      expect(requests.length).toBe(2);

      // Lifecycle refresh: exactly one request.
      await internals(el).handleAgentAction('a-1', 'stop');
      await settle(el);
      expect(requests.length).toBe(3);
    }, 20_000);

    it('the same page opened in grid view issues exactly one agents request (the fit request), and grid -> list issues zero', async () => {
      // Grid with the updated sort is sorted-eligible, the same as list.
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
      expect(requests[0].url).toContain('sort=updated');

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

    it('100 agents: one request on load; each paged interaction costs exactly one; list <-> grid is free while paged; tree drains once, after which toggles are free', async () => {
      const { el, requests } = await mountList('p-fit-100', 100);
      expect(requests.length).toBe(1);
      expect(requests[0].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(internals(el).agentWindow.state).toBe('paged');

      let n = requests.length;
      await internals(el).agentWindow.next();
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('cursor=');

      n = requests.length;
      await internals(el).agentWindow.prev();
      expect(requests.length - n).toBe(1);

      n = requests.length;
      internals(el).toggleSort('updated'); // flips dir
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('dir=asc');
      expect(internals(el).agentWindow.state).toBe('paged');

      n = requests.length;
      internals(el).setPhaseFilter('running');
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('phase=running');
      n = requests.length;
      internals(el).setPhaseFilter('');
      await settle(el);
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
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('label=env%3Dprod');
      expect(internals(el).agentWindow.state).toBe('paged');

      n = requests.length;
      await internals(el).handleAgentAction('a-1', 'stop');
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain(`fit=${PROJECT_AGENTS_FIT_THRESHOLD}`);
      expect(internals(el).agentWindow.state).toBe('paged');

      // Clear the label so the grid load is unfiltered.
      n = requests.length;
      input.value = '';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(internals(el).agentWindow.state).toBe('paged');

      // list <-> grid while paged: both render the same server page.
      const toggle = viewToggle(el)!;
      n = requests.length;
      for (const v of ['grid', 'list', 'grid']) {
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await settle(el);
      }
      expect(requests.length - n).toBe(0);
      expect(internals(el).agentWindow.state).toBe('paged');
      await el.updateComplete;
      expect(el.shadowRoot!.querySelectorAll('.agent-grid > *').length).toBe(25);

      // grid -> tree: one drain (a single legacy page at 100 agents), then
      // every toggle is free.
      n = requests.length;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'graph' } }));
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).not.toContain('sort=');
      expect(internals(el).agentWindow.state).toBe('small');

      n = requests.length;
      for (const v of ['list', 'grid', 'graph', 'list']) {
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await settle(el);
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
      await settle(el);
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
      await settle(el);
      expect(requests.length).toBe(3);
      expect(requests[2].url).not.toContain('sort=');

      // A new label commit retries sorted mode once.
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
      expect(requests.length).toBe(4);
      expect(requests[3].url).toContain('sort=updated');
      expect(requests[3].url).toContain('label=env%3Dprod');
    });
  });

  describe('label 400 keeps previous data', () => {
    it('a non-OK label-commit response keeps the previously loaded agents', async () => {
      const projectId = 'p-label-400';
      // Name sort is complete-needing: the label commit drains, and the
      // drain's first (legacy) page is the one that fails.
      localStorage.setItem('scion-view-project-agents', 'grid');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
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
      await settle(el);

      const after = (el as unknown as { agents: Agent[] }).agents;
      expect(after.length).toBe(5); // previous data kept, not cleared to []
      expect(requests.filter((r) => r.url.includes('label=')).length).toBe(1); // a 400 is not retried
      expect(internals(el).committedLabel).toBe('');
      expect(internals(el).agentWindow.state).toBe('small');
    });
  });

  describe('paged state: live updates, and reconnect', () => {
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
      await flushLive(el);

      // A status delta for an already-known project agent too.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: agents[0].id, phase: 'stopped' },
      });
      await flushLive(el);

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
      await flushLive(el);

      // A pure phase change off-page, with no active phase filter, does not
      // change which agent belongs on which page — it only affects the
      // live "Running" count, which shows no chip.
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
      await flushLive(el);
      expect(internals(el).agentStats.total).toBe(4);

      // A view-state change (sort direction flip) triggers a fresh
      // `loadAgentsForView` request, which always asks for `stats=1`. The
      // fixture's fetch handler still returns `stats.agents` built from the
      // full, static fixture list — it has no knowledge of the delete,
      // exactly like a REST response that was already in flight, or served
      // from a stale read replica, when the delete happened.
      internals(el).toggleSort('updated');
      await settle(el);
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
      await flushLive(el);

      // A view-state change (sort direction flip) issues a fresh
      // `loadAgentsForView` request, landing in the paged branch again
      // (`fit` is forced to 0). The fixture still lists the already-deleted
      // agent.
      internals(el).toggleSort('updated');
      await settle(el);
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
      await flushLive(el);

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
      await flushLive(el);

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
      await settle(el); // the phase change's own one paged refetch
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
      await flushLive(el);

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(requests.length).toBe(before); // still zero-cost
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
      await settle(el);
      await el.updateComplete;
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentStats.total).toBe(30); // only the 30 env=prod members

      const before = requests.length;
      // The non-member (env=dev, a different project-scope agent) sends a
      // status update. It is not on any page and was never a member, so the
      // off-page add rule (the add rule, mirroring the server's
      // stats population) must not add it.
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: nonMember.id, phase: 'running' },
      });
      await flushLive(el);

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
      await flushLive(el);

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(internals(el).agentWindow.items.find((a) => a.id === agents[0].id)).toBeUndefined();
    });

    it('a reconnect of the live connection raises the chip with no request', async () => {
      const projectId = 'p-paged-resync';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      expect(internals(el).agentWindow.state).toBe('paged');
      const before = requests.length;

      expect(internals(el).agentWindow.updatesAvailable).toBe(false);

      // A drop and reconnect of the live connection, through state.ts's own
      // resync detection.
      const sse = (stateManager as unknown as { sseClientInstance: EventTarget }).sseClientInstance;
      sse.dispatchEvent(new CustomEvent('disconnected'));
      sse.dispatchEvent(new CustomEvent('connected'));
      await settle(el);

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
      await flushLive(el);

      // The small-state list view must see the same live update grid/tree/
      // stats already saw via `this.agents` — no re-adoption step, and no
      // extra request.
      const afterAgents = (el as unknown as { agents: Agent[] }).agents;
      expect(afterAgents.find((a) => a.id === 'a-4')?.phase).toBe('stopped');
      expect(internals(el).agentWindow.items.find((a) => a.id === 'a-4')?.phase).toBe('stopped');
      expect(requests.length).toBe(1);
      // mergeChanged: the array identity changed (a-4 changed),
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
      await flushLive(el);

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
      await flushLive(el);

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
      await flushLive(el);

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
      await flushLive(el);

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
      await flushLive(el);
      expect(internals(el).agentWindow.items.some((a) => a.id === 'a-1')).toBe(false);

      // A lifecycle refresh re-fetches, and the fixture's fetch handler still
      // returns the original fixture list — 'a-1' included — because it has
      // no knowledge of the delete (exactly like a REST response that was
      // already in flight, or served from a stale read replica, when the
      // delete happened). The already-tombstoned ID must not reappear.
      internals(el).backgroundRefresh('lifecycle-refresh');
      await flushLive(el);

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
      await flushLive(el);

      internals(el).backgroundRefresh('lifecycle-refresh');
      await flushLive(el);

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
      await flushLive(el);

      stubSortedAgentsWithoutList(projectId, globalThis.fetch);
      const warn = vi.spyOn(console, 'warn');
      internals(el).backgroundRefresh('lifecycle-refresh');
      await vi.waitFor(() => {
        expect((el as unknown as { agents: Agent[] }).agents).toEqual([]);
      });
      await el.updateComplete;

      expect(warn).not.toHaveBeenCalledWith('Background refresh failed:', expect.anything());
      expect(internals(el).agentWindow.items).toEqual([]);
    });
  });

  describe('view changes that need the complete set drain it once, then cost nothing', () => {
    function stubAlwaysPaged(projectId: string, agents: Agent[], requests: AgentsRequest[]) {
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
          })(u.toString(), init);
        })
      );
    }

    it('sort -> name while paged drains once into the local set; later sort, dir and phase changes cost zero', async () => {
      const projectId = 'p-name-drain';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      stubAlwaysPaged(projectId, agents, requests);

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');

      let n = requests.length;
      internals(el).toggleSort('name');
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).not.toContain('sort=');
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agents.length).toBe(30);

      n = requests.length;
      internals(el).toggleSort('updated');
      internals(el).toggleSort('updated'); // dir flip
      internals(el).toggleSort('created');
      internals(el).setPhaseFilter('running');
      await settle(el);
      expect(requests.length - n).toBe(0);
      expect(internals(el).agentWindow.state).toBe('small');
    });

    it('always paged: a dir flip and a created sort cost one request each, and list <-> grid toggles cost zero', async () => {
      const projectId = 'p-always-paged';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      stubAlwaysPaged(projectId, agents, requests);

      const el = await createComponent(projectId);
      expect(internals(el).agentWindow.state).toBe('paged');

      let n = requests.length;
      internals(el).toggleSort('updated'); // same field: flips dir only
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('sort=updated');
      expect(requests[requests.length - 1].url).toContain('dir=asc');

      n = requests.length;
      internals(el).toggleSort('created');
      await settle(el);
      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('sort=created');
      expect(internals(el).agentWindow.state).toBe('paged');

      const toggle = viewToggle(el)!;
      const perToggleCosts: number[] = [];
      for (const v of ['grid', 'list', 'grid', 'list', 'grid', 'list']) {
        n = requests.length;
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await settle(el);
        perToggleCosts.push(requests.length - n);
      }
      expect(perToggleCosts).toEqual([0, 0, 0, 0, 0, 0]);
      expect(internals(el).agentWindow.state).toBe('paged');
    });
  });

  describe('costs of the earlier sorted-list cut are gone', () => {
    const mountPaged = async (projectId: string, view: 'list' | 'grid') => {
      localStorage.setItem('scion-view-project-agents', view);
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const posts: Array<{ resolve: (v: Response) => void }> = [];
      const inner = createRealisticFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests,
      });
      const fetchSpy = vi.fn((input: string | URL | Request, init?: RequestInit) => {
        if ((init?.method ?? 'GET').toUpperCase() === 'POST') {
          // Lifecycle actions hang, so only the optimistic update can change a row.
          const d = deferred<Response>();
          posts.push(d);
          return d.promise;
        }
        return inner(input, init);
      });
      vi.stubGlobal('fetch', fetchSpy);
      const el = await createComponent(projectId);
      const agentGets = () =>
        fetchSpy.mock.calls.filter(([input]) =>
          String(input).includes(`/api/v1/projects/${projectId}/agents`)
        ).length;
      return { el, requests, agentGets, posts };
    };

    it('paged -> grid issues no request and renders the server page; paged -> tree and paged -> name sort each start one drain', async () => {
      const a = await mountPaged('p-gone-grid', 'list');
      expect(internals(a.el).agentWindow.state).toBe('paged');
      let n = a.agentGets();
      viewToggle(a.el)!.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'grid' } }));
      await settle(a.el);
      await a.el.updateComplete;
      expect(a.agentGets() - n).toBe(0);
      expect(internals(a.el).agentWindow.state).toBe('paged');
      expect(a.el.shadowRoot!.querySelectorAll('.agent-grid > *').length).toBe(25);

      n = a.agentGets();
      viewToggle(a.el)!.dispatchEvent(
        new CustomEvent('view-change', { detail: { view: 'graph' } })
      );
      await settle(a.el);
      expect(a.agentGets() - n).toBe(1);
      expect(a.requests[a.requests.length - 1].url).toContain('limit=500');
      expect(a.requests[a.requests.length - 1].url).not.toContain('sort=');
      expect(internals(a.el).agentWindow.state).toBe('small');
      a.el.remove();
      localStorage.clear();

      const b = await mountPaged('p-gone-name', 'grid');
      expect(internals(b.el).agentWindow.state).toBe('paged');
      n = b.agentGets();
      internals(b.el).toggleSort('name');
      await settle(b.el);
      expect(b.agentGets() - n).toBe(1);
      expect(b.requests[b.requests.length - 1].url).not.toContain('sort=');
      expect(internals(b.el).agentWindow.state).toBe('small');
    });

    it('a grid page load followed by a switch to list issues no extra request', async () => {
      const { el, requests, agentGets } = await mountPaged('p-gone-fit', 'grid');
      expect(agentGets()).toBe(1);
      expect(requests[0].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('paged');

      viewToggle(el)!.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await settle(el);
      expect(agentGets()).toBe(1);
      expect(internals(el).agentWindow.state).toBe('paged');
    });

    it('an optimistic lifecycle update applies to the paged row before the server answers, with no agents request', async () => {
      const { el, agentGets, posts } = await mountPaged('p-gone-optimistic', 'list');
      expect(internals(el).agentWindow.state).toBe('paged');
      const target = internals(el).agentWindow.items[3];
      expect(target.phase).toBe('running');

      const n = agentGets();
      void internals(el).handleAgentAction(target.id, 'stop');
      await settle(el);
      expect(posts.length).toBe(1); // still in flight
      expect(agentGets() - n).toBe(0);
      const row = internals(el).agentWindow.items.find((a) => a.id === target.id);
      expect(row?.phase).toBe('stopping');
      expect(internals(el).agentWindow.items[3].id).toBe(target.id); // stays in place
      expect(internals(el).agents).toEqual([]); // the page-level set stays empty while paged
    });
  });

  describe('request counts per interaction, one test per agent count', () => {
    type Check = (el: TestEl, agents: Agent[]) => void;
    type Step = [label: string, run: (el: TestEl) => void | Promise<void>, check?: Check];
    const commitLabel = (el: TestEl, value: string) => {
      const input = labelInput(el)!;
      input.value = value;
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
    };
    const clearLabel = (el: TestEl) => {
      const input = labelInput(el)!;
      input.value = '';
      input.dispatchEvent(new Event('sl-clear'));
    };
    const setView = (el: TestEl, view: string) =>
      viewToggle(el)!.dispatchEvent(new CustomEvent('view-change', { detail: { view } }));
    const stopAll = async (el: TestEl) => {
      await internals(el).handleStopAll();
    };
    const reconnect = (el: TestEl) => {
      void el;
      const sse = (stateManager as unknown as { sseClientInstance: EventTarget }).sseClientInstance;
      sse.dispatchEvent(new CustomEvent('disconnected'));
      sse.dispatchEvent(new CustomEvent('connected'));
    };
    /**
     * The rendered page holds only agents passing `keep`, and as many as
     * fit on the page: a filter that was ignored would leave other agents
     * on it, and one applied twice would leave too few.
     */
    const rowsMatch =
      (keep: (a: Agent) => boolean): Check =>
      (el, agents) => {
        const w = internals(el).agentWindow;
        const items = w.items;
        // A capped set holds only what the drain loaded.
        const pool = w.state === 'capped' ? internals(el).agents : agents;
        const expected = pool.filter(keep).length;
        expect(items.every(keep)).toBe(true);
        expect(items.length).toBe(Math.min(internals(el).pagerPageSize, expected));
        const rows = el.shadowRoot!.querySelectorAll('.agent-table-container tbody tr');
        expect(rows.length).toBe(items.length);
      };
    const onPage =
      (index: number): Check =>
      (el) =>
        expect(internals(el).agentWindow.pageIndex).toBe(index);
    const signalsReconnect: Check = (el) => {
      const w = internals(el).agentWindow;
      if (w.state === 'paged') {
        expect(w.updatesAvailable).toBe(true);
      } else if (w.state === 'capped') {
        // The capped banner wins over the stale one; the stale flag is still set.
        expect(w.stale).toBe(true);
        expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
          'more exist'
        );
      } else {
        expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
          'may be stale'
        );
      }
    };
    // Each step is one interaction of the request-count table: a view
    // switch, a sort or dir change, a phase change, page navigation, a page
    // size change, label typing, a label commit or clear (sl-change or
    // sl-clear), a lifecycle or stop-all refresh, a reconnect, and the chip.
    const steps: Step[] = [
      ['switch to grid view', (el) => setView(el, 'grid')],
      ['switch to list view', (el) => setView(el, 'list'), rowsMatch(() => true)],
      ['flip the updated sort direction', (el) => internals(el).toggleSort('updated')],
      ['sort by created', (el) => internals(el).toggleSort('created')],
      ['sort by updated after created', (el) => internals(el).toggleSort('updated')],
      [
        'filter phase running',
        (el) => internals(el).setPhaseFilter('running'),
        rowsMatch((a) => a.phase === 'running'),
      ],
      ['clear the phase filter', (el) => internals(el).setPhaseFilter(''), rowsMatch(() => true)],
      [
        'next page',
        (el) => internals(el).agentWindow.next(),
        (el, agents) => onPage(agents.length > 25 ? 1 : 0)(el, agents),
      ],
      ['previous page', (el) => internals(el).agentWindow.prev(), onPage(0)],
      ['page size 50', (el) => internals(el).onPagerSizeChange(50), rowsMatch(() => true)],
      ['page size back to 25', (el) => internals(el).onPagerSizeChange(25), rowsMatch(() => true)],
      [
        'type a label without committing it',
        (el) => {
          const input = labelInput(el)!;
          input.value = 'env=pr';
          input.dispatchEvent(new Event('sl-input'));
        },
      ],
      [
        'commit label env=prod',
        (el) => commitLabel(el, 'env=prod'),
        rowsMatch((a) => a.labels?.env === 'prod'),
      ],
      ['clear label env=prod with sl-clear', (el) => clearLabel(el), rowsMatch(() => true)],
      [
        'commit bare-key label team',
        (el) => commitLabel(el, 'team'),
        rowsMatch((a) => a.labels?.team !== undefined),
      ],
      ['clear bare-key label team with sl-clear', (el) => clearLabel(el), rowsMatch(() => true)],
      ['lifecycle refresh', (el) => internals(el).backgroundRefresh('lifecycle-refresh')],
      ['stop-all refresh', stopAll],
      ['live connection reconnect', reconnect, signalsReconnect],
      ['switch to tree view', (el) => setView(el, 'graph')],
      ['switch from tree back to list view', (el) => setView(el, 'list')],
      ['sort by name', (el) => internals(el).toggleSort('name')],
      ['sort by updated after the name sort', (el) => internals(el).toggleSort('updated')],
      [
        'flip the updated sort direction after the tree',
        (el) => internals(el).toggleSort('updated'),
      ],
      [
        'filter phase running after the tree',
        (el) => internals(el).setPhaseFilter('running'),
        rowsMatch((a) => a.phase === 'running'),
      ],
      ['next page after the tree', (el) => internals(el).agentWindow.next()],
      [
        'lifecycle refresh after the tree',
        (el) => internals(el).backgroundRefresh('lifecycle-refresh'),
      ],
      ['stop-all refresh after the tree', stopAll],
      ['counts chip click', (el) => internals(el).backgroundRefresh('chip')],
    ];

    /** Every third agent stopped; even agents env=prod, odd env=dev; every fifth also has a bare `team` key. */
    const mixedAgents = (count: number, projectId: string): Agent[] =>
      Array.from({ length: count }, (_, i) =>
        makeAgent(i, {
          projectId,
          phase: i % 3 === 0 ? 'stopped' : 'running',
          labels: {
            env: i % 2 === 0 ? 'prod' : 'dev',
            ...(i % 5 === 0 ? { team: 'core' } : {}),
          },
        })
      );

    const cases: Array<{
      name: string;
      count: number;
      costs: number[];
      states: string[];
      load?: { requests: number; state: string; banner: string };
    }> = [
      {
        name: '25 agents (complete, so small): only commits and refreshes send a request',
        count: 25,
        // prettier-ignore
        costs: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1],
        // prettier-ignore
        states: [
          'small', 'small', 'small', 'small', 'small', 'small', 'small', 'small',
          'small', 'small', 'small', 'small', 'small', 'small', 'small', 'small',
          'small', 'small', 'small', 'small', 'small', 'small', 'small', 'small',
          'small', 'small', 'small', 'small', 'small',
        ],
      },
      {
        name: '60 agents at page size 25 (above the 50-agent fit threshold, so paged): next and prev are real page requests',
        count: 60,
        // prettier-ignore
        costs: [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 1, 0, 1, 0, 0, 0, 0, 0, 0, 1, 1, 1],
        // prettier-ignore
        states: [
          'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged',
          'paged', 'paged', 'paged', 'paged', 'small', 'paged', 'small', 'paged',
          'paged', 'paged', 'paged', 'small', 'small', 'small', 'small', 'small',
          'small', 'small', 'paged', 'paged', 'paged',
        ],
      },
      {
        name: '500 agents (above the 50-agent fit threshold, so paged): each server-side change costs one request',
        count: 500,
        // prettier-ignore
        costs: [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 1, 0, 1, 0, 0, 0, 0, 0, 0, 1, 1, 1],
        // prettier-ignore
        states: [
          'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged',
          'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'small', 'paged',
          'paged', 'paged', 'paged', 'small', 'small', 'small', 'small', 'small',
          'small', 'small', 'paged', 'paged', 'paged',
        ],
      },
      {
        name: '1,200 agents (paged; the tree drains three pages into a held set, after which changes are free)',
        count: 1200,
        // prettier-ignore
        costs: [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 3, 1, 1, 1, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 3],
        // prettier-ignore
        states: [
          'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'paged',
          'paged', 'paged', 'paged', 'paged', 'paged', 'paged', 'held', 'paged',
          'paged', 'paged', 'paged', 'held', 'held', 'held', 'held', 'held',
          'held', 'held', 'held', 'held', 'held',
        ],
      },
      {
        name: '2,001 agents (above the 2,000 candidate ceiling, so sorted requests are refused and the set is a capped drain; a k=v label that narrows the set pages again)',
        count: 2001,
        // prettier-ignore
        costs: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 4, 4, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4],
        // prettier-ignore
        states: [
          'capped', 'capped', 'capped', 'capped', 'capped', 'capped', 'capped', 'capped',
          'capped', 'capped', 'capped', 'capped', 'paged', 'capped', 'capped', 'capped',
          'capped', 'capped', 'capped', 'capped', 'capped', 'capped', 'capped', 'capped',
          'capped', 'capped', 'capped', 'capped', 'capped',
        ],
        load: {
          requests: 5, // the refused sorted request, then four drain pages
          state: 'capped',
          banner: '2,000 loaded (newest 2,000 checked), more exist',
        },
      },
    ];

    for (const c of cases) {
      it(
        c.name,
        async () => {
          const projectId = `p-counts-${c.count}`;
          localStorage.setItem('scion-view-project-agents', 'list');
          const agents = mixedAgents(c.count, projectId);
          const requests: AgentsRequest[] = [];
          vi.stubGlobal(
            'fetch',
            vi.fn(
              createRealisticFetchHandler({
                projectId,
                projectCaps: { actions: ['read', 'stop_all'] },
                agents,
                requests,
                refuseSortedAbove: 2000,
              })
            )
          );
          const el = await createComponent(projectId);
          await settle(el);
          expect(requests[0].url).toContain('sort=updated');
          if (c.load) {
            expect(requests.length).toBe(c.load.requests);
            expect(internals(el).agentWindow.state).toBe(c.load.state);
            expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
              c.load.banner
            );
          } else {
            expect(requests.length).toBe(1);
            expect(internals(el).agentWindow.state).toBe(
              c.count > PROJECT_AGENTS_FIT_THRESHOLD ? 'paged' : 'small'
            );
          }

          const costs: number[] = [];
          const states: string[] = [];
          for (const [name, run, check] of steps) {
            const n = requests.length;
            await run(el);
            await settle(el);
            costs.push(requests.length - n);
            states.push(internals(el).agentWindow.state);
            if (check) {
              try {
                check(el, agents);
              } catch (err) {
                throw new Error(`step "${name}": ${(err as Error).message}`);
              }
            }
          }
          const named = (xs: Array<number | string>) =>
            steps.map(([name], i) => `${name}: ${xs[i]}`);
          expect(named(costs)).toEqual(named(c.costs));
          expect(named(states)).toEqual(named(c.states));
        },
        60_000
      );
    }
  });

  describe('a drain that hits its request cap', () => {
    it('is capped: the tree and list show the banner, the list pager shows the capped total, lifecycle refresh is free and Refresh drains again', async () => {
      const projectId = 'p-capped';
      localStorage.setItem('scion-view-project-agents', 'graph');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true, // 20 per page: four pages leave more behind
          })
        )
      );
      const el = await createComponent(projectId);
      await vi.waitFor(() => expect(internals(el).agentWindow.state).toBe('capped'));
      await el.updateComplete;
      expect(requests.length).toBe(4);
      expect(requests.every((r) => !r.url.includes('sort='))).toBe(true);
      expect(internals(el).agents.length).toBe(80);
      const banner = el.shadowRoot!.querySelector('.agent-window-banner');
      expect(banner?.textContent).toContain('80 loaded (newest 2,000 checked), more exist');

      let n = requests.length;
      internals(el).backgroundRefresh('lifecycle-refresh');
      await settle(el);
      expect(requests.length - n).toBe(0);

      // Name sort is not sorted-eligible, so the list view stays capped.
      viewToggle(el)!.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await settle(el);
      await el.updateComplete;
      expect(requests.length - n).toBe(0);
      expect(internals(el).agentWindow.state).toBe('capped');
      expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
        '80 loaded (newest 2,000 checked), more exist'
      );
      const pg = pager(el)!;
      await (pg as unknown as { updateComplete: Promise<boolean> }).updateComplete;
      expect(pg.shadowRoot?.textContent).toContain('80 loaded (newest 2,000 checked), more exist');

      viewToggle(el)!.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'graph' } }));
      await el.updateComplete;
      n = requests.length;
      const refresh = el.shadowRoot!.querySelector('.agent-window-banner sl-tag') as HTMLElement;
      refresh.click();
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false));
      await settle(el);
      expect(requests.length - n).toBe(4);
      expect(internals(el).agentWindow.state).toBe('capped');
    });
  });

  describe('a capped set shows the banner in every view rendered from it', () => {
    async function mountCapped(
      projectId: string,
      view: string,
      sortField: string
    ): Promise<{ el: TestEl; requests: AgentsRequest[] }> {
      localStorage.setItem('scion-view-project-agents', view);
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: sortField, dir: 'asc' })
      );
      const agents = Array.from({ length: 100 }, (_, i) =>
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
            legacyTruncated: true,
          })
        )
      );
      const el = await createComponent(projectId);
      return { el, requests };
    }

    const bannerText = (el: TestEl) =>
      el.shadowRoot!.querySelector('.agent-window-banner')?.textContent ?? '';

    for (const [view, sortField] of [
      ['grid', 'name'],
      ['list', 'name'],
      ['list', 'status'],
      ['graph', 'updated'],
    ] as const) {
      it(`${view} view with ${sortField} sort`, async () => {
        const { el } = await mountCapped(`p-capped-${view}-${sortField}`, view, sortField);
        await vi.waitFor(() => expect(internals(el).agentWindow.state).toBe('capped'));
        await el.updateComplete;
        expect(bannerText(el)).toContain('80 loaded (newest 2,000 checked), more exist');
      });
    }

    it('a bare-key label in the list view with updated sort', async () => {
      const { el, requests } = await mountCapped('p-capped-bare-key', 'list', 'updated');
      // Sorted-eligible: the first request is the fit request, and it pages.
      expect(requests[0].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(bannerText(el)).toBe('');

      const input = labelInput(el)!;
      input.value = 'env';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await vi.waitFor(() => expect(internals(el).agentWindow.state).toBe('capped'));
      await el.updateComplete;
      expect(bannerText(el)).toContain('80 loaded (newest 2,000 checked), more exist');
    });
  });

  describe('a live create while a seeding request is in flight is never lost', () => {
    const emitCreate = (projectId: string, id: string) =>
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: id,
          id,
          name: id,
          projectId,
          template: 't',
          phase: 'running',
          created: '2026-03-01T00:00:00Z',
          updated: '2026-03-01T00:00:00Z',
          messageMode: 'project',
        },
      });

    /**
     * Mounts the page with agents GETs answered by the realistic handler.
     * The first agents GET (and the next one after each `hold()`) computes
     * its response at once, so the server snapshot predates any create the
     * test emits, and returns it only when the test calls `release()`.
     */
    async function mountHeld(projectId: string, view: string, sortField: string, count: number) {
      localStorage.setItem('scion-view-project-agents', view);
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: sortField, dir: 'desc' })
      );
      const agents = Array.from({ length: count }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const inner = createRealisticFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests,
      });
      let armed = true;
      let held: { url: string; gate: ReturnType<typeof deferred<void>> } | null = null;
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
          const url =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const res = await inner(input, init);
          const isAgentsGet =
            new URL(url, 'http://localhost').pathname === `/api/v1/projects/${projectId}/agents`;
          if (armed && isAgentsGet) {
            armed = false;
            const gate = deferred<void>();
            held = { url, gate };
            await gate.promise;
          }
          return res;
        })
      );
      const el = await createComponent(projectId, { holdsFirstLoad: true });
      const waitHeld = async (): Promise<string> => {
        await vi.waitFor(() => expect(held).not.toBeNull());
        return held!.url;
      };
      const release = async (): Promise<void> => {
        const h = held!;
        held = null;
        h.gate.resolve();
        await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false));
        await settle(el);
        await el.updateComplete;
      };
      const hold = (): void => {
        armed = true;
      };
      return { el, requests, waitHeld, release, hold };
    }

    const flushSse = async (el: TestEl) => {
      await flushLive(el);
    };

    it('the fit request answering complete: the create joins the small set', async () => {
      const projectId = 'p-live-fit-small';
      const m = await mountHeld(projectId, 'list', 'updated', 10);
      expect(await m.waitHeld()).toContain('fit=');

      emitCreate(projectId, 'a-live');
      await flushSse(m.el);
      await m.release();

      expect(internals(m.el).agentWindow.state).toBe('small');
      expect(internals(m.el).agents.map((a) => a.id)).toContain('a-live');
      expect(internals(m.el).agentWindow.items[0].id).toBe('a-live');
      expect(internals(m.el).agentStats.total).toBe(11);
    });

    it('the fit request answering paged: the create joins the member index and raises the chip', async () => {
      const projectId = 'p-live-fit-paged';
      const m = await mountHeld(projectId, 'list', 'updated', 60);
      expect(await m.waitHeld()).toContain('fit=');

      emitCreate(projectId, 'a-live');
      await flushSse(m.el);
      await m.release();

      expect(internals(m.el).agentWindow.state).toBe('paged');
      expect(internals(m.el).agentStats.total).toBe(61);
      expect(internals(m.el).agentWindow.updatesAvailable).toBe(true);
      expect(m.requests.length).toBe(1);
    });

    it('above the fit threshold: an off-page phase change while the paged fit request is in flight reaches the running count', async () => {
      const projectId = 'p-live-fit-unknown';
      const m = await mountHeld(projectId, 'list', 'updated', 60);
      expect(await m.waitHeld()).toContain('fit=');
      // The oldest agent sorts last, off page 0, and is not in the store yet.
      expect(stateManager.getAgent('a-0')).toBeUndefined();

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: 'a-0', phase: 'stopped' },
      });
      await flushSse(m.el);
      await m.release();

      expect(internals(m.el).agentWindow.state).toBe('paged');
      expect(internals(m.el).agentWindow.items.some((a) => a.id === 'a-0')).toBe(false);
      expect(internals(m.el).agentStats).toMatchObject({ total: 60, running: 59 });
      expect(m.requests.length).toBe(1);
    });

    it('the legacy first request: the create joins the drained set', async () => {
      const projectId = 'p-live-legacy';
      const m = await mountHeld(projectId, 'list', 'name', 10);
      expect(await m.waitHeld()).not.toContain('sort=');

      emitCreate(projectId, 'a-live');
      await flushSse(m.el);
      await m.release();

      expect(internals(m.el).agentWindow.state).toBe('small');
      expect(internals(m.el).agents.map((a) => a.id)).toContain('a-live');
      expect(internals(m.el).agentStats.total).toBe(11);
    });

    it('a paged page request with stats: the create survives the member index re-seed', async () => {
      const projectId = 'p-live-page';
      const m = await mountHeld(projectId, 'list', 'updated', 60);
      await m.waitHeld();
      await m.release();
      expect(internals(m.el).agentWindow.state).toBe('paged');
      expect(internals(m.el).agentStats.total).toBe(60);

      // The paged chip refetches page 0 with stats=1.
      m.hold();
      const refreshed = internals(m.el).agentWindow.refresh();
      expect(await m.waitHeld()).toContain('stats=1');
      emitCreate(projectId, 'a-live');
      await flushSse(m.el);
      await m.release();
      await refreshed;
      await m.el.updateComplete;

      expect(internals(m.el).agentStats.total).toBe(61);
      expect(internals(m.el).agentWindow.updatesAvailable).toBe(true);
    });

    it('a live status change during a paged page request is reflected in the adopted page', async () => {
      const projectId = 'p-live-page-update';
      const m = await mountHeld(projectId, 'list', 'updated', 60);
      await m.waitHeld();
      await m.release();
      const last = internals(m.el).agentWindow.items.at(-1)!;

      m.hold();
      const refreshed = internals(m.el).agentWindow.refresh();
      await m.waitHeld();
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: last.id, phase: 'stopped' },
      });
      await flushSse(m.el);
      await m.release();
      await refreshed;

      const row = internals(m.el).agentWindow.items.find((a) => a.id === last.id);
      expect(row?.phase).toBe('stopped');
    });
  });

  describe('label typing while paged', () => {
    it('does not reset pageIndex or issue a request, and DOES apply the live preview filter to the page', async () => {
      const projectId = 'p-typing';
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
      await settle(el);
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
      input.value = 'env'; // bare key: not sorted-eligible, goes through the legacy path
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('small'); // legacy load, no nextCursor
      expect(internals(el).agentWindow.pageIndex).toBe(0); // reset
    });
  });

  describe('persisted page size', () => {
    it('is read once in connectedCallback, used for the first request, and matches the rendered pager', async () => {
      const projectId = 'p-window-1';
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
      const projectId = 'p-invalid';
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
      const projectId = 'p-window-2';
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
      await vi.waitFor(() => expect(pending[2]).toBeDefined());
      internals(el).backgroundRefresh('lifecycle-refresh');
      await vi.waitFor(() => expect(pending[3]).toBeDefined());

      // Resolve the NEWER one first.
      pending[3].resolve(
        jsonResponse({
          agents: secondTriggerAgents,
          totalCount: 1,
          complete: true,
          stats: { total: 1, running: 1, agents: [[secondTriggerAgents[0].id, 'running']] },
        })
      );
      await vi.waitFor(() =>
        expect((el as unknown as { agents: Agent[] }).agents.map((a) => a.name)).toEqual([
          'second-trigger',
        ])
      );

      // Resolve the OLDER one afterward — it must be discarded.
      pending[2].resolve(
        jsonResponse({
          agents: firstTriggerAgents,
          totalCount: 1,
          complete: true,
          stats: { total: 1, running: 1, agents: [[firstTriggerAgents[0].id, 'running']] },
        })
      );
      await settle(el);
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
     * cursor to new params and the server would legitimately 400 it. The fix is to disable navigation (and the chip) outright while
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
      const projectId = 'p-window-3';
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
      await vi.waitFor(() => expect(heldResolve).not.toBeNull());
      await el.updateComplete;
      expect(pagerLoading(el)).toBe(true); // the pager is disabled during the gap

      const requestsBeforeClick = requests.length;
      clickNext(el); // the pager's own guard makes this a no-op
      await flushLive(el);
      expect(internals(el).agentWindow.pageIndex).toBe(0); // never navigated
      expect(requests.length).toBe(requestsBeforeClick); // no mismatched-cursor request

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
      await settle(el);
      await el.updateComplete;

      expect(internals(el).agentWindow.pageIndex).toBe(0); // lands on page 0, as the refresh intended
      expect(pagerLoading(el)).toBe(false); // re-enabled
      // Next now works normally.
      clickNext(el);
      await settle(el);
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(1);
    });

    it('small-state local Next/Prev stay enabled, and work, during an in-flight lifecycle refresh', async () => {
      // Small-state pagination is a pure local slice of `display` and sends no request, so it has nothing to race with a
      // page-level load — the `.loading` gate (and `onPagerNav`'s guard)
      // only apply the page-level half in the paged state, so this isn't
      // disabled for the duration of every agent action.
      const projectId = 'p-prime-small';
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
      await vi.waitFor(() => expect(heldResolve).not.toBeNull());
      await el.updateComplete;
      expect(pagerLoading(el)).toBe(false); // unlike the paged state, the gate does not fire here
      expect(requests.length).toBe(requestsBeforeRefresh + 1); // the held refresh request was sent

      clickNext(el); // the pager's own real guard — must not be disabled
      await flushLive(el);
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
      await settle(el);
      await el.updateComplete;

      expect(internals(el).agentWindow.state).toBe('small');
      expect(pagerLoading(el)).toBe(false);
      expect(internals(el).agentWindow.pageIndex).toBe(1); // small -> small: unaffected by the refresh
      // Paging continues to work normally once the refresh has landed.
      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onPrev(): void;
      };
      pg.onPrev();
      await settle(el);
      await el.updateComplete;
      expect(internals(el).agentWindow.pageIndex).toBe(0);
      clickNext(el);
      await settle(el);
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
        await vi.waitFor(() => expect(heldResolve).not.toBeNull());
        await el.updateComplete;
        expect(pagerLoading(el)).toBe(true);

        const requestsBeforeClick = requests.length;
        clickNext(el);
        await flushLive(el);
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
        await settle(el);
        await el.updateComplete;

        expect(internals(el).agentWindow.pageIndex).toBe(0); // the trigger's own page 0 landed
        expect(internals(el).agentWindow.error).toBeNull();
      });
    }
  });

  describe("a failed view-change request while paged invalidates cursors, so Next can't replay a stale one", () => {
    it('a phase change that 500s leaves the window paged with no error; Next then no-ops instead of sending a mismatched cursor', async () => {
      const projectId = 'p-triple-prime';
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
      await settle(el);
      await el.updateComplete;

      // The failed view-change request keeps the previous page
      // but must no longer claim Next is possible: the stored cursor was
      // minted under the old (unfiltered) phase, and a Next now would bind
      // it to `phase=stopped` and get a 400.
      expect(internals(el).agentWindow.error).toBeNull(); // the failure itself is silent (previous data kept)
      expect(internals(el).agentWindow.hasNext).toBe(false); // invalidated

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext(); // the pager's own guard refuses: hasNext is false
      await settle(el);
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request was ever sent
      expect(internals(el).agentWindow.error).toBeNull(); // in particular, no 400
      expect(internals(el).agentWindow.pageIndex).toBe(0);

      // A subsequent successful view-change restores navigation via
      // setPaged — this is a *different* phase, not a retry of 'stopped',
      // because any successful view-change restores it, not just a retry
      // of the one that failed.
      internals(el).setPhaseFilter('running');
      await settle(el);
      await el.updateComplete;
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.hasNext).toBe(true);
    });

    it('a phase change that fails with a NETWORK error also invalidates cursors', async () => {
      const projectId = 'p-quad-prime-net';
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
      await settle(el);
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
      await settle(el);
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();
    });

    it('a phase change whose response is ok but json() rejects also invalidates cursors, with no unhandled rejection', async () => {
      const projectId = 'p-sorted-parse-error';
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
      await settle(el);
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
      await settle(el);
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();

      // A subsequent successful view-change still restores navigation, so
      // the invalidation above was not a permanent, leaked failure state.
      internals(el).setPhaseFilter('running');
      await settle(el);
      await el.updateComplete;
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.hasNext).toBe(true);
    });

    it('a view-change that 422s, whose drain fallback also fails, invalidates cursors', async () => {
      const projectId = 'p-quad-prime-legacy';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let fail422NextSorted = false;
      let failLegacy = false;
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
            } else if (failLegacy) {
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
      // the drain fallback it triggers then itself fails (500) — the
      // window must still be left with invalidated navigation, not a stale
      // cursor armed under the old (pre-change) params.
      fail422NextSorted = true;
      failLegacy = true; // every attempt, retries included
      const before = requests.length;
      internals(el).setPhaseFilter('stopped');
      // The drain retries a failed page twice (250 ms, then 500 ms).
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false), { timeout: 3000 });
      await el.updateComplete;
      expect(requests.length - before).toBe(1 + 3); // the 422, then three drain attempts

      expect(internals(el).agentWindow.state).toBe('paged'); // the drain failure doesn't change window state
      expect(internals(el).agentWindow.hasNext).toBe(false);
      expect(internals(el).agentWindow.hasPrev).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await settle(el);
      await el.updateComplete;
      expect(requests.length).toBe(requestsBeforeNext); // no mismatched-cursor request
      expect(internals(el).agentWindow.error).toBeNull();
    });

    it('a view-change that 422s, whose drain fallback rejects with a NETWORK error, also invalidates cursors', async () => {
      const projectId = 'p-legacy-network-error';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      let fail422NextSorted = false;
      let failLegacy = false;
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
            } else if (failLegacy) {
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

      // Same as the 500 case above, but the drain's fetch itself rejects
      // rather than resolving with a non-OK response.
      fail422NextSorted = true;
      failLegacy = true; // every attempt, retries included
      const before = requests.length;
      internals(el).setPhaseFilter('stopped');
      // The drain retries a failed page twice (250 ms, then 500 ms).
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false), { timeout: 3000 });
      await el.updateComplete;
      expect(requests.length - before).toBe(1 + 3); // the 422, then three drain attempts

      expect(internals(el).agentWindow.state).toBe('paged');
      expect(internals(el).agentWindow.hasNext).toBe(false);
      expect(internals(el).agentWindow.hasPrev).toBe(false);

      const pg = el.shadowRoot!.querySelector('scion-agent-pager') as unknown as {
        onNext(): void;
      };
      const requestsBeforeNext = requests.length;
      pg.onNext();
      await settle(el);
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
      // the OLD (pre-commit) label and get a 400.
      failNextSorted = true;
      const input = labelInput(el)!;
      input.value = 'env=dev';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
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
      await settle(el);
      await el.updateComplete;
      // Exactly one legitimate request, bound to the reverted (empty)
      // label, with no 400: this is what reverting committedLabel on a
      // network error (not just a non-OK response) prevents from
      // mismatching.
      expect(requests.length).toBe(requestsBeforeNext + 1);
      expect(internals(el).agentWindow.error).toBeNull();
      expect(internals(el).agentWindow.pageIndex).toBe(1);
    });

    it('a bare-key label commit whose drain response is ok but json() rejects reverts committedLabel, with no unhandled rejection', async () => {
      const projectId = 'p-legacy-parse-error-label-commit';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 5 }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: 'dev' } })
      );
      const requests: AgentsRequest[] = [];
      let failLegacy = false;
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (
            failLegacy &&
            u.pathname === `/api/v1/projects/${projectId}/agents` &&
            !u.searchParams.get('sort')
          ) {
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
      expect(internals(el).agentWindow.state).toBe('small'); // sorted-eligible, complete (5 is below the fit threshold)
      expect(internals(el).committedLabel).toBe('');

      // First, a successful commit on the sorted path, so the
      // revert below has a non-trivial value to land back on rather than
      // coincidentally landing on the initial empty label.
      const input = labelInput(el)!;
      input.value = 'env=dev';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
      await el.updateComplete;
      expect(internals(el).committedLabel).toBe('env=dev');
      const agentsAfterFirstCommit = internals(el).agents;

      // A label with no "=" is not sorted-eligible, so this commit drains;
      // every attempt (the drain retries an unparseable body twice) fails.
      failLegacy = true;
      const requestsBeforeFailingCommit = requests.length;
      input.value = 'badlabel';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false), { timeout: 3000 });
      await el.updateComplete;

      expect(requests.length).toBe(requestsBeforeFailingCommit + 3); // one drain page, retried twice
      expect(internals(el).committedLabel).toBe('env=dev'); // reverted to the prior commit, not left at 'badlabel'
      expect(internals(el).agents).toBe(agentsAfterFirstCommit); // previous rows kept
    });
  });

  describe('fit-path label 400 restores the previous committedLabel', () => {
    it('a 400 on the sorted (fit) path keeps the previous data and reverts committedLabel', async () => {
      const projectId = 'p-fit-400';
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
      expect(internals(el).agentWindow.state).toBe('small'); // sorted-eligible, complete (5 is below the fit threshold)
      const before = (el as unknown as { agents: Agent[] }).agents;
      expect(before.length).toBe(5);
      expect(internals(el).committedLabel).toBe('');

      failLabelOnFitPath = true;
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);

      // The sorted (fit) request, not the legacy path, received the 400.
      expect(requests.some((r) => r.url.includes('sort=') && r.url.includes('label=env'))).toBe(
        true
      );
      const after = (el as unknown as { agents: Agent[] }).agents;
      expect(after.length).toBe(5); // previous data kept
      expect(internals(el).committedLabel).toBe(''); // reverted, not left at the rejected label
    });
  });

  describe('a loading indicator replaces the empty-filter message while a drain is in flight', () => {
    it('paged -> name sort shows "Loading agents…" instead of the server page while the drain is in flight, then the sorted list', async () => {
      const projectId = 'p-loading';
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
      internals(el).toggleSort('name');
      await vi.waitFor(() => expect(heldResolve).not.toBeNull());
      await el.updateComplete;

      expect(el.shadowRoot?.textContent).toContain('Loading agents');
      expect(el.shadowRoot?.textContent).not.toContain('No agents match the current filter');

      heldResolve!(jsonResponse({ agents, _capabilities: { actions: ['read'] } }));
      await settle(el);
      await el.updateComplete;

      expect(el.shadowRoot?.textContent).not.toContain('Loading agents');
      expect(internals(el).agentWindow.state).toBe('small');
      expect(el.shadowRoot?.textContent).toContain('agent-0');
    });

    it('a lifecycle refresh in grid with a phase filter matching nothing shows the filter-empty message, not a loading flicker', async () => {
      const projectId = 'p-window-4';
      // Name sort is complete-needing: this.agents is populated by a drain.
      localStorage.setItem('scion-view-project-agents', 'grid');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
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
      await vi.waitFor(() => expect(heldResolve).not.toBeNull());
      await el.updateComplete;

      // Must NOT flicker to "Loading agents…" here — this.agents already
      // has data, it's just phase-filtered to nothing.
      expect(el.shadowRoot?.textContent).not.toContain('Loading agents');
      expect(el.shadowRoot?.textContent).toContain('No agents match the current filter');

      heldResolve!(jsonResponse({ agents, _capabilities: { actions: ['read'] } }));
      await settle(el);
      await el.updateComplete;
      expect(el.shadowRoot?.textContent).toContain('No agents match the current filter');
    });
  });

  describe('a view change while a request is in flight', () => {
    const isProjectAgents = (projectId: string) => (u: URL) =>
      u.pathname === `/api/v1/projects/${projectId}/agents`;

    function setup(projectId: string, count: number) {
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: count }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 3 === 0 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      const h = holdable(
        createRealisticFetchHandler({
          projectId,
          projectCaps: { actions: ['read'] },
          agents,
          requests,
        }),
        isProjectAgents(projectId)
      );
      vi.stubGlobal('fetch', vi.fn(h.fn));
      return { agents, h };
    }

    const idle = async (el: TestEl) => {
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false));
      await settle(el);
      await el.updateComplete;
    };

    for (const change of ['phase', 'dir'] as const) {
      it(`a ${change} change during the first load supersedes it and loads the new view`, async () => {
        const projectId = `p-first-${change}`;
        const { agents, h } = setup(projectId, 60);
        h.hold();
        const el = await createComponent(projectId, { holdsFirstLoad: true });
        expect(h.sent).toHaveLength(1);
        if (change === 'phase') internals(el).setPhaseFilter('stopped');
        else internals(el).toggleSort('updated');
        h.release();
        await idle(el);

        expect(h.sent[0].signal?.aborted).toBe(true);
        expect(h.sent).toHaveLength(2);
        const q = new URL(h.sent[1].url, 'http://x').searchParams;
        if (change === 'phase') expect(q.get('phase')).toBe('stopped');
        else expect(q.get('dir')).toBe('asc');
        const win = internals(el).agentWindow;
        expect(win.state).toBe('paged');
        const dir = change === 'dir' ? 1 : -1;
        const expected = agents
          .filter((a) => change !== 'phase' || a.phase === 'stopped')
          .sort((a, b) => dir * (a.updated ?? '').localeCompare(b.updated ?? ''))
          .slice(0, 25)
          .map((a) => a.id);
        expect(win.items.map((a) => a.id)).toEqual(expected);
      });
    }

    it('A to B to A while paged: the B request is aborted and the A page stays, with no request', async () => {
      const projectId = 'p-a-b-a';
      const { h } = setup(projectId, 60);
      const el = await createComponent(projectId);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      const before = win.items.map((a) => a.id);
      h.hold();
      internals(el).toggleSort('updated'); // desc to asc
      await vi.waitFor(() => expect(h.sent).toHaveLength(2));
      internals(el).toggleSort('updated'); // back to desc
      h.release();
      await idle(el);

      expect(h.sent[1].signal?.aborted).toBe(true);
      expect(h.sent).toHaveLength(2);
      expect(win.state).toBe('paged');
      expect(win.items.map((a) => a.id)).toEqual(before);
    });

    it('a switch to name sort during the first load drains the set instead of dead-ending', async () => {
      const projectId = 'p-first-name';
      const { h } = setup(projectId, 60);
      h.hold();
      const el = await createComponent(projectId, { holdsFirstLoad: true });
      expect(h.sent).toHaveLength(1);
      internals(el).toggleSort('name');
      h.release();
      await idle(el);

      expect(h.sent[0].signal?.aborted).toBe(true);
      expect(h.sent).toHaveLength(2);
      expect(new URL(h.sent[1].url, 'http://x').searchParams.has('sort')).toBe(false);
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agentWindow.items).toHaveLength(25);
      expect(el.shadowRoot?.textContent).not.toContain('Could not load every agent');
    });

    it('a drain of 2,001 agents that lands capped after a switch to the updated sort sends one sorted request and ends paged', async () => {
      const projectId = 'p-drain-then-updated';
      const { h } = setup(projectId, 2001);
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
      h.hold();
      const el = await createComponent(projectId, { holdsFirstLoad: true });
      expect(h.sent).toHaveLength(1);
      expect(new URL(h.sent[0].url, 'http://x').searchParams.has('sort')).toBe(false);
      internals(el).toggleSort('updated');
      h.release();
      await idle(el);

      // Four drain pages, then one sorted request for the updated sort.
      expect(h.sent).toHaveLength(5);
      expect(
        h.sent.slice(0, 4).every((r) => !new URL(r.url, 'http://x').searchParams.has('sort'))
      ).toBe(true);
      expect(new URL(h.sent[4].url, 'http://x').searchParams.get('sort')).toBe('updated');
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      expect(win.planRequest('view-change', '')).toBe('none');
    });

    it('a superseding sorted request aborts an in-flight Next, and its late page is not shown', async () => {
      const projectId = 'p-next-superseded';
      const { agents, h } = setup(projectId, 60);
      const el = await createComponent(projectId);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      h.hold();
      void win.next();
      await vi.waitFor(() => expect(h.sent).toHaveLength(2));
      expect(new URL(h.sent[1].url, 'http://x').searchParams.has('cursor')).toBe(true);
      internals(el).toggleSort('updated'); // desc to asc: a new sorted request
      await vi.waitFor(() => expect(h.sent).toHaveLength(3));
      expect(h.sent[1].signal?.aborted).toBe(true);
      h.release();
      await idle(el);
      expect(win.pageIndex).toBe(0);
      const expected = [...agents]
        .sort((a, b) => (a.updated ?? '').localeCompare(b.updated ?? ''))
        .slice(0, 25)
        .map((a) => a.id);
      expect(win.items.map((a) => a.id)).toEqual(expected);
    });

    for (const change of ['dir', 'phase', 'page size'] as const) {
      it(`a ${change} change on page 2 lands on page 0 with no cursor`, async () => {
        const projectId = `p-reset-${change.replace(' ', '-')}`;
        const { h } = setup(projectId, 60);
        const el = await createComponent(projectId);
        const win = internals(el).agentWindow;
        expect(win.state).toBe('paged');
        await win.next();
        await win.next();
        expect(win.pageIndex).toBe(2);
        expect((win as unknown as { rangeStart: number }).rangeStart).toBe(50);

        if (change === 'dir') internals(el).toggleSort('updated');
        else if (change === 'phase') internals(el).setPhaseFilter('running');
        else internals(el).onPagerSizeChange(50);
        await idle(el);

        expect(win.pageIndex).toBe(0);
        expect((win as unknown as { rangeStart: number }).rangeStart).toBe(0);
        expect(new URL(h.sent.at(-1)!.url, 'http://x').searchParams.has('cursor')).toBe(false);
      });
    }
  });

  describe('a scope switch during a drain', () => {
    it('aborts the page fetch and sends no further page request', async () => {
      const projectId = 'p-scope-drain';
      localStorage.setItem('scion-view-project-agents', 'graph');
      const agents = Array.from({ length: 2001 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      const inner = createRealisticFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests,
      });
      const legacy: Array<{ url: string; signal: AbortSignal | undefined }> = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(raw, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents`) {
            legacy.push({ url: raw, signal: init?.signal ?? undefined });
            // The page is navigated away while page 2 is in flight.
            if (legacy.length === 2) stateManager.setScope({ type: 'brokers-list' });
          }
          return inner(input, init);
        })
      );
      const el = await createComponent(projectId);
      await vi.waitFor(() => expect(internals(el).agentsLoading).toBe(false));
      await settle(el);
      expect(legacy).toHaveLength(2);
      expect(legacy[1].signal?.aborted).toBe(true);
      expect(new URL(legacy[1].url, 'http://x').searchParams.get('cursor')).toBe('500');
    });
  });

  describe('capped stats', () => {
    it('a capped set marks the Agents and Running stats as incomplete', async () => {
      const projectId = 'p-capped-stats';
      localStorage.setItem('scion-view-project-agents', 'graph');
      const agents = Array.from({ length: 2001 }, (_, i) =>
        makeAgent(i, { projectId, phase: i % 2 === 0 ? 'running' : 'stopped' })
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
      await vi.waitFor(() => expect(internals(el).agentWindow.state).toBe('capped'));
      await el.updateComplete;
      const stats = Array.from(el.shadowRoot!.querySelectorAll('.stat')).map((n) =>
        (n.textContent ?? '').replace(/\s+/g, ' ').trim()
      );
      expect(stats[0]).toBe('Agents 2,000loaded (newest 2,000 checked), more exist');
      expect(stats[1]).toBe('Running 1,000among loaded (newest 2,000 checked), more exist');
    });

    it('a complete set shows plain formatted stats', async () => {
      const projectId = 'p-plain-stats';
      localStorage.setItem('scion-view-project-agents', 'graph');
      const agents = Array.from({ length: 1200 }, (_, i) => makeAgent(i, { projectId }));
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
      await vi.waitFor(() => expect(internals(el).agentWindow.state).toBe('held'));
      await el.updateComplete;
      const stats = Array.from(el.shadowRoot!.querySelectorAll('.stat')).map((n) =>
        (n.textContent ?? '').replace(/\s+/g, ' ').trim()
      );
      expect(stats[0]).toBe('Agents 1,200');
      expect(stats[1]).toBe('Running 1,200');
      expect(el.shadowRoot!.querySelector('.stat-incomplete')).toBeNull();
    });
  });

  describe('full-view pages replace stored agents', () => {
    it('a field missing from a later full-view page is gone from the store', async () => {
      const projectId = 'p-replace';
      localStorage.setItem('scion-view-project-agents', 'list');
      let agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, taskSummary: 'old task' })
      );
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) =>
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
          })(input, init)
        )
      );
      const el = await createComponent(projectId);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('paged');
      const id = win.items[0].id;
      expect(stateManager.getAgent(id)?.taskSummary).toBe('old task');
      agents = agents.map((a) => {
        const { taskSummary: _dropped, ...rest } = a;
        return rest as Agent;
      });
      await win.refresh();
      expect(stateManager.getAgent(id)).toBeDefined();
      expect(stateManager.getAgent(id)?.taskSummary).toBeUndefined();
    });
  });

  describe('a refused label keeps the typed label filter on the kept rows', () => {
    for (const path of [
      { name: 'the sorted request', sort: 'updated' },
      { name: 'the drain', sort: 'name' },
    ] as const) {
      it(`a label 400 on ${path.name} keeps the previous rows filtered by the typed label`, async () => {
        const projectId = `p-label-400-preview-${path.sort}`;
        localStorage.setItem('scion-view-project-agents', 'list');
        localStorage.setItem(
          `scion-sort-project-agents-${projectId}`,
          JSON.stringify({ field: path.sort, dir: path.sort === 'name' ? 'asc' : 'desc' })
        );
        const agents = Array.from({ length: 10 }, (_, i) =>
          makeAgent(i, { projectId, labels: { env: i % 2 === 0 ? 'prod' : 'dev' } })
        );
        const requests: AgentsRequest[] = [];
        let refuse = false;
        const inner = createRealisticFetchHandler({
          projectId,
          projectCaps: { actions: ['read'] },
          agents,
          requests,
        });
        vi.stubGlobal(
          'fetch',
          vi.fn((input: string | URL | Request, init?: RequestInit) => {
            const raw =
              typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
            const u = new URL(raw, 'http://localhost');
            if (
              refuse &&
              u.pathname === `/api/v1/projects/${projectId}/agents` &&
              u.searchParams.get('label') === 'env=prod'
            ) {
              requests.push({ url: raw });
              return Promise.resolve(jsonResponse({ error: { message: 'bad label' } }, 400));
            }
            return inner(input, init);
          })
        );
        const el = await createComponent(projectId);
        await settle(el);
        expect(internals(el).agentWindow.state).toBe('small');
        expect(internals(el).agentWindow.items).toHaveLength(10);

        refuse = true;
        const n = requests.length;
        const input = labelInput(el)!;
        input.value = 'env=prod';
        input.dispatchEvent(new Event('sl-input'));
        input.dispatchEvent(new Event('sl-change'));
        await settle(el);

        expect(requests.length - n).toBe(1); // the refused request, not retried
        expect(internals(el).committedLabel).toBe('');
        const prodIds = agents
          .filter((a) => a.labels?.env === 'prod')
          .map((a) => a.id)
          .sort();
        const shown = internals(el)
          .agentWindow.items.map((a) => a.id)
          .sort();
        expect(shown).toEqual(prodIds);
        expect(el.shadowRoot!.querySelectorAll('.agent-table-container tbody tr')).toHaveLength(5);
        expect(labelInput(el)!.value).toBe('env=prod');
      });
    }
  });

  describe('a reconnect in the local states', () => {
    const reconnect = () => {
      const sse = (stateManager as unknown as { sseClientInstance: EventTarget }).sseClientInstance;
      sse.dispatchEvent(new CustomEvent('disconnected'));
      sse.dispatchEvent(new CustomEvent('connected'));
    };
    const bannerText = (el: TestEl) =>
      el.shadowRoot!.querySelector('.agent-window-banner')?.textContent ?? '';

    for (const c of [
      { state: 'small', count: 5, view: 'list', banner: 'may be stale' },
      { state: 'held', count: 1200, view: 'graph', banner: 'may be stale' },
      // The capped banner wins over the stale one.
      { state: 'capped', count: 100, view: 'graph', banner: '80 loaded (newest 2,000 checked)' },
    ] as const) {
      it(`${c.state}: a reconnect marks the set as possibly stale with no request`, async () => {
        const projectId = `p-reconnect-${c.state}`;
        localStorage.setItem('scion-view-project-agents', c.view);
        const agents = Array.from({ length: c.count }, (_, i) => makeAgent(i, { projectId }));
        const requests: AgentsRequest[] = [];
        vi.stubGlobal(
          'fetch',
          vi.fn(
            createRealisticFetchHandler({
              projectId,
              projectCaps: { actions: ['read'] },
              agents,
              requests,
              legacyTruncated: c.state === 'capped',
            })
          )
        );
        const el = await createComponent(projectId);
        await settle(el);
        expect(internals(el).agentWindow.state).toBe(c.state);
        expect(internals(el).agentWindow.stale).toBe(false);
        if (c.state !== 'capped') expect(bannerText(el)).toBe('');
        const n = requests.length;

        reconnect();
        await settle(el);

        expect(internals(el).agentWindow.stale).toBe(true);
        expect(bannerText(el)).toContain(c.banner);
        expect(requests.length).toBe(n);
      });
    }
  });

  describe('drain failures and the read filter at page level', () => {
    it('a drain whose second page fails after retries shows "Incomplete: loaded N" and incomplete stats that do not claim 2,000 were checked', async () => {
      const projectId = 'p-drain-page2-fails';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
      const agents = Array.from({ length: 600 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            failLegacyCursor: '500',
          })
        )
      );
      const el = await createComponent(projectId);
      await settle(el);
      // The first page, then the second page tried three times.
      expect(requests).toHaveLength(4);
      expect(requests.slice(1).every((r) => r.url.includes('cursor=500'))).toBe(true);
      expect(internals(el).agentWindow.state).toBe('capped');
      expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
        'Incomplete: loaded 500'
      );
      const stats = Array.from(el.shadowRoot!.querySelectorAll('.stat')).map((n) =>
        (n.textContent ?? '').replace(/\s+/g, ' ').trim()
      );
      expect(stats[0]).toBe('Agents 500loaded, incomplete');
      expect(stats[1]).toMatch(/^Running \d+among loaded, incomplete$/);
      expect(el.shadowRoot!.textContent).not.toContain('newest 2,000 checked');
    });

    it('2,001 agents of which 700 of the newest 2,000 are readable: "700 loaded (newest 2,000 checked), more exist"', async () => {
      const projectId = 'p-drain-read-filtered';
      localStorage.setItem('scion-view-project-agents', 'graph');
      const agents = Array.from({ length: 2001 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            readable: (a) => Number(a.id.slice(2)) % 20 < 7,
          })
        )
      );
      const el = await createComponent(projectId);
      await settle(el);
      expect(requests).toHaveLength(4);
      expect(internals(el).agentWindow.state).toBe('capped');
      expect(internals(el).agents).toHaveLength(700);
      expect(el.shadowRoot!.querySelector('.agent-window-banner')?.textContent).toContain(
        '700 loaded (newest 2,000 checked), more exist'
      );
    });
  });

  describe('leaving the capped state', () => {
    it('a capped set returns to paged on a switch to the updated sort', async () => {
      const projectId = 'p-capped-to-paged';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'name', dir: 'asc' })
      );
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i, { projectId }));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true,
          })
        )
      );
      const el = await createComponent(projectId);
      await settle(el);
      expect(internals(el).agentWindow.state).toBe('capped');
      const n = requests.length;

      internals(el).toggleSort('updated');
      await settle(el);

      expect(requests.length - n).toBe(1);
      expect(requests[requests.length - 1].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('paged');
      expect(el.shadowRoot!.querySelector('.agent-window-banner')).toBeNull();
    });

    it('a capped set whose label was refused stays capped on a switch to the updated sort; a new label retries once', async () => {
      const projectId = 'p-capped-refused';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 100 }, (_, i) =>
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
            legacyTruncated: true,
            refuseSortedAbove: 50,
          })
        )
      );
      const el = await createComponent(projectId);
      await settle(el);
      // The refused sorted request, then a four-page drain.
      expect(requests).toHaveLength(5);
      expect(internals(el).agentWindow.state).toBe('capped');

      let n = requests.length;
      internals(el).toggleSort('name');
      await settle(el);
      internals(el).toggleSort('updated');
      await settle(el);
      internals(el).toggleSort('updated'); // dir flip
      await settle(el);
      expect(requests.length - n).toBe(0);
      expect(internals(el).agentWindow.state).toBe('capped');

      n = requests.length;
      const input = labelInput(el)!;
      input.value = 'env=prod';
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
      await settle(el);
      expect(requests.length - n).toBe(5);
      expect(requests[n].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('capped');
    });
  });

  describe('small state: a live reorder across a local page boundary', () => {
    it('an activity bump moves an agent from page 2 to page 1 with no request and no page change', async () => {
      const projectId = 'p-small-reorder';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i, { projectId }));
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
      await settle(el);
      const win = internals(el).agentWindow;
      expect(win.state).toBe('small');
      await win.next();
      await el.updateComplete;
      expect(win.pageIndex).toBe(1);
      // Updated desc: a-29 first; page 2 holds the five oldest.
      expect(win.items.map((a) => a.id)).toEqual(['a-4', 'a-3', 'a-2', 'a-1', 'a-0']);

      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        data: { agentId: 'a-0', phase: 'running', lastActivityEvent: '2026-03-01T00:00:00Z' },
      });
      await flushLive(el);

      expect(win.pageIndex).toBe(1);
      expect(win.items.map((a) => a.id)).toEqual(['a-5', 'a-4', 'a-3', 'a-2', 'a-1']);
      expect(win.display[0].id).toBe('a-0');
      expect(requests).toHaveLength(1);

      await win.prev();
      await el.updateComplete;
      expect(win.items[0].id).toBe('a-0');
      expect(win.items).toHaveLength(25);
      expect(requests).toHaveLength(1);
    });
  });

  describe('label typing and the debounce window', () => {
    it('label typing sends no request even after the debounce window', async () => {
      const projectId = 'p-label-debounce';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 60 }, (_, i) =>
        makeAgent(i, { projectId, labels: { env: i % 2 === 0 ? 'prod' : 'dev' } })
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
      await settle(el);
      expect(internals(el).agentWindow.state).toBe('paged');
      const n = requests.length;

      vi.useFakeTimers();
      try {
        const input = labelInput(el)!;
        for (const value of ['e', 'en', 'env', 'env=', 'env=prod']) {
          input.value = value;
          input.dispatchEvent(new Event('sl-input'));
          await vi.advanceTimersByTimeAsync(100);
        }
        await vi.advanceTimersByTimeAsync(2000);
        expect(requests.length).toBe(n);
        expect(internals(el).agentsLoading).toBe(false);
      } finally {
        vi.useRealTimers();
      }
      await el.updateComplete;
      expect(requests.length).toBe(n);
      expect(internals(el).agentWindow.items.every((a) => a.labels?.env === 'prod')).toBe(true);
    });
  });

  describe('page-level agents requests the tests hold, fail or answer', () => {
    /**
     * Mounts the page over the realistic handler. `ctl.holdNext()` holds
     * the next agents GET open until `ctl.release()`; `ctl.failAgents` and
     * `ctl.failProjectOnce` answer 500. Every agents GET is recorded with
     * its signal.
     */
    function controlledFetch(projectId: string, agents: Agent[]) {
      const requests: AgentsRequest[] = [];
      const inner = createRealisticFetchHandler({
        projectId,
        projectCaps: { actions: ['read'] },
        agents,
        requests,
      });
      const sent: Array<{ url: string; signal: AbortSignal | undefined }> = [];
      const gates: Array<() => void> = [];
      const ctl = {
        requests,
        sent,
        toHold: 0,
        failAgents: false,
        failProjectOnce: false,
        holdNext(): void {
          ctl.toHold++;
        },
        get heldCount(): number {
          return gates.length;
        },
        release(): void {
          for (const open of gates.splice(0)) open();
        },
      };
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
          const raw =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(raw, 'http://localhost');
          const signal = init?.signal ?? undefined;
          if (u.pathname === `/api/v1/projects/${projectId}` && ctl.failProjectOnce) {
            ctl.failProjectOnce = false;
            return jsonResponse({ error: { message: 'project boom' } }, 500);
          }
          if (u.pathname === `/api/v1/projects/${projectId}/agents`) {
            sent.push({ url: raw, signal });
            // The response is computed now, so it predates any live event
            // the test emits while it is held.
            const res = ctl.failAgents
              ? (requests.push({ url: raw }), jsonResponse({ error: { message: 'boom' } }, 500))
              : await inner(input, init);
            if (ctl.toHold > 0) {
              ctl.toHold--;
              await new Promise<void>((resolve, reject) => {
                gates.push(resolve);
                signal?.addEventListener(
                  'abort',
                  () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' })),
                  { once: true }
                );
              });
            }
            if (signal?.aborted) throw Object.assign(new Error('aborted'), { name: 'AbortError' });
            return res;
          }
          return inner(input, init);
        })
      );
      return ctl;
    }

    const commitLabel = (el: TestEl, value: string) => {
      const input = labelInput(el)!;
      input.value = value;
      input.dispatchEvent(new Event('sl-input'));
      input.dispatchEvent(new Event('sl-change'));
    };

    const emitLabelledCreate = (projectId: string, id: string, env: string) =>
      (
        stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
      ).handleUpdate({
        subject: `project.${projectId}.agent.created`,
        data: {
          agentId: id,
          id,
          name: id,
          projectId,
          template: 't',
          phase: 'running',
          created: '2026-03-01T00:00:00Z',
          updated: '2026-03-01T00:00:00Z',
          messageMode: 'project',
          labels: { env },
        },
      });

    for (const path of [
      { name: 'a complete fit request', view: 'list', count: 10 },
      { name: 'a drain (the tree view)', view: 'graph', count: 1200 },
    ]) {
      it(`a live create during ${path.name} under a committed k=v label joins only if it matches the label`, async () => {
        const projectId = `p-label-create-${path.view}`;
        localStorage.setItem('scion-view-project-agents', path.view);
        const agents = Array.from({ length: path.count }, (_, i) =>
          makeAgent(i, { projectId, labels: { env: i % 2 === 0 ? 'prod' : 'dev' } })
        );
        const ctl = controlledFetch(projectId, agents);
        const el = await createComponent(projectId);
        await settle(el);
        const n = ctl.sent.length;
        ctl.holdNext();
        commitLabel(el, 'env=prod');
        await vi.waitFor(() => expect(ctl.heldCount).toBe(1));
        const q = new URL(ctl.sent[n].url, 'http://x').searchParams;
        expect(q.get('label')).toBe('env=prod');
        expect(q.has('sort')).toBe(path.view === 'list');

        emitLabelledCreate(projectId, 'live-prod', 'prod');
        emitLabelledCreate(projectId, 'live-dev', 'dev');
        await flushLive(el);
        ctl.release();
        await settle(el);

        const expected = agents.filter((a) => a.labels?.env === 'prod').length + 1;
        const ids = new Set(internals(el).agents.map((a) => a.id));
        expect(ids.has('live-prod')).toBe(true);
        expect(ids.has('live-dev')).toBe(false);
        expect(internals(el).agentWindow.state).toBe(path.view === 'list' ? 'small' : 'held');
        expect(internals(el).agentStats.total).toBe(expected);
        expect(internals(el).agentWindow.display.every((a) => a.labels?.env === 'prod')).toBe(true);
      });
    }

    it('removing the element during a drain aborts its page fetch and sends no further page request', async () => {
      const projectId = 'p-remove-drain';
      localStorage.setItem('scion-view-project-agents', 'graph');
      const agents = Array.from({ length: 1200 }, (_, i) => makeAgent(i, { projectId }));
      const ctl = controlledFetch(projectId, agents);
      ctl.holdNext();
      const el = await createComponent(projectId, { holdsFirstLoad: true });
      await vi.waitFor(() => expect(ctl.heldCount).toBe(1));
      expect(ctl.sent).toHaveLength(1);
      // The tree view drains from the first request.
      expect(new URL(ctl.sent[0].url, 'http://x').searchParams.has('sort')).toBe(false);
      expect(ctl.sent[0].signal?.aborted).toBe(false);

      // Removing the element alone does not change the store's scope.
      el.remove();
      expect(ctl.sent[0].signal?.aborted).toBe(true);
      ctl.release();
      for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
      expect(ctl.sent).toHaveLength(1);
      expect(stateManager.getAgents().filter((a) => a.projectId === projectId)).toHaveLength(0);
    });

    it('a failed page load that follows earlier data empties the agents instead of keeping the old rows', async () => {
      const projectId = 'p-page-load-fails';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 10 }, (_, i) => makeAgent(i, { projectId }));
      const ctl = controlledFetch(projectId, agents);
      // The project request fails once while the agents request succeeds.
      ctl.failProjectOnce = true;
      const el = await createComponent(projectId);
      await settle(el);
      expect(internals(el).agents).toHaveLength(10);
      expect(internals(el).agentWindow.state).toBe('small');
      const retry = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
        b.textContent?.includes('Retry')
      ) as HTMLElement | undefined;
      expect(retry).toBeDefined();

      // Retry loads the page again; this time its agents request fails.
      ctl.failAgents = true;
      const n = ctl.requests.length;
      retry!.click();
      await vi.waitFor(() => expect(ctl.requests.length).toBe(n + 1));
      await vi.waitFor(() => expect(el.shadowRoot!.textContent).toContain('Window Project'));
      await settle(el);
      expect(new URL(ctl.requests[n].url, 'http://x').searchParams.get('sort')).toBe('updated');
      expect(internals(el).agents).toEqual([]);
      expect(internals(el).agentWindow.state).toBe('small');
      expect(internals(el).agentStats.total).toBe(0);
      expect(el.shadowRoot!.querySelectorAll('.agent-table-container tbody tr')).toHaveLength(0);
    });
  });
});
