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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { clearAdminStatus } from '../../client/admin-status.js';
import {
  resetServerFlagStateForTests,
  setServerFlags,
  githubTriggersEnabled,
} from '../../utils/feature-flags.js';
import { GITHUB_ADMIN_SECTIONS } from '../pages/admin-integrations-github-sections.js';
import type { GitHubRepoRoutingEntry, ScionAdminGitHubRouting } from './admin-github-routing.js';

function jsonResponse(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(status === 204 ? null : JSON.stringify(body), {
      status,
      headers: status === 204 ? {} : { 'Content-Type': 'application/json' },
    })
  );
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function setSelectValue(select: Element, value: string): void {
  (select as HTMLElement & { value: string }).value = value;
  select.dispatchEvent(new CustomEvent('sl-change', { bubbles: true }));
}

const SAMPLE_ENTRIES: GitHubRepoRoutingEntry[] = [
  {
    installation_id: 4412,
    repo: 'acme/api',
    projects: [
      { id: 'proj-api', name: 'api', has_github_triggers: true },
      { id: 'proj-api-staging', name: 'api-staging', has_github_triggers: false },
    ],
    primary_project_id: 'proj-api',
    status: 'ok',
  },
  {
    installation_id: 4412,
    repo: 'acme/web',
    projects: [
      { id: 'proj-web', name: 'web', has_github_triggers: true },
      { id: 'proj-web-alice', name: 'web-alice', has_github_triggers: true },
    ],
    primary_project_id: null,
    status: 'none',
  },
  {
    installation_id: 9001,
    repo: 'beta/service',
    projects: [{ id: 'proj-svc', name: 'service', has_github_triggers: false }],
    primary_project_id: 'proj-deleted',
    status: 'stale_primary',
  },
];

describe('scion-admin-github-routing', () => {
  let element: ScionAdminGitHubRouting | null = null;

  beforeAll(async () => {
    await import('./admin-github-routing.js');
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    clearAdminStatus();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    clearAdminStatus();
    vi.restoreAllMocks();
  });

  function enableGitHubTriggers(): void {
    setServerFlags({ 'hub.triggers': true, 'hub.github_triggers': true });
  }

  async function mount(
    handler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
  ): Promise<ScionAdminGitHubRouting> {
    vi.stubGlobal('fetch', vi.fn(handler));
    const el = document.createElement('scion-admin-github-routing') as ScionAdminGitHubRouting;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('is registered in GITHUB_ADMIN_SECTIONS', () => {
    const entry = GITHUB_ADMIN_SECTIONS.find((s) => s.id === 'routing');
    expect(entry).toBeDefined();
    expect(entry?.tag).toBe('scion-admin-github-routing');
  });

  it('is hidden and makes no requests when hub.github_triggers is off', async () => {
    setServerFlags({ 'hub.triggers': true, 'hub.github_triggers': false });
    expect(githubTriggersEnabled()).toBe(false);

    const fetchMock = vi.fn(() => jsonResponse(SAMPLE_ENTRIES));
    vi.stubGlobal('fetch', fetchMock);

    element = document.createElement('scion-admin-github-routing') as ScionAdminGitHubRouting;
    document.body.appendChild(element);
    await element.updateComplete;

    expect(element.hidden).toBe(true);
    expect(query(element, '.section')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('is hidden and makes no requests when hub.triggers is off even if hub.github_triggers is on', async () => {
    setServerFlags({ 'hub.triggers': false, 'hub.github_triggers': true });
    expect(githubTriggersEnabled()).toBe(false);

    const fetchMock = vi.fn(() => jsonResponse(SAMPLE_ENTRIES));
    vi.stubGlobal('fetch', fetchMock);

    element = document.createElement('scion-admin-github-routing') as ScionAdminGitHubRouting;
    document.body.appendChild(element);
    await element.updateComplete;

    expect(element.hidden).toBe(true);
    expect(query(element, '.section')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders rows, has_github_triggers badges, statuses, multi-project warnings, and installation filter', async () => {
    enableGitHubTriggers();
    element = await mount(() => jsonResponse(SAMPLE_ENTRIES));

    await vi.waitFor(() => {
      expect(queryAll(element!, '[data-routing-row]')).toHaveLength(3);
    });

    const apiRow = query(element, '[data-routing-row="4412:acme/api"]')!;
    expect(apiRow.textContent).toContain('acme/api');
    expect(
      apiRow.querySelector('[data-project-id="proj-api"] [data-has-github-triggers]')
    ).not.toBeNull();
    expect(
      apiRow.querySelector('[data-project-id="proj-api-staging"] [data-has-github-triggers]')
    ).toBeNull();
    expect(apiRow.querySelector('[data-routing-status="ok"]')).not.toBeNull();
    expect(apiRow.querySelector('[data-multi-project-warning]')).toBeNull();

    const webRow = query(element, '[data-routing-row="4412:acme/web"]')!;
    expect(webRow.querySelector('[data-routing-status="none"]')).not.toBeNull();
    expect(webRow.querySelector('[data-multi-project-warning]')).not.toBeNull();

    const svcRow = query(element, '[data-routing-row="9001:beta/service"]')!;
    expect(svcRow.querySelector('[data-routing-status="stale_primary"]')).not.toBeNull();
    expect(svcRow.querySelector('[data-multi-project-warning]')).toBeNull();

    // Filter by installation 9001
    const filterSelect = query(element, '[data-installation-filter]')!;
    setSelectValue(filterSelect, '9001');
    await element.updateComplete;
    expect(queryAll(element, '[data-routing-row]')).toHaveLength(1);
    expect(query(element, '[data-routing-row="9001:beta/service"]')).not.toBeNull();

    // Reset filter to all installations
    setSelectValue(filterSelect, '');
    await element.updateComplete;
    expect(queryAll(element, '[data-routing-row]')).toHaveLength(3);
  });

  it('sets a primary project via PUT and clears it via DELETE', async () => {
    enableGitHubTriggers();
    const calls: Array<{ method: string; path: string; body?: unknown }> = [];

    element = await mount((url, init) => {
      const path =
        typeof url === 'string' ? url : url instanceof URL ? url.pathname + url.search : url.url;
      const method = init?.method ?? 'GET';
      const body = init?.body ? (JSON.parse(init.body as string) as unknown) : undefined;
      calls.push({ method, path, body });

      if (method === 'PUT') {
        return jsonResponse({
          ...SAMPLE_ENTRIES[1],
          primary_project_id: 'proj-web',
          status: 'ok',
        });
      }
      if (method === 'DELETE') {
        return jsonResponse(null, 204);
      }
      return jsonResponse(SAMPLE_ENTRIES);
    });

    await vi.waitFor(() => {
      expect(queryAll(element!, '[data-routing-row]')).toHaveLength(3);
    });

    // Set primary project on acme/web
    const webSelect = query(element, '[data-routing-select="4412:acme/web"]')!;
    setSelectValue(webSelect, 'proj-web');

    await vi.waitFor(() => {
      expect(calls.some((c) => c.method === 'PUT')).toBe(true);
    });

    const putCall = calls.find((c) => c.method === 'PUT')!;
    expect(putCall.path).toBe('/api/v1/github-app/repo-routing/acme/web');
    expect(putCall.body).toEqual({
      installation_id: 4412,
      primary_project_id: 'proj-web',
    });

    await vi.waitFor(() => {
      const webRow = query(element!, '[data-routing-row="4412:acme/web"]')!;
      expect(webRow.querySelector('[data-routing-status="ok"]')).not.toBeNull();
      expect(webRow.querySelector('[data-multi-project-warning]')).toBeNull();
    });

    // Clear primary project on acme/api ("fan out to all")
    const apiSelect = query(element, '[data-routing-select="4412:acme/api"]')!;
    setSelectValue(apiSelect, '');

    await vi.waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE')).toBe(true);
    });

    const deleteCall = calls.find((c) => c.method === 'DELETE')!;
    expect(deleteCall.path).toBe('/api/v1/github-app/repo-routing/acme/api?installation=4412');
    expect(deleteCall.body).toBeUndefined();

    await vi.waitFor(() => {
      const apiRow = query(element!, '[data-routing-row="4412:acme/api"]')!;
      expect(apiRow.querySelector('[data-routing-status="none"]')).not.toBeNull();
      expect(apiRow.querySelector('[data-multi-project-warning]')).not.toBeNull();
    });
  });

  it('handles 409 not_a_candidate by re-fetching and displaying an inline error', async () => {
    enableGitHubTriggers();
    let getCount = 0;

    element = await mount((url, init) => {
      const method = init?.method ?? 'GET';
      if (method === 'PUT') {
        return jsonResponse(
          {
            error: {
              code: 'not_a_candidate',
              message:
                'project is not a current candidate for this repository under this installation',
            },
          },
          409
        );
      }
      getCount++;
      if (getCount === 1) {
        return jsonResponse(SAMPLE_ENTRIES);
      }
      // Re-fetched state reflects that proj-web-alice was removed
      return jsonResponse([
        {
          ...SAMPLE_ENTRIES[1],
          projects: [{ id: 'proj-web', name: 'web', has_github_triggers: true }],
        },
      ]);
    });

    await vi.waitFor(() => {
      expect(queryAll(element!, '[data-routing-row]')).toHaveLength(3);
    });

    const webSelect = query(element, '[data-routing-select="4412:acme/web"]')!;
    setSelectValue(webSelect, 'proj-web-alice');

    await vi.waitFor(() => {
      expect(getCount).toBe(2);
    });
    await element.updateComplete;

    const errEl = query(element, '[data-routing-error]');
    expect(errEl).not.toBeNull();
    expect(errEl?.textContent).toContain('not a current candidate');
    expect(queryAll(element, '[data-routing-row]')).toHaveLength(1);
  });

  it('switches to read-only mode on 403 from PUT without hub.github_app.update', async () => {
    enableGitHubTriggers();

    element = await mount((_url, init) => {
      if (init?.method === 'PUT') {
        return jsonResponse(
          { error: { code: 'forbidden', message: 'missing hub.github_app.update' } },
          403
        );
      }
      return jsonResponse(SAMPLE_ENTRIES);
    });

    await vi.waitFor(() => {
      expect(queryAll(element!, '[data-routing-select]')).toHaveLength(3);
    });

    const webSelect = query(element, '[data-routing-select="4412:acme/web"]')!;
    setSelectValue(webSelect, 'proj-web');

    await vi.waitFor(() => {
      expect(query(element!, '[data-routing-readonly]')).not.toBeNull();
    });

    expect(query(element, '[data-routing-readonly]')?.textContent).toContain(
      'hub.github_app.update'
    );
    expect(queryAll(element, '[data-routing-select]')).toHaveLength(0);
    expect(queryAll(element, '[data-primary-readonly]')).toHaveLength(3);
    expect(shadowText(element)).toContain('— fan out to all —');
  });

  it('renders read-only up front when admin status lacks hub.github_app.update', async () => {
    enableGitHubTriggers();
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/auth/admin-status')) {
          return jsonResponse({
            isAdmin: true,
            isSuperAdmin: false,
            permissions: ['hub.github_app.read'],
          });
        }
        return jsonResponse(SAMPLE_ENTRIES);
      })
    );

    element = document.createElement('scion-admin-github-routing') as ScionAdminGitHubRouting;
    element.pageData = {
      path: '/admin/integrations/github',
      title: 'Integrations',
      user: {
        id: 'user-ro',
        email: 'reader@example.com',
        name: 'Reader',
        role: 'member',
      },
    };
    document.body.appendChild(element);

    await vi.waitFor(() => {
      expect(queryAll(element!, '[data-routing-row]')).toHaveLength(3);
      expect(query(element!, '[data-routing-readonly]')).not.toBeNull();
    });

    expect(queryAll(element, '[data-routing-select]')).toHaveLength(0);
    expect(queryAll(element, '[data-primary-readonly]')).toHaveLength(3);
  });

  it('renders read-permission message without empty state when initial GET returns 403', async () => {
    enableGitHubTriggers();

    element = await mount(() =>
      jsonResponse({ error: { code: 'forbidden', message: 'forbidden' } }, 403)
    );

    await vi.waitFor(() => {
      expect(query(element!, '[data-routing-forbidden-read]')).not.toBeNull();
    });
    expect(query(element, '[data-routing-forbidden-read]')?.textContent).toContain(
      'hub.github_app.read'
    );
    expect(queryAll(element, '[data-routing-select]')).toHaveLength(0);
    expect(shadowText(element)).not.toContain('No repositories');
  });

  it('ignores stale GET responses when a newer loadRouting completes first', async () => {
    enableGitHubTriggers();
    let resolveFirst!: (res: Response) => void;
    let callCount = 0;

    element = await mount(() => {
      callCount++;
      if (callCount === 1) {
        return new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        });
      }
      return jsonResponse([SAMPLE_ENTRIES[0]]);
    });

    await vi.waitFor(() => {
      expect(callCount).toBe(1);
    });

    // Trigger a newer refresh that resolves immediately
    const refreshPromise = element.loadRouting({ refresh: true });
    await refreshPromise;
    await element.updateComplete;

    expect(queryAll(element, '[data-routing-row]')).toHaveLength(1);
    expect(query(element, '[data-routing-row="4412:acme/api"]')).not.toBeNull();

    // Now resolve the slow earlier GET with all 3 entries — it must be ignored
    resolveFirst(
      new Response(JSON.stringify(SAMPLE_ENTRIES), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(queryAll(element, '[data-routing-row]')).toHaveLength(1);
    expect(query(element, '[data-routing-row="4412:acme/api"]')).not.toBeNull();
  });
});
