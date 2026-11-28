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
 * Tests for project-detail.ts: Clone / Create Template make a new project,
 * so they need project read AND hub-scope project.create (design §5.F).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities, PageData, UserRole } from '../../shared/types.js';
import { clearAdminStatus } from '../../client/admin-status.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

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

const PROJECT_ID = 'p-1';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(
  opts: {
    projectCaps: Capabilities;
    hubCaps?: Capabilities;
    agents?: Array<Record<string, unknown>>;
  },
  role: UserRole
) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/auth/admin-status')) {
      const isAdmin = role === 'admin';
      return Promise.resolve(jsonResponse({ isAdmin, isSuperAdmin: isAdmin, permissions: [] }));
    }
    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(
        jsonResponse({ projects: [], ...(opts.hubCaps ? { _capabilities: opts.hubCaps } : {}) })
      );
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
      const agents = opts.agents ?? [];
      return Promise.resolve(
        jsonResponse({
          agents,
          totalCount: agents.length,
          complete: true,
          stats: {
            total: agents.length,
            running: agents.filter((a) => a.phase === 'running').length,
            agents: agents.map((a) => [a.id, a.phase]),
          },
          _capabilities: { actions: [] },
        })
      );
    }
    if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
      return Promise.resolve(
        jsonResponse({
          id: PROJECT_ID,
          name: 'Project One',
          slug: 'project-one',
          _capabilities: opts.projectCaps,
        })
      );
    }
    return Promise.resolve(jsonResponse({}, 404));
  };
}

async function createComponent(
  opts: {
    projectCaps: Capabilities;
    hubCaps?: Capabilities;
    agents?: Array<Record<string, unknown>>;
  },
  role: UserRole = 'member'
): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts, role)));
  const el = document.createElement('scion-page-project-detail') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
    projectId: string;
  };
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function headerActionsText(el: HTMLElement): string {
  return el.shadowRoot?.querySelector('.header-actions')?.textContent?.replace(/\s+/g, ' ') ?? '';
}

function hasClone(el: HTMLElement): boolean {
  return /\bClone\b/.test(headerActionsText(el));
}

describe('scion-page-project-detail — Clone / Create Template gating', () => {
  let element: HTMLElement | null = null;

  // project-detail pulls in many sub-components; import it once, outside the
  // per-test timeout, so a loaded CI machine does not time out the first test.
  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    clearAdminStatus();
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders the page header (sanity)', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      hubCaps: { actions: ['create'] },
    });
    expect(element.shadowRoot?.querySelector('.header-actions')).not.toBeNull();
    expect(headerActionsText(element)).toContain('Metrics');
  });

  it('shows Clone with project update and hub create (owner/admin)', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read', 'update'] },
      hubCaps: { actions: ['list', 'create'] },
    });
    expect(hasClone(element)).toBe(true);
  });

  it('hides Clone with only project read and hub create (member)', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      hubCaps: { actions: ['list', 'create'] },
    });
    expect(hasClone(element)).toBe(false);
  });

  it('hides Clone without hub create (viewer)', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read', 'update'] }, hubCaps: { actions: ['list'] } },
      'viewer'
    );
    expect(hasClone(element)).toBe(false);
    // Unrelated header actions still render.
    expect(headerActionsText(element)).toContain('Metrics');
  });

  it('hides Clone without project update even with hub create', async () => {
    element = await createComponent({
      projectCaps: { actions: [] },
      hubCaps: { actions: ['create'] },
    });
    expect(hasClone(element)).toBe(false);
  });

  it('hides Clone when hub caps cannot be loaded (fail-closed)', async () => {
    element = await createComponent({ projectCaps: { actions: ['read', 'update'] } });
    expect(hasClone(element)).toBe(false);
  });

  it('admin with hub create gets the Clone / Create Template dropdown', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read', 'update'] }, hubCaps: { actions: ['create'] } },
      'admin'
    );
    const text = headerActionsText(element);
    expect(text).toContain('Clone Project');
    expect(text).toContain('Create Template');
  });

  it('hides Create Template without hub create, regardless of admin role string', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read', 'update'] }, hubCaps: { actions: [] } },
      'admin'
    );
    const text = headerActionsText(element);
    expect(text).not.toContain('Create Template');
    expect(hasClone(element)).toBe(false);
  });

  it('renders the agents expand toggle as a native button with aria-expanded', async () => {
    localStorage.removeItem('scion-agents-expanded');
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      agents: [
        {
          id: 'a-1',
          name: 'agent-one',
          slug: 'agent-one',
          status: 'running',
          phase: 'running',
          projectId: PROJECT_ID,
          created: '2026-11-28T00:00:00Z',
          updated: '2026-11-28T00:00:00Z',
        },
      ],
    });
    const toggle = element.shadowRoot?.querySelector<HTMLButtonElement>('.agents-expand-toggle');
    expect(toggle).not.toBeNull();
    expect(toggle!.tagName).toBe('BUTTON');
    expect(toggle!.type).toBe('button');
    expect(toggle!.getAttribute('aria-label')).toBe('Expand agents');
    expect(toggle!.getAttribute('aria-expanded')).toBe('false');
    expect(element.shadowRoot?.querySelector('sl-icon-button[aria-expanded]')).toBeNull();

    toggle!.click();
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;

    expect(toggle!.getAttribute('aria-label')).toBe('Collapse agents');
    expect(toggle!.getAttribute('aria-expanded')).toBe('true');
  });
});
