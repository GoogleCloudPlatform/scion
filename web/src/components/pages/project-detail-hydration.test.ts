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
 * Tests for project-detail.ts startup: reuse of the server-prefetched
 * project, and project header readiness separate from the agents load.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { PageData } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import { stateManager } from '../../client/state.js';
import { AgentDrainRunner } from '../../client/agent-drain.js';

/**
 * happy-dom has no EventSource; setScope opens one. Opens on the next tick,
 * so a drain's wait for the live connection resolves at once.
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

const PROJECT_ID = 'p-hyd';
const USER = { id: 'u', email: 'u@example.com', name: 'U', role: 'member' as const };

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Deferred {
  promise: Promise<Response>;
  resolve(r: Response): void;
}

function deferred(): Deferred {
  let resolve!: (r: Response) => void;
  const promise = new Promise<Response>((r) => (resolve = r));
  return { promise, resolve };
}

const agent = (id: string) => ({
  id,
  name: id,
  projectId: PROJECT_ID,
  phase: 'running',
  activity: 'idle',
  created: '2026-01-01T00:00:00Z',
  updated: '2026-01-01T00:00:00Z',
});

interface FetchPlan {
  /** Fetched project; defaults to "Fetched Name". */
  project?: () => Promise<Response>;
  /** Agents responses, consumed in order; the last one repeats. */
  agents?: Array<() => Promise<Response>>;
}

let calls: string[] = [];

function installFetch(plan: FetchPlan): void {
  let agentsCall = 0;
  calls = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request): Promise<Response> => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const path = new URL(raw, 'http://localhost').pathname;
      calls.push(path);
      if (path === `/api/v1/projects/${PROJECT_ID}/agents`) {
        const list = plan.agents ?? [
          () => Promise.resolve(jsonResponse({ agents: [agent('a-1')], complete: true })),
        ];
        const next = list[Math.min(agentsCall, list.length - 1)];
        agentsCall++;
        return next();
      }
      if (path === `/api/v1/projects/${PROJECT_ID}`) {
        return plan.project
          ? plan.project()
          : Promise.resolve(
              jsonResponse({ id: PROJECT_ID, name: 'Fetched Name', _capabilities: { actions: [] } })
            );
      }
      return Promise.resolve(jsonResponse({}, 404));
    })
  );
}

type TestEl = HTMLElement & {
  updateComplete: Promise<boolean>;
  pageData: PageData | null;
  projectId: string;
};

function mount(data?: Record<string, unknown>): TestEl {
  const el = document.createElement('scion-page-project-detail') as TestEl;
  el.projectId = PROJECT_ID;
  // Drain retries without a delay.
  (el as unknown as { drainRunner: AgentDrainRunner }).drainRunner = new AgentDrainRunner({
    retryDelayMs: 0,
  });
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: USER,
    ...(data ? { data } : {}),
  };
  document.body.appendChild(el);
  return el;
}

async function flush(el: TestEl): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

const projectRequests = () => calls.filter((p) => p === `/api/v1/projects/${PROJECT_ID}`).length;
const agentsRequests = () =>
  calls.filter((p) => p === `/api/v1/projects/${PROJECT_ID}/agents`).length;
const heading = (el: TestEl) =>
  el.shadowRoot?.querySelector('.header h1')?.textContent?.trim() ?? null;
const text = (el: TestEl) => el.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';

const SSR_PROJECT = {
  id: PROJECT_ID,
  name: 'Prefetched Name',
  _capabilities: { actions: ['read'] },
};

describe('scion-page-project-detail — startup hydration and readiness', () => {
  let el: TestEl | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
    stateManager.setScope({ type: 'dashboard' });
  });

  afterEach(() => {
    localStorage.removeItem('scion-view-project-agents');
    el?.remove();
    el = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('cold SSR load: uses the matching prefetched project and sends no project request', async () => {
    installFetch({});
    el = mount(SSR_PROJECT);
    await el.updateComplete;
    // The header is there on the first render, before any response.
    expect(heading(el)).toContain('Prefetched Name');
    await flush(el);
    expect(projectRequests()).toBe(0);
    expect(agentsRequests()).toBe(1);
    expect(heading(el)).toContain('Prefetched Name');
    // Seeded into the store after the page set its scope, so the scope
    // change did not discard it.
    expect(stateManager.getProject(PROJECT_ID)?.name).toBe('Prefetched Name');
  });

  it('SPA navigation (no payload): fetches the project', async () => {
    installFetch({});
    el = mount();
    await flush(el);
    expect(projectRequests()).toBe(1);
    expect(heading(el)).toContain('Fetched Name');
  });

  it('mismatched payload (another project, a list, or not a project): ignored and fetched', async () => {
    for (const data of [
      { id: 'p-other', name: 'Other Project' },
      { projects: [{ id: PROJECT_ID, name: 'Listed' }] },
      { id: PROJECT_ID },
    ]) {
      installFetch({});
      el = mount(data);
      await flush(el);
      expect(projectRequests()).toBe(1);
      expect(heading(el)).toContain('Fetched Name');
      el.remove();
      el = null;
    }
  });

  it('the payload is used once: a reconnect of the same element fetches', async () => {
    installFetch({});
    el = mount(SSR_PROJECT);
    await flush(el);
    expect(projectRequests()).toBe(0);
    el.remove();
    document.body.appendChild(el);
    await flush(el);
    expect(projectRequests()).toBe(1);
    expect(heading(el)).toContain('Fetched Name');
  });

  it('shows the project header while the agents request is pending, without the empty state', async () => {
    const held = deferred();
    installFetch({ agents: [() => held.promise] });
    el = mount();
    await flush(el);
    expect(heading(el)).toContain('Fetched Name');
    expect(text(el)).toContain('Loading agents…');
    expect(text(el)).not.toContain('No Agents');

    held.resolve(jsonResponse({ agents: [agent('a-1')], complete: true }));
    await flush(el);
    expect(text(el)).not.toContain('Loading agents…');
    expect(el.shadowRoot?.querySelectorAll('.agent-card').length ?? 0).toBeGreaterThan(0);
  });

  it('a failed agents load keeps the project on screen and offers an agents-only retry', async () => {
    installFetch({
      agents: [
        () => Promise.resolve(jsonResponse({ error: 'boom' }, 500)),
        () => Promise.resolve(jsonResponse({ agents: [agent('a-1')], complete: true })),
      ],
    });
    el = mount(SSR_PROJECT);
    await flush(el);
    expect(heading(el)).toContain('Prefetched Name');
    expect(text(el)).toContain('Could not load agents.');
    expect(text(el)).not.toContain('No Agents');
    expect(text(el)).not.toContain('Failed to Load Project');

    const retry = el.shadowRoot?.querySelector('.agents-load-error sl-button') as HTMLElement;
    expect(retry).toBeTruthy();
    retry.click();
    await flush(el);
    expect(agentsRequests()).toBe(2);
    expect(projectRequests()).toBe(0);
    expect(text(el)).not.toContain('Could not load agents.');
    expect(el.shadowRoot?.querySelectorAll('.agent-card').length ?? 0).toBeGreaterThan(0);
  });

  it('a network error on the agents load also shows the agents error, not the page error', async () => {
    installFetch({ agents: [() => Promise.reject(new TypeError('network'))] });
    el = mount();
    await flush(el);
    expect(heading(el)).toContain('Fetched Name');
    expect(text(el)).toContain('Could not load agents.');
  });

  it('a failed project load shows the page error; its retry refetches', async () => {
    let fail = true;
    installFetch({
      project: () =>
        Promise.resolve(
          fail
            ? jsonResponse({ error: 'denied' }, 403)
            : jsonResponse({ id: PROJECT_ID, name: 'Fetched Name' })
        ),
    });
    el = mount();
    await flush(el);
    expect(text(el)).toContain('Failed to Load Project');
    fail = false;
    const retry = el.shadowRoot?.querySelector('.error-state sl-button') as HTMLElement;
    retry.click();
    await flush(el);
    expect(projectRequests()).toBe(2);
    expect(heading(el)).toContain('Fetched Name');
  });

  it('SSE race: a project update during the pending agents load applies over the prefetched project', async () => {
    const held = deferred();
    installFetch({ agents: [() => held.promise] });
    el = mount(SSR_PROJECT);
    await flush(el);
    (
      stateManager as unknown as {
        handleProjectEvent(id: string, type: string, data: unknown): void;
      }
    ).handleProjectEvent(PROJECT_ID, 'updated', { projectId: PROJECT_ID, name: 'Renamed Live' });
    await flush(el);
    expect(heading(el)).toContain('Renamed Live');
    expect(text(el)).toContain('Loading agents…');

    held.resolve(jsonResponse({ agents: [agent('a-1')], complete: true }));
    await flush(el);
    expect(heading(el)).toContain('Renamed Live');
    expect(projectRequests()).toBe(0);
  });
  const switchView = (target: TestEl, view: string): void => {
    target
      .shadowRoot!.querySelector('scion-view-toggle')!
      .dispatchEvent(new CustomEvent('view-change', { detail: { view } }));
  };

  it('case A: a graph-to-grid view change during the pending first load; No Agents never shows, and a failure shows the error and Retry', async () => {
    localStorage.setItem('scion-view-project-agents', 'graph');
    const first = deferred();
    let failing = true;
    installFetch({
      agents: [
        () => first.promise,
        () =>
          Promise.resolve(
            failing
              ? jsonResponse({ error: 'boom' }, 500)
              : jsonResponse({ agents: [agent('a-1')], complete: true })
          ),
      ],
    });
    el = mount(SSR_PROJECT);
    await flush(el);
    expect(agentsRequests()).toBe(1);
    expect(text(el)).toContain('Loading agents…');

    switchView(el, 'grid');
    await flush(el);
    // In the small state a view change plans no request of its own: the
    // first (drain) request stays the live one.
    expect(agentsRequests()).toBe(1);
    expect(text(el)).toContain('Loading agents…');
    expect(text(el)).not.toContain('No Agents');

    first.resolve(jsonResponse({ error: 'boom' }, 500));
    // The drain retries the failed page before giving up.
    await vi.waitFor(() => expect(text(el!)).toContain('Could not load agents.'));
    expect(text(el)).not.toContain('No Agents');
    expect(heading(el)).toContain('Prefetched Name');

    const retry = el.shadowRoot?.querySelector('.agents-load-error sl-button') as HTMLElement;
    expect(retry).toBeTruthy();
    failing = false;
    const before = agentsRequests();
    retry.click();
    await vi.waitFor(() => expect(text(el!)).not.toContain('Could not load agents.'));
    expect(agentsRequests()).toBe(before + 1);
    expect(el.shadowRoot?.querySelectorAll('.agent-card').length ?? 0).toBeGreaterThan(0);
  });

  it('a lifecycle refresh that fails before any result was adopted shows the agents error', async () => {
    // Stop All's follow-up refresh is the only non page-load trigger that can
    // run before a result; it enters through backgroundRefresh.
    const first = deferred();
    installFetch({
      agents: [() => first.promise, () => Promise.resolve(jsonResponse({ error: 'boom' }, 500))],
    });
    el = mount(SSR_PROJECT);
    await flush(el);
    (el as unknown as { backgroundRefresh(trigger: string): void }).backgroundRefresh(
      'lifecycle-refresh'
    );
    await flush(el);
    expect(agentsRequests()).toBe(2);
    // The superseded first response lands late and is ignored.
    first.resolve(jsonResponse({ agents: [], complete: true }));
    await vi.waitFor(() => expect(text(el!)).toContain('Could not load agents.'));
    expect(text(el)).not.toContain('No Agents');
    expect(el.shadowRoot?.querySelector('.agents-load-error sl-button')).toBeTruthy();
  });

  it('a view change that supersedes a lifecycle refresh before any result re-sends the page load', async () => {
    localStorage.setItem('scion-view-project-agents', 'grid');
    const first = deferred();
    const second = deferred();
    installFetch({
      agents: [
        () => first.promise,
        () => second.promise,
        () => Promise.resolve(jsonResponse({ agents: [agent('a-1')], complete: true })),
      ],
    });
    el = mount(SSR_PROJECT);
    await flush(el);
    (el as unknown as { backgroundRefresh(trigger: string): void }).backgroundRefresh(
      'lifecycle-refresh'
    );
    await flush(el);
    expect(agentsRequests()).toBe(2);

    // Switching to graph needs the complete set, superseding the refresh.
    switchView(el, 'graph');
    await vi.waitFor(() => expect(agentsRequests()).toBe(3));
    first.resolve(jsonResponse({ agents: [], complete: true }));
    second.resolve(jsonResponse({ agents: [], complete: true }));
    await vi.waitFor(() => expect(text(el!)).not.toContain('Loading agents…'));
    expect(text(el)).not.toContain('No Agents');
    expect(el.shadowRoot?.querySelector('scion-agent-tree-view')).toBeTruthy();
  });

  it('case B: a view change that re-issues the first load keeps the loading row until it lands', async () => {
    localStorage.setItem('scion-view-project-agents', 'grid');
    const first = deferred();
    const second = deferred();
    installFetch({ agents: [() => first.promise, () => second.promise] });
    el = mount(SSR_PROJECT);
    await flush(el);
    expect(agentsRequests()).toBe(1);

    switchView(el, 'graph');
    await flush(el);
    expect(agentsRequests()).toBe(2);
    expect(text(el)).toContain('Loading agents…');
    expect(text(el)).not.toContain('No Agents');

    first.resolve(jsonResponse({ agents: [], complete: true }));
    await flush(el);
    expect(text(el)).toContain('Loading agents…');
    expect(text(el)).not.toContain('No Agents');

    second.resolve(jsonResponse({ agents: [agent('a-1')], complete: true }));
    await flush(el);
    expect(text(el)).not.toContain('Loading agents…');
    expect(text(el)).not.toContain('No Agents');
    expect(el.shadowRoot?.querySelector('scion-agent-tree-view')).toBeTruthy();
  });

  it('case B failure: a re-issued first load that fails shows the error and Retry', async () => {
    localStorage.setItem('scion-view-project-agents', 'grid');
    const first = deferred();
    installFetch({
      agents: [() => first.promise, () => Promise.resolve(jsonResponse({ error: 'boom' }, 500))],
    });
    el = mount(SSR_PROJECT);
    await flush(el);
    switchView(el, 'graph');
    await flush(el);
    // The re-issued first load (a drain) and its retries.
    expect(agentsRequests()).toBeGreaterThanOrEqual(2);
    first.resolve(jsonResponse({ agents: [], complete: true }));
    await vi.waitFor(() => expect(text(el!)).toContain('Could not load agents.'));
    expect(text(el)).not.toContain('No Agents');
    expect(el.shadowRoot?.querySelector('.agents-load-error sl-button')).toBeTruthy();
  });
});
