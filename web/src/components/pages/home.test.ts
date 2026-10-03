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
 * Tests for scion-page-home — focused on invite-stats 403 suppression (#1733).
 *
 * The dashboard calls `apiFetch('/api/v1/admin/invites/stats')` for admin
 * users. When the user lacks the specific permission, the endpoint returns 403.
 * The call must use `suppressAccessDeniedToast: true` so no toast fires (and
 * thus no double-removal NotFoundError occurs).
 */

import { describe, it, expect, vi, afterEach, beforeAll, beforeEach } from 'vitest';
import type { AccessDeniedDetail } from '../../client/api.js';
import type { Agent, Capabilities, Project } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

// We test the suppression behavior by intercepting fetch and the
// scion:access-denied event — no need to render the full Lit component.

describe('dashboard invite-stats 403 suppression (#1733)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let accessDeniedEvents: AccessDeniedDetail[];
  const accessDeniedListener = (e: Event) => {
    accessDeniedEvents.push((e as CustomEvent<AccessDeniedDetail>).detail);
  };

  beforeEach(() => {
    accessDeniedEvents = [];
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    window.addEventListener('scion:access-denied', accessDeniedListener);
  });

  afterEach(() => {
    window.removeEventListener('scion:access-denied', accessDeniedListener);
    vi.restoreAllMocks();
  });

  it('invite-stats 403 does not produce scion:access-denied event', async () => {
    // Import apiFetch fresh so it uses our mocked fetch
    const { apiFetch } = await import('../../client/api.js');

    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          error: {
            code: 'forbidden',
            message: 'Insufficient permissions',
            details: {
              resource_type: 'invite_stats',
              denied_action: 'read',
            },
          },
        }),
        { status: 403, headers: { 'Content-Type': 'application/json' } }
      )
    );

    // This mirrors the exact call in home.ts loadData()
    await apiFetch('/api/v1/admin/invites/stats', {
      suppressAccessDeniedToast: true,
    }).catch(() => null);

    // No access-denied event should fire
    expect(accessDeniedEvents).toHaveLength(0);
  });

  it('unsuppressed 403 from a different endpoint still fires access-denied event', async () => {
    const { apiFetch } = await import('../../client/api.js');

    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          error: {
            code: 'forbidden',
            message: 'Insufficient permissions',
            details: {
              resource_type: 'agent',
              denied_action: 'delete',
            },
          },
        }),
        { status: 403, headers: { 'Content-Type': 'application/json' } }
      )
    );

    // A normal apiFetch call WITHOUT suppressAccessDeniedToast
    await apiFetch('/api/v1/agents/test-agent');

    // Access-denied event should fire normally
    expect(accessDeniedEvents).toHaveLength(1);
    expect(accessDeniedEvents[0].action).toBe('delete');
    expect(accessDeniedEvents[0].resource).toBe('agent');
  });
});

/* ========================================================================== */
/* Create Project quick action gated on hub-scope project.create       */
/* ========================================================================== */

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

/** Fetch stub: the dashboard list calls plus the helper's ?limit=1 call. */
function dashboardFetch(projectCaps: Capabilities | undefined) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/projects')) {
      return Promise.resolve(
        jsonResponse({
          projects: [],
          ...(projectCaps ? { _capabilities: projectCaps } : {}),
        })
      );
    }
    if (path.includes('/api/v1/agents')) {
      return Promise.resolve(jsonResponse({ agents: [] }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

async function mountHome(): Promise<HTMLElement> {
  const el = document.createElement('scion-page-home') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: unknown;
  };
  el.pageData = { path: '/', title: 'Dashboard', user: { id: 'u', email: 'u@x', name: 'U' } };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function actionTitles(el: HTMLElement): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.action-card h4') ?? []).map(
    (h) => h.textContent?.trim() ?? ''
  );
}

function hasCreateProjectLink(el: HTMLElement): boolean {
  return !!el.shadowRoot?.querySelector('a.action-card[href="/projects/new"]');
}

describe('dashboard Create Project card (hub project.create)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./home.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // Leave the dashboard scope so its state maps are cleared between tests.
    stateManager.setScope({ type: 'brokers-list' });
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows Create Project when hub caps include create', async () => {
    vi.stubGlobal('fetch', vi.fn(dashboardFetch({ actions: ['list', 'create'] })));
    element = await mountHome();

    expect(hasCreateProjectLink(element)).toBe(true);
    expect(actionTitles(element)).toContain('Create Project');
  });

  it('hides Create Project when hub caps lack create (viewer)', async () => {
    vi.stubGlobal('fetch', vi.fn(dashboardFetch({ actions: ['list'] })));
    element = await mountHome();

    expect(hasCreateProjectLink(element)).toBe(false);
    expect(actionTitles(element)).not.toContain('Create Project');
    // Other quick actions are unaffected.
    expect(actionTitles(element)).toContain('View Projects');
  });

  it('hides Create Project when the response carries no capabilities (fail-closed)', async () => {
    vi.stubGlobal('fetch', vi.fn(dashboardFetch(undefined)));
    element = await mountHome();

    expect(hasCreateProjectLink(element)).toBe(false);
  });

  it('reuses the dashboard projects fetch: no extra capabilities request', async () => {
    const fetchMock = vi.fn(dashboardFetch({ actions: ['create'] }));
    vi.stubGlobal('fetch', fetchMock);
    element = await mountHome();

    const projectCalls = fetchMock.mock.calls
      .map(([u]) => String(u))
      .filter((u) => u.includes('/api/v1/projects'));
    expect(projectCalls).toHaveLength(1);
    expect(projectCalls[0]).not.toContain('limit=1');
  });

  it('uses project scope caps already in state without fetching', async () => {
    const fetchMock = vi.fn(dashboardFetch({ actions: [] }));
    vi.stubGlobal('fetch', fetchMock);
    stateManager.setScope({ type: 'dashboard' });
    stateManager.seedProjects([{ id: 'p1', name: 'P1' } as Project]);
    stateManager.seedScopeCapabilities('project', { actions: ['create'] });

    element = await mountHome();

    expect(hasCreateProjectLink(element)).toBe(true);
    expect(fetchMock.mock.calls.map(([u]) => String(u))).not.toContainEqual(
      expect.stringContaining('/api/v1/projects')
    );
  });

  it('falls back to the helper when hydrated state has no project caps', async () => {
    const fetchMock = vi.fn(dashboardFetch({ actions: ['list'] }));
    vi.stubGlobal('fetch', fetchMock);
    stateManager.setScope({ type: 'dashboard' });
    stateManager.seedAgents([{ id: 'a1', name: 'A1' } as Agent]);
    // Hydrated from the agents page's complete load.
    stateManager.markAgentSetComplete('full');

    element = await mountHome();

    expect(fetchMock.mock.calls.map(([u]) => String(u))).toContainEqual(
      expect.stringContaining('/api/v1/projects?limit=1')
    );
    expect(hasCreateProjectLink(element)).toBe(false);
  });
});
