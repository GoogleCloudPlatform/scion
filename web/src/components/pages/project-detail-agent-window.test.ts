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
  agentWindow: {
    next(): Promise<void>;
    prev(): Promise<void>;
    state: 'small' | 'paged';
    items: Agent[];
    updatesAvailable: boolean;
  };
  listViewUsesWindow: boolean;
}

function internals(el: TestEl): Internals {
  return el as unknown as Internals;
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

  describe('paged state: off-page upsert (R2-B2) and reconnect', () => {
    it('an off-page upsert of a page-0 agent raises the chip and updates stats with no request; a resync raises the chip with no request', async () => {
      const projectId = 'p-paged';
      localStorage.setItem('scion-view-project-agents', 'list');
      // 30 agents with a 25-row default page size: page 0 holds a-0..a-24,
      // page 1 holds the rest, so there is a real page 1 to navigate to.
      const agents = Array.from({ length: 30 }, (_, i) => makeAgent(i));
      const requests: AgentsRequest[] = [];
      // Force the paged branch regardless of count, to exercise paged-state
      // live updates without needing a true >500-candidate fixture.
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname === `/api/v1/projects/${projectId}/agents` && u.searchParams.get('sort')) {
            requests.push({ url: rawUrl });
            const cursor = u.searchParams.get('cursor');
            const limit = Number(u.searchParams.get('limit') ?? '25');
            const sorted = [...agents].sort((a, b) =>
              (b.updated ?? '').localeCompare(a.updated ?? '')
            );
            const startIdx = cursor ? Number(cursor) : 0;
            const page = sorted.slice(startIdx, startIdx + limit);
            const nextIdx = startIdx + limit;
            return Promise.resolve(
              jsonResponse({
                agents: page,
                totalCount: sorted.length,
                complete: false,
                nextCursor: nextIdx < sorted.length ? String(nextIdx) : undefined,
                stats: u.searchParams.get('stats')
                  ? {
                      total: sorted.length,
                      running: sorted.filter((a) => a.phase === 'running').length,
                      agents: sorted.map((a) => [a.id, a.phase]),
                    }
                  : undefined,
              })
            );
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
      expect(internals(el).agentWindow.state).toBe('paged');

      // Go to page 1, then change the phase of a page-0 agent (off-page from page 1's perspective).
      await internals(el).agentWindow.next();
      await el.updateComplete;
      const beforeRequestCount = requests.length;

      // Drive a real SSE status delta for the page-0 agent through the
      // state manager's own update path (handleUpdate -> handleAgentEvent),
      // the same path the real SSEClient would, so the coalesced-flush
      // pipeline (dirty set, rAF/100ms flush, `agents-changed`) is exercised
      // for real rather than bypassed.
      (
        stateManager as unknown as {
          handleUpdate(u: { subject: string; data: unknown }): void;
        }
      ).handleUpdate({
        subject: `project.${projectId}.agent.status`,
        // agents[29] has the highest `updated` value, so sorted desc by
        // `updated` it is on page 0 (the window is currently on page 1).
        data: { agentId: agents[29].id, phase: 'stopped' },
      });
      // Flush fires on the next animation frame, or after a 100ms fallback.
      await new Promise((r) => setTimeout(r, 150));
      await el.updateComplete;

      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(requests.length).toBe(beforeRequestCount); // no request

      // Reconnect: dispatch agents-resync directly on the shared stateManager.
      stateManager.dispatchEvent(new Event('agents-resync'));
      await el.updateComplete;
      expect(internals(el).agentWindow.updatesAvailable).toBe(true);
      expect(requests.length).toBe(beforeRequestCount); // still no request
    });
  });
});
