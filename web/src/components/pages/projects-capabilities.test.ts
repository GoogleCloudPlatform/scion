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
 * ptone/scion#5311: the Projects page seeds the hub-scope project
 * capabilities from its own list response, so the next page's
 * create-project check (fetchHubProjectCapabilities) sends no
 * `projects?limit=1` request.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import { requestUrl } from '../../client/__fixtures__/request-url.js';
import {
  fetchHubProjectCapabilities,
  resetHubProjectCapabilitiesCache,
} from '../../client/hub-capabilities.js';
import { stateManager } from '../../client/state.js';

class FakeEventSource extends EventTarget {
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  close(): void {
    this.readyState = 2;
  }
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

let requests: string[] = [];

function stubFetch(listBody: unknown): void {
  requests = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = requestUrl(input);
      requests.push(url);
      if (url.startsWith('/api/v1/projects')) return json(listBody);
      return json({});
    })
  );
}

type ProjectsPage = HTMLElement & { loading: boolean };

async function mountProjectsPage(): Promise<ProjectsPage> {
  const el = document.createElement('scion-page-projects') as ProjectsPage;
  document.body.appendChild(el);
  await vi.waitFor(() => expect(el.loading).toBe(false), { timeout: 3000 });
  return el;
}

beforeAll(async () => {
  stubFetch({ projects: [] });
  await import('./projects.js');
});

afterEach(() => {
  document.body.innerHTML = '';
  // Leave the dashboard scope, which clears the store's projects and scope
  // capabilities, so the next mount fetches its list instead of hydrating.
  stateManager.setScope({ type: 'brokers-list' });
  resetHubProjectCapabilitiesCache();
  vi.unstubAllGlobals();
});

describe('Projects page: hub capabilities (ptone/scion#5311)', () => {
  it('projects page seeds capabilities (no projects?limit=1 on next nav)', async () => {
    stubFetch({ projects: [{ id: 'p1', name: 'P1' }], _capabilities: { actions: ['create'] } });
    await mountProjectsPage();
    expect(requests.some((u) => u.startsWith('/api/v1/projects'))).toBe(true);
    requests = [];

    // What the next page (project detail, template list) asks.
    const caps = await fetchHubProjectCapabilities();

    expect(caps?.actions).toEqual(['create']);
    expect(requests.filter((u) => u.includes('projects?limit=1'))).toEqual([]);
  });

  it('a list response without capabilities seeds nothing, so the helper still asks', async () => {
    stubFetch({ projects: [{ id: 'p1', name: 'P1' }] });
    await mountProjectsPage();
    expect(requests.some((u) => u.startsWith('/api/v1/projects'))).toBe(true);
    requests = [];

    await fetchHubProjectCapabilities();

    expect(requests.filter((u) => u.includes('projects?limit=1'))).toHaveLength(1);
  });
});
