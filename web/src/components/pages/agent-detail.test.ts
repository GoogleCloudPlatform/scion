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
  /** Mirrors the real state manager's tombstone set (state.ts `deletedAgentIds`). */
  private deletedIds = new Set<string>();
  setAgent(agent: { id: string }): void {
    this.agentsById.set(agent.id, agent);
    this.deletedIds.delete(agent.id);
  }
  /** Prune a stale row without a tombstone (mirrors the real `removeAgent`). */
  removeAgent(id: string): void {
    this.agentsById.delete(id);
  }
  /** Simulate a real SSE `deleted` event: tombstone, then remove. */
  deleteAgent(id: string): void {
    this.deletedIds.add(id);
    this.agentsById.delete(id);
  }
  getAgent(id: string): { id: string } | undefined {
    return this.agentsById.get(id);
  }
  getDeletedAgentIds(): Set<string> {
    return this.deletedIds;
  }
  getProject(): undefined {
    return undefined;
  }
  setScope(): void {}
  seedAgents(agents: Array<{ id: string }>): void {
    for (const a of agents) {
      if (!this.deletedIds.has(a.id)) this.agentsById.set(a.id, a);
    }
  }
  seedProjects(): void {}
  /**
   * Mirrors the real `applyDeleteAccepted` (DELETE 202): merge the returned
   * deletion into the known agent and flush, without removing it. The real
   * claim guard (`shouldApplyAcceptedDeletion`) is not reproduced here; it
   * is covered in client/state-deletion.test.ts.
   */
  applyDeleteAccepted(id: string, deletion: unknown): boolean {
    const existing = this.agentsById.get(id);
    if (!deletion || !existing || this.deletedIds.has(id)) return false;
    this.agentsById.set(id, { ...existing, deletion } as { id: string });
    this.notifyAgentsUpdated();
    return true;
  }
  /** Fire the same coalesced event the real state manager dispatches after a flush. */
  notifyAgentsUpdated(): void {
    this.dispatchEvent(new CustomEvent('agents-updated'));
  }
  /** Test-only: reset between specs so state never leaks across them. */
  reset(): void {
    this.agentsById.clear();
    this.deletedIds.clear();
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
// so the action proceeds without a real dialog in the test DOM. Individual
// tests override this with `mockResolvedValueOnce` to exercise a decline.
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

// showToast() creates a real `sl-alert` and calls its `.toast()` method,
// which only exists once Shoelace's element definition is registered. This
// file never loads that definition (it would pull in the real component
// tree), so the failed-delete/declined-force-delete tests — which hit
// the error path — mock the util instead, the same way api.js is mocked.
vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

await import('./agent-detail.js');
import { DELETE_REDIRECT_DELAY_MS } from './agent-detail.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { setPreferredTimeZone } from '../../utils/time.js';
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

let originalLocationDescriptor: PropertyDescriptor | undefined;

/** Stubbed `pathname` is mutable so tests can simulate navigating elsewhere. */
function stubLocation(): { assignedHref: string | undefined; pathname: string } {
  originalLocationDescriptor = Object.getOwnPropertyDescriptor(window, 'location');
  const state = { assignedHref: undefined as string | undefined, pathname: `/agents/${AGENT_ID}` };
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: {
      get pathname() {
        return state.pathname;
      },
      set pathname(v: string) {
        state.pathname = v;
      },
      get href() {
        return state.assignedHref ?? '';
      },
      set href(v: string) {
        state.assignedHref = v;
      },
    },
  });
  return state;
}

describe('scion-page-agent-detail delete navigation (ptone/scion#2480)', () => {
  let navClicks: Array<{ path: string }>;
  let navClickListener: (e: Event) => void;

  beforeEach(() => {
    fakeStateManager.reset();
    vi.mocked(showConfirm).mockClear();
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
    // Restore window.location rather than leaving the stub in place.
    if (originalLocationDescriptor) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
      originalLocationDescriptor = undefined;
    }
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

    // Simulate the SSE `deleted` event: the real state manager tombstones
    // the ID and removes it from its map before dispatching the coalesced
    // 'agents-updated'.
    fakeStateManager.deleteAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    expect(navClicks).toEqual([]);

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(tracker.assignedHref).toBeUndefined();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  it('SPA-redirects after delete when the current path has a trailing slash', async () => {
    const tracker = stubLocation();
    tracker.pathname = `/agents/${AGENT_ID}/`;
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

  it('shows the deleted state before the SPA redirect fires (fake timers)', async () => {
    stubLocation();
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

    // With the timer still pending, "Redirecting…" is shown alongside the
    // deleted state.
    const pendingText =
      el.shadowRoot?.querySelector('[data-testid="agent-deleted-state"]')?.textContent ?? '';
    expect(pendingText).toContain('Redirecting');

    vi.advanceTimersByTime(1);
    await Promise.resolve();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  // --- absence from stateManager is not the same as "deleted" -------------

  it('an agents-updated flush during page load (project fetch held) does not show the deleted state', async () => {
    let resolveProject!: (value: Response) => void;
    const heldProject = new Promise<Response>((resolve) => {
      resolveProject = resolve;
    });
    apiFetch.mockReset();
    apiFetch.mockImplementation((url: string) => {
      if (url === '/api/v1/projects/proj-1') return heldProject;
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
    el.pageData = {
      path: `/agents/${AGENT_ID}`,
      title: 'Agent',
      data: makeAgent({ projectId: 'proj-1' }),
    };
    document.body.appendChild(el);
    await el.updateComplete;

    // loadData is still awaiting the held project fetch, so this page has
    // not reseeded stateManager with its own agent yet — mirrors setScope()
    // clearing the map ahead of that reseed.
    expect((el as unknown as { loading: boolean }).loading).toBe(true);
    expect(fakeStateManager.getAgent(AGENT_ID)).toBeUndefined();

    // An unrelated flush (e.g. a status delta for any agent in the
    // project) arrives in that window.
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(navClicks).toEqual([]);

    resolveProject(okJson({ id: 'proj-1', name: 'Proj One' }));
    await vi.waitFor(() => {
      expect((el as unknown as { loading: boolean }).loading).toBe(false);
    });
  });

  it('an unrelated prune (removeAgent without a tombstone) does not show the deleted state', async () => {
    const el = await mount(makeAgent());
    fakeStateManager.setAgent({ id: AGENT_ID });

    // A stale-row prune (chat.ts's use of removeAgent) with no deletion.
    fakeStateManager.removeAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(navClicks).toEqual([]);
  });

  // --- the redirect must not fire off-route or while hidden ---------------

  it('an SSE delete while hidden behind /terminals does not pull the user out of the terminal workspace', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const tracker = stubLocation();
    const el = await mount(makeAgent());
    fakeStateManager.setAgent({ id: AGENT_ID });

    // renderRoute (main.ts) keeps this page connected-but-hidden behind
    // /terminals rather than disconnecting it.
    tracker.pathname = '/terminals';

    fakeStateManager.deleteAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    // No stray SPA navigation pulls the user out of /terminals.
    expect(navClicks).toEqual([]);
    // The deleted state still offers a way out, instead of "Redirecting…" forever.
    expect(el.shadowRoot?.querySelector('[data-testid="agent-deleted-link"]')).not.toBeNull();
  });

  it('opening /terminals within the 1s delete-redirect window suppresses the redirect', async () => {
    const tracker = stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');

    // The user opens the terminal workspace before the 1s delay elapses.
    tracker.pathname = '/terminals';

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(tracker.assignedHref).toBeUndefined();
    expect(navClicks).toEqual([]);
  });

  // --- guards that already existed, now pinned by a test ------------------

  it('a failed delete (non-502/503) does not enter the deleted state', async () => {
    stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: false,
        status: 500,
        json: () => Promise.resolve({}),
      } as unknown as Response)
    );

    await internals.handleAction('delete');
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(navClicks).toEqual([]);
  });

  it('a declined force-delete does not enter the deleted state', async () => {
    stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: false,
        status: 502,
        json: () => Promise.resolve({}),
      } as unknown as Response)
    );
    // Confirm the delete prompt, then decline the force-delete prompt.
    vi.mocked(showConfirm).mockResolvedValueOnce(true).mockResolvedValueOnce(false);

    await internals.handleAction('delete');
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(navClicks).toEqual([]);
  });

  it('a local delete success followed by an SSE delete produces exactly one nav-click', async () => {
    stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };
    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');

    // An SSE delete for the same agent arrives before the redirect fires;
    // the `deleted` guard must make this a no-op.
    fakeStateManager.deleteAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  it('removing the element during the delay produces no nav-click', async () => {
    stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };
    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');

    el.remove();

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();

    expect(navClicks).toEqual([]);
  });

  it(
    'after a skipped redirect (hidden behind /terminals, then back) shows no ' +
      '"Redirecting" text and still shows the link',
    async () => {
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      const tracker = stubLocation();
      const el = await mount(makeAgent());
      fakeStateManager.setAgent({ id: AGENT_ID });

      // renderRoute (main.ts) keeps this page connected-but-hidden behind
      // /terminals rather than disconnecting it.
      tracker.pathname = '/terminals';

      fakeStateManager.deleteAgent(AGENT_ID);
      fakeStateManager.notifyAgentsUpdated();
      await el.updateComplete;

      // The timer fires while still hidden, so the redirect is skipped and
      // the timer is cleared without ever navigating.
      vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
      await Promise.resolve();
      await el.updateComplete;
      expect(navClicks).toEqual([]);

      // Coming back to this agent's own route does not re-arm the timer.
      tracker.pathname = `/agents/${AGENT_ID}`;
      await el.updateComplete;

      const text =
        el.shadowRoot?.querySelector('[data-testid="agent-deleted-state"]')?.textContent ?? '';
      expect(text).not.toContain('Redirecting');
      expect(text).toContain('Agent deleted.');
      expect(el.shadowRoot?.querySelector('[data-testid="agent-deleted-link"]')).not.toBeNull();
      expect(navClicks).toEqual([]);
    }
  );

  // --- reconnecting while deleted must not strand the page ---------------

  it('disconnecting and reconnecting while deleted leaves a way out, with no stray timer', async () => {
    stubLocation();
    const el = await mount(makeAgent());
    const internals = el as unknown as {
      handleAction(action: string, event?: MouseEvent): Promise<void>;
    };
    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');
    await el.updateComplete;

    el.remove(); // disconnectedCallback clears the pending redirect timer
    document.body.appendChild(el); // reconnected; no timer re-arms
    await el.updateComplete;

    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    expect(el.shadowRoot?.querySelector('[data-testid="agent-deleted-link"]')).not.toBeNull();

    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS * 10);
    await Promise.resolve();
    expect(navClicks).toEqual([]);
  });
});

describe('scion-page-agent-detail times in the display zone (tz-refactor task 19)', () => {
  afterEach(() => {
    setPreferredTimeZone('');
  });

  it('formats absolute times in the preferred zone, 24-hour, with a zone label', () => {
    // Browser zone is pinned to UTC; 15:00Z is midnight in Asia/Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-page-agent-detail') as any;
    expect(el.formatDate('2026-10-01T15:00:00Z')).toBe('Oct 2, 2026, 00:00 (Asia/Tokyo)');
  });

  it('keeps its em dash for a zero time', () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-page-agent-detail') as any;
    expect(el.formatDate('0001-01-01T00:00:00Z')).toBe('—');
    expect(el.formatRelativeTime('0001-01-01T00:00:00Z')).toBe('—');
  });
});

describe('scion-page-agent-detail backend-driven delete (ptone/scion#2483 phase 1b)', () => {
  type DeletionInfo = import('../../shared/types.js').DeletionInfo;
  const T0 = Date.parse('2026-10-04T12:00:00Z');
  const iso = (ms: number): string => new Date(ms).toISOString();
  const deletingView = (leaseMs = T0 + 20_000): DeletionInfo => ({
    state: 'deleting',
    soft: false,
    claim: 1,
    startedAt: iso(T0),
    leaseExpiresAt: iso(leaseMs),
  });
  const accepted = (deletion: DeletionInfo): Response =>
    ({
      ok: true,
      status: 202,
      json: () => Promise.resolve({ agentId: AGENT_ID, deletion }),
    }) as unknown as Response;

  let navClicks: Array<{ path: string }>;
  let navClickListener: (e: Event) => void;

  beforeEach(() => {
    fakeStateManager.reset();
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
    if (originalLocationDescriptor) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
      originalLocationDescriptor = undefined;
    }
  });

  const actionable = (overrides: Partial<Agent> = {}): Agent =>
    makeAgent({ _capabilities: { actions: ['read', 'lifecycle', 'delete'] }, ...overrides });

  /** Header action labels; the icon-only Delete button reads as "delete". */
  function headerActions(el: ScionPageAgentDetail): string[] {
    const buttons = el.shadowRoot?.querySelectorAll('.header sl-button') ?? [];
    return [...buttons].map((b) =>
      b.querySelector('sl-icon[name="trash"]') ? 'delete' : (b.textContent ?? '').trim()
    );
  }

  function headerBadge(el: ScionPageAgentDetail): string | null {
    const badge = el.shadowRoot?.querySelector('.header scion-deletion-badge');
    const inner = badge?.shadowRoot?.querySelector('.badge');
    return inner ? (inner.textContent ?? '').trim() : null;
  }

  async function settle(el: ScionPageAgentDetail): Promise<void> {
    await el.updateComplete;
    const badge = el.shadowRoot?.querySelector('.header scion-deletion-badge') as
      | (HTMLElement & { updateComplete: Promise<boolean> })
      | null;
    await badge?.updateComplete;
  }

  it('202 keeps the page, shows Deleting…, hides actions; SSE deleted then redirects', async () => {
    stubLocation();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], now: T0 });
    const agent = actionable();
    const el = await mount(agent);
    fakeStateManager.setAgent(agent);
    expect(headerActions(el)).toEqual(expect.arrayContaining(['Stop', 'delete']));

    const internals = el as unknown as { handleAction(action: string): Promise<void> };
    apiFetch.mockImplementationOnce(() => Promise.resolve(accepted(deletingView())));
    await internals.handleAction('delete');
    await settle(el);

    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(headerBadge(el)).toBe('Deleting…');
    const actions = headerActions(el);
    for (const hidden of ['Stop', 'Suspend', 'Start', 'Resume', 'delete']) {
      expect(actions).not.toContain(hidden);
    }
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS * 2);
    expect(navClicks).toEqual([]);

    fakeStateManager.deleteAgent(AGENT_ID);
    fakeStateManager.notifyAgentsUpdated();
    await el.updateComplete;
    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });

  it('a failed delta shows "Delete failed" and re-enables actions; deletion:null clears it', async () => {
    stubLocation();
    const agent = actionable({ deletion: deletingView(Date.now() + 60_000) });
    const el = await mount(agent);
    fakeStateManager.setAgent(agent);
    await settle(el);
    expect(headerBadge(el)).toBe('Deleting…');
    expect(headerActions(el)).not.toContain('delete');

    fakeStateManager.setAgent({
      ...agent,
      deletion: { ...deletingView(), state: 'failed', code: 'runtime_error', error: 'boom' },
    } as Agent);
    fakeStateManager.notifyAgentsUpdated();
    await settle(el);
    expect(headerBadge(el)).toBe('Delete failed: boom');
    expect(headerActions(el)).toEqual(expect.arrayContaining(['Stop', 'delete']));

    fakeStateManager.setAgent({ ...agent, deletion: null } as Agent);
    fakeStateManager.notifyAgentsUpdated();
    await settle(el);
    expect(headerBadge(el)).toBeNull();
    expect(headerActions(el)).toEqual(expect.arrayContaining(['Stop', 'delete']));
  });

  it('flips to "Delete interrupted" at leaseExpiresAt with no new data, and re-enables actions', async () => {
    stubLocation();
    const agent = actionable();
    const el = await mount(agent);
    // Fake timers only after mount: mount's vi.waitFor advances fake time.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], now: T0 });
    fakeStateManager.setAgent({ ...agent, deletion: deletingView(T0 + 20_000) } as Agent);
    fakeStateManager.notifyAgentsUpdated();
    await settle(el);
    expect(headerBadge(el)).toBe('Deleting…');

    vi.advanceTimersByTime(19_999);
    await settle(el);
    expect(headerBadge(el)).toBe('Deleting…');

    vi.advanceTimersByTime(1);
    await settle(el);
    expect(headerBadge(el)).toBe('Delete interrupted');
    expect(headerActions(el)).toEqual(expect.arrayContaining(['Stop', 'delete']));
  });

  it('force DELETE 202 (502, confirm, force 202) keeps the page with Deleting… and no redirect', async () => {
    stubLocation();
    const agent = actionable();
    const el = await mount(agent);
    fakeStateManager.setAgent(agent);
    const internals = el as unknown as { handleAction(action: string): Promise<void> };
    apiFetch
      .mockImplementationOnce(() =>
        Promise.resolve({
          ok: false,
          status: 502,
          json: () => Promise.resolve({}),
        } as unknown as Response)
      )
      .mockImplementationOnce(() => Promise.resolve(accepted(deletingView(Date.now() + 60_000))));

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');
    await settle(el);

    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/agents/${AGENT_ID}?force=true`, {
      method: 'DELETE',
    });
    expect((el as unknown as { deleted: boolean }).deleted).toBe(false);
    expect(headerBadge(el)).toBe('Deleting…');
    expect(headerActions(el)).not.toContain('delete');
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS * 2);
    expect(navClicks).toEqual([]);
  });

  it('204 still shows the deleted state and redirects as before', async () => {
    stubLocation();
    const el = await mount(actionable());
    const internals = el as unknown as { handleAction(action: string): Promise<void> };
    apiFetch.mockImplementationOnce(() => Promise.resolve(noContent()));
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    await internals.handleAction('delete');
    expect((el as unknown as { deleted: boolean }).deleted).toBe(true);
    vi.advanceTimersByTime(DELETE_REDIRECT_DELAY_MS);
    await Promise.resolve();
    expect(navClicks).toEqual([{ path: '/agents' }]);
  });
});
