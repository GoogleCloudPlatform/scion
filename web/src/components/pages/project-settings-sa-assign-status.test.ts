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
 * ptone/scion#4391: the project default service-account picker applies to
 * every runtime profile, so there is no single Kubernetes target to report
 * a mapping for. It does not ask for the mapping state, shows no mapping
 * labels, and says why, even on a project whose brokers are all Kubernetes.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionPageProjectSettings } from './project-settings.js';

const PROJECT_RESPONSE = {
  id: 'proj-1',
  name: 'Test Project',
  slug: 'test-project',
  _capabilities: { actions: ['update', 'manage'] },
};

const SETTINGS = {
  defaultGCPIdentityMode: 'assign',
  defaultGCPIdentityServiceAccountID: 'sa-a',
};

function account(id: string) {
  return {
    id,
    scope: 'project',
    scopeId: 'proj-1',
    email: `${id}@example.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    createdBy: 'user-1',
    createdAt: '2026-01-01T00:00:00Z',
  };
}

function stubFetch(): URL[] {
  const listUrls: URL[] = [];
  const json = (body: unknown) =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(requestUrl(input), 'http://hub.example.com');
      const path = url.pathname;
      if (path.includes('/runtime-brokers')) {
        return json({
          brokers: [
            {
              id: 'broker-1',
              name: 'broker-1',
              slug: 'broker-1',
              status: 'online',
              connectionState: 'connected',
              autoProvide: false,
              profiles: [{ name: 'k8s-a', type: 'kubernetes', available: true }],
            },
          ],
        });
      }
      if (path.endsWith('/gcp-service-accounts')) {
        listUrls.push(url);
        return json({ items: [account('sa-a'), account('sa-b')] });
      }
      if (/\/api\/v1\/projects\/[^/]+$/.test(path)) return json(PROJECT_RESPONSE);
      if (path.endsWith('/settings/resolved')) {
        return json({ projectId: 'proj-1', project: SETTINGS, settings: {} });
      }
      if (path.endsWith('/settings')) return json(SETTINGS);
      return json({});
    })
  );
  return listUrls;
}

/** The project default's Service Account field (not the mode field). */
function serviceAccountField(el: HTMLElement): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  return (
    fields.find((f) => f.querySelector('label')?.textContent?.trim() === 'Service Account') ?? null
  );
}

describe('project-settings: default service-account picker and mapping state', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    stubFetch();
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('does not ask for or show a mapping state, and says why', async () => {
    const listUrls = stubFetch();
    const el = document.createElement('scion-page-project-settings') as InstanceType<
      typeof ScionPageProjectSettings
    >;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    element = el;
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 400));
    await el.updateComplete;

    expect(listUrls.length).toBeGreaterThan(0);
    for (const url of listUrls) {
      expect(url.searchParams.get('includeHubScoped')).toBe('true');
      expect(url.searchParams.has('assignStatus')).toBe(false);
      expect(url.searchParams.has('profile')).toBe(false);
    }

    const field = serviceAccountField(el);
    expect(field).not.toBeNull();
    const opts = Array.from(field!.querySelectorAll('sl-option'));
    expect(opts.map((o) => o.getAttribute('value'))).toEqual(['sa-a', 'sa-b']);
    for (const o of opts) {
      expect(o.hasAttribute('data-assign-state')).toBe(false);
      expect(o.querySelector('.assign-status')).toBeNull();
    }
    const help = (field!.querySelector('.field-help')?.textContent ?? '').replace(/\s+/g, ' ');
    expect(help).toContain('applies to every runtime profile, so no mapping state is shown here');
  });
});
