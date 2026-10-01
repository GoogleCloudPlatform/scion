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
 * Regression tests for ptone/scion#2480: deleting an agent from its detail
 * page used to assign `window.location.href`, forcing a full document
 * reload and losing all other in-browser state (open terminal panes, chat
 * tabs, etc). The fix shows a brief client-side-only "deleted" state and
 * then navigates within the SPA (`nav-click`), both when this page
 * initiates the delete and when the agent is deleted elsewhere (observed
 * via an SSE `deleted` event through `onAgentsUpdated`).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

/** Stand-in for the global stateManager: only the surface agent-detail.ts uses. */
class FakeStateManager extends EventTarget {
  private agentsById = new Map<string, { id: string }>();
  setAgent(agent: { id: string }): void {
    this.agentsById.set(agent.id, agent);
  }
  removeAgent(id: string): void {
    this.agentsById.delete(id);
  }
  getAgent(id: string): { id: string } | undefined {
    return this.agentsById.get(id);
  }
  getProject(): undefined {
    return undefined;
  }
  setScope(): void {}
  seedAgents(agents: Array<{ id: string }>): void {
    for (const a of agents) this.agentsById.set(a.id, a);
  }
  seedProjects(): void {}
  /** Fire the same coalesced event the real state manager dispatches after a flush. */
  notifyAgentsUpdated(): void {
    this.dispatchEvent(new CustomEvent('agents-updated'));
  }
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();

vi.mock('../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: () => Promise.resolve('error'),
}));

vi.mock('../../client/state.js', () => ({
  get stateManager() {
    return fakeStateManager;
  },
}));

// chat-thread.ts (pulled in transitively via the `../shared/chat/chat-thread.js`
// side-effect import in agent-detail.ts) gets its stateManager from
// client/main.js, not client/state.js directly. Mock it the same way so the
// real main.ts — with its SSE/terminal-workspace singleton side effects —
// never loads in this test.
vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  get stateManager() {
    return fakeStateManager;
  },
}));

// Auto-confirm every showConfirm() call (the delete and force-delete prompts)
// so the action proceeds without a real dialog in the test DOM.
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

await import('./agent-detail.js');
import { DELETE_REDIRECT_DELAY_MS } from './agent-detail.js';
type ScionPageAgentDetail = import('./agent-detail.js').ScionPageAgentDetail;
type Agent = import('../../shared/types.js').Agent;

const AGENT_ID = 'agent-1';

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: AGENT_ID,
    name: 'Test Agent',
    projectId: '',
    template: 'default',
    phase: 'running',
    ...overrides,
  };
}

function okJson(body: unknown): Response {
  return { ok: true, status: 200, json: () => Promise.resolve(body) } as unknown as Response;
}

function noContent(): Response {
  return { ok: true, status: 204, json: () => Promise.resolve(null) } as unknown as Response;
}

/**
 * Mount the page with an SSR-prefetched agent so no initial GET is needed.
 * `project`, if given, answers the `GET /api/v1/projects/{projectId}` call
 * `loadData` makes when the agent has a `projectId`; anything else (metrics,
 * auth/me, etc.) is optional in the component and should degrade quietly.
 */
async function mount(
  agent: Agent,
  project?: { id: string; name: string }
): Promise<ScionPageAgentDetail> {
  apiFetch.mockReset();
  apiFetch.mockImplementation((url: string) => {
    if (project && url === `/api/v1/projects/${project.id}`) {
      return Promise.resolve(okJson(project));
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      json: () => Promise.resolve({}),
    } as unknown as Response);
  });

  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail & {
    pageData: unknown;
    agentId: string;
  };
  el.agentId = AGENT_ID;
  el.pageData = { path: `/agents/${AGENT_ID}`, title: 'Agent', data: agent };
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => {
    expect((el as unknown as { loading: boolean }).loading).toBe(false);
  });
  await el.updateComplete;
  return el;
}

function stubLocation(): { assignedHref: string | undefined } {
  const tracker = { assignedHref: undefined as string | undefined };
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: {
      pathname: `/agents/${AGENT_ID}`,
      get href() {
        return tracker.assignedHref ?? '';
      },
      set href(v: string) {
        tracker.assignedHref = v;
      },
    },
  });
  return tracker;
}

describe('scion-page-agent-detail delete navigation (ptone/scion#2480)', () => {
  let navClicks: Array<{ path: string }>;
  let navClickListener: (e: Event) => void;

  beforeEach(() => {
    fakeStateManager.removeAgent(AGENT_ID);
    navClicks = [];
    navClickListener = (e: Event) => {
      navClicks.push((e as CustomEvent<{ path: string }>).detail);
    };
    document.addEventListener('nav-click', navClickListener);
  });

  afterEach(() => {
    document.removeEventListener('nav-click', navClickListener);
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  it('delete: does not assign location.href and requests SPA navigation', async () => {
    const tracker = stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(tracker.assignedHref).toBeUndefined();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  it('force-delete: does not assign location.href and requests SPA navigation', async () => {
    const tracker = stubLocation();
    const el = await mount(makeAgent({ projectId: 'proj-1' }), { id: 'proj-1', name: 'Proj One' });
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };

    // First DELETE fails with 502 (broker unreachable) -> force-delete prompt
    // (auto-confirmed) -> second DELETE with ?force=true succeeds.
    apiFetch
      .mockImplementationOnce(() =>
        Promise.resolve({
          ok: false,
          status: 502,
          json: () => Promise.resolve({}),
        } as unknown as Response)
      )
      .mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(tracker.assignedHref).toBeUndefined();
    expect(navClicks).toEqual([{ path: '/projects/proj-1' }]);
  });

  it('SSE removal of the current agent shows the deleted state and then SPA-redirects', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const tracker = stubLocation();
    const el = await mount(makeAgent());
    fakeStateManager.setAgent({ id: AGENT_ID });

    // Simulate the SSE `deleted` event: the real state manager removes the
    // agent from its map before dispatching the coalesced 'agents-updated'.
    fakeStateManager.removeAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    expect(navClicks).toEqual([]);

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(tracker.assignedHref).toBeUndefined();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  it('shows the deleted state before the SPA redirect fires (fake timers)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    await internals.handleAction('delete');
    await el.updateComplete;

    // The deleted state is visible immediately ...
    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    expect(el.shadowRoot?.querySelector('[data-testid="agent-deleted-state"]')).not.toBeNull();
    expect(navClicks).toEqual([]);

    // ... and the redirect only fires once the delay elapses.
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS - 1);
    expect(navClicks).toEqual([]);

    vi.advanceTimersByTime(1);
    await Promise.resolve();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });
});
