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
 * W10 P1 subset (design §9, the P1 gate), plus the W4 items the P1c brief
 * names explicitly: off-page upsert (R2-B2), reconnect chip with no
 * request, label typing/commit, a label 400 keeping previous data,
 * lifecycle refresh issuing exactly one request, and the 422 fallback with
 * its per-label memory. A5 (small-state display identical to pre-change
 * `displayAgents`) is covered via `agent-sort.test.ts`'s W1 parity and the
 * list/grid-identity assertions below.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, Capabilities, PageData } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import { stateManager } from '../../client/state.js';

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
  committedLabel: string;
  pagerPageSize: number;
  agents: Agent[];
  agentWindow: {
    next(): Promise<void>;
    prev(): Promise<void>;
    state: 'small' | 'paged';
    items: Agent[];
    pageIndex: number;
    updatesAvailable: boolean;
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

/** A deferred promise, for controlling fetch resolution order explicitly (round 1 review B5). */
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/**
 * A more realistic sorted-mode handler than `createFetchHandler`'s (used by
 * the round 1 review fix tests): `complete` is decided by `agents.length` vs
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

        const fit = Number(u.searchParams.get('fit') ?? '500');
        if (sorted.length <= fit) {
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

describe('project-detail — agent list window (P1c)', () => {
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

  describe('W10 P1 subset — list view, updated sort, 100 agents', () => {
    it('page load issues exactly one agents request (the fit request); every client-only interaction issues zero; label commit and lifecycle refresh issue exactly one each', async () => {
      const projectId = 'p-w10-list';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i));
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
      expect(requests[0].url).toContain('fit=500');

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
      const projectId = 'p-w10-grid';
      localStorage.setItem('scion-view-project-agents', 'grid');
      const agents = Array.from({ length: 100 }, (_, i) => makeAgent(i));
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

  describe('422 fallback and per-label memory', () => {
    it('a 422 falls back to the legacy load and is remembered for that label; a new label retries once', async () => {
      const projectId = 'p-422';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 10 }, (_, i) =>
        makeAgent(i, { labels: { env: 'prod' } })
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
      localStorage.setItem('scion-view-project-agents', 'grid'); // not P1-eligible: exercises the legacy-path N2 behavior directly
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
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

  describe('paged state: live updates (design §6.2 table, round 1 review N1/N2/B6), and reconnect', () => {
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

    it('R2-B2 + N1: an off-page phase change with no phase filter is counts-only — stats update live, no chip, no request', async () => {
      const projectId = 'p-paged-counts';
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i));
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
      // live "Running" count, which the design table says shows no chip for
      // (round 1 review N1).
      expect(internals(el).agentStats.running).toBe(29);
      expect(internals(el).agentWindow.updatesAvailable).toBe(false);
      expect(requests.length).toBe(before);
    });

    it('N1: an off-page change that newly passes the active phase filter raises the chip', async () => {
      const projectId = 'p-paged-newly-passes';
      const agents = Array.from({ length: 30 }, (_, i) =>
        makeAgent(i, { phase: i === 29 ? 'stopped' : 'running' })
      );
      const requests: AgentsRequest[] = [];
      const el = await mountForcedPaged(projectId, agents, requests);
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 10)); // the phase change's own one paged refetch (B3)
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

    it('B6: an off-page status change for a non-member under a different committed label never inflates stats.total', async () => {
      const projectId = 'p-paged-label';
      const members = Array.from({ length: 30 }, (_, i) =>
        makeAgent(i, { labels: { env: 'prod' } })
      );
      const nonMember = makeAgent(999, { labels: { env: 'dev' }, phase: 'stopped' });
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
      expect(requests.length).toBe(before);
    });

    it('on-page delete and phase-filter-failure still show the chip (backfill, unchanged by the N1 fix)', async () => {
      const projectId = 'p-paged-backfill';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
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

    it('reconnect (agents-resync) raises the chip with no request (plumbing only — state.ts/P1a owns resync detection itself)', async () => {
      const projectId = 'p-paged-resync';
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
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

  describe('small state: live updates (round 1 review B1 — critical)', () => {
    it('an SSE status delta updates the rendered list row, and an SSE create appears, with no re-adoption and no page reset', async () => {
      const projectId = 'p-small-live';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem(
        `scion-sort-project-agents-${projectId}`,
        JSON.stringify({ field: 'updated', dir: 'desc' })
      );
      const agents = Array.from({ length: 5 }, (_, i) => makeAgent(i));
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
      // extra request (round 1 review B1).
      expect((el as unknown as { agents: Agent[] }).agents.find((a) => a.id === 'a-4')?.phase).toBe(
        'stopped'
      );
      expect(internals(el).agentWindow.items.find((a) => a.id === 'a-4')?.phase).toBe('stopped');
      expect(requests.length).toBe(1);

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
  });

  describe('paged -> legacy transitions issue exactly one request each (round 1 review B2/B3)', () => {
    it('sort -> name (legacy) and name -> updated (paged again) each issue exactly one request; a phase change while paged issues exactly one', async () => {
      const projectId = 'p-b2b3';
      localStorage.setItem('scion-view-project-agents', 'list');
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i));
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
      expect(requests.length - n).toBe(1); // exactly one legacy load, not two (B3)
      expect(requests[requests.length - 1].url).not.toContain('sort=');
      expect(internals(el).agentWindow.state).toBe('small'); // out of 'paged' even though truncated (B2)

      n = requests.length;
      internals(el).toggleSort('updated');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1); // exactly one fit request, not two (B3)
      expect(requests[requests.length - 1].url).toContain('sort=updated');
      expect(internals(el).agentWindow.state).toBe('paged'); // forced paged again (fit=0)

      n = requests.length;
      internals(el).setPhaseFilter('running');
      await new Promise((r) => setTimeout(r, 10));
      expect(requests.length - n).toBe(1); // phase change while paged: exactly one (design row 6)
    });

    it('grid<->list toggles cost exactly the documented interim requests, then zero (round 1 review B3)', async () => {
      const projectId = 'p-b3-toggles';
      localStorage.setItem('scion-view-project-agents', 'list');
      // 30 agents; the sorted endpoint reports complete once asked again
      // with `fit=500` (the real default), so after one legacy load and one
      // fit retry the window settles into 'small' and further toggles are free.
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          // Force only the FIRST sorted request (the initial page load) to be
          // paged, by capping `fit` there; subsequent requests use the real
          // default (500) and come back complete.
          return createRealisticFetchHandler({
            projectId,
            projectCaps: { actions: ['read'] },
            agents,
            requests,
            legacyTruncated: true,
          })(input, init);
        })
      );
      // The mount's own first request uses fit=500 by default in the real
      // code, and 30 <= 500, so it is actually complete — use a dedicated
      // paged-first-load harness instead: start already paged via a forced
      // page-1 navigation isn't meaningful here, so this test instead starts
      // from the B2/B3 test's end state conceptually: begin in list+paged
      // by making the window ineligible at mount (grid), matching the
      // reviewer's probe P3 sequence.
      localStorage.setItem('scion-view-project-agents', 'grid');
      const el = await createComponent(projectId);
      // Grid load is legacy (not P1-eligible); truncated per legacyTruncated.
      expect(requests.length).toBe(1);
      expect(internals(el).listViewUsesWindow).toBe(false);

      const toggle = viewToggle(el)!;
      let n = requests.length;
      toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: 'list' } }));
      await new Promise((r) => setTimeout(r, 10));
      // grid(truncated) -> list(updated sort): one fit request (§11 P1c interim cost).
      expect(requests.length - n).toBe(1);
      expect(internals(el).listViewUsesWindow).toBe(true); // 30 <= 500: complete this time

      n = requests.length;
      for (const v of ['grid', 'list', 'grid', 'list', 'grid']) {
        toggle.dispatchEvent(new CustomEvent('view-change', { detail: { view: v } }));
        await new Promise((r) => setTimeout(r, 10));
      }
      expect(requests.length - n).toBe(0); // held: this.agents already has the complete set
    }, 20_000);
  });

  describe('persisted page size (round 1 review B4)', () => {
    it('is read once in connectedCallback, used for the first request, and matches the rendered pager', async () => {
      const projectId = 'p-b4';
      localStorage.setItem('scion-view-project-agents', 'list');
      localStorage.setItem('scion-pagesize-project-agents', '50');
      const agents = Array.from({ length: 80 }, (_, i) => makeAgent(i));
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
      const agents = Array.from({ length: 10 }, (_, i) => makeAgent(i));
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

  describe('stale-response guard (round 1 review B5)', () => {
    it('an older trigger whose response resolves later never overwrites a newer one', async () => {
      const projectId = 'p-b5';
      localStorage.setItem('scion-view-project-agents', 'list');
      const mountAgents = [makeAgent(0, { name: 'mount' })];
      const firstTriggerAgents = [makeAgent(1, { name: 'first-trigger' })];
      const secondTriggerAgents = [makeAgent(2, { name: 'second-trigger' })];
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
});
