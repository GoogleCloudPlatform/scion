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
 * Agent badges on the project page (web.agent_badges, ptone/scion#5503):
 * the agent card and table row show `scion-agent-badges` in place of the
 * cpu icon while the experiment is on, and nothing changes while it is off.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { Agent, PageData } from '../../shared/types.js';
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

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: PROJECT_ID,
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read'] },
    ...overrides,
  } as Agent;
}

const AGENTS = [
  agent('a1', {
    harnessVendor: 'anthropic',
    modelSize: 'small',
    appliedConfig: { harness: 'claude' } as Agent['appliedConfig'],
  }),
];

function fetchHandler(url: string | URL | Request): Promise<Response> {
  const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
  if (path.includes('/api/v1/projects?limit=1')) {
    return Promise.resolve(jsonResponse({ projects: [] }));
  }
  if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
    // The sorted list response (the page asks sort=updated&stats=1).
    return Promise.resolve(
      jsonResponse({
        agents: AGENTS,
        totalCount: AGENTS.length,
        complete: true,
        stats: {
          total: AGENTS.length,
          running: AGENTS.length,
          agents: AGENTS.map((a) => [a.id, a.phase]),
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
        _capabilities: { actions: ['read'] },
      })
    );
  }
  return Promise.resolve(jsonResponse({}, 404));
}

async function createComponent(): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-project-detail') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
    projectId: string;
  };
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  for (let i = 0; i < 4; i++) {
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  await el.updateComplete;
  return el;
}

/** The container (card heading or name cell) of the agent link for `id`. */
function containerFor(el: HTMLElement, id: string): Element | null {
  const link = Array.from(el.shadowRoot!.querySelectorAll('a')).find(
    (a) => a.getAttribute('href') === `/agents/${id}`
  );
  return link?.closest('h3, .name-cell') ?? null;
}

describe('scion-page-project-detail agent badges', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    const store = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, String(v)),
      removeItem: (k: string) => void store.delete(k),
      clear: () => store.clear(),
    });
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    delete window.__SCION_FEATURES__;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  for (const view of ['grid', 'list'] as const) {
    it(`${view}: shows the badges in place of the cpu icon when on`, async () => {
      window.__SCION_FEATURES__ = { 'web.agent_badges': true };
      localStorage.setItem('scion-view-project-agents', view);
      element = await createComponent();

      const container = containerFor(element, 'a1');
      expect(container).not.toBeNull();
      const badges = container!.querySelector('scion-agent-badges');
      expect(badges?.getAttribute('vendor')).toBe('anthropic');
      expect(badges?.getAttribute('size')).toBe('small');
      expect(badges?.getAttribute('harness')).toBe('claude');
      expect(container!.querySelector('sl-icon[name="cpu"]')).toBeNull();
    });

    it(`${view}: keeps the cpu icon and creates no badges when off`, async () => {
      window.__SCION_FEATURES__ = { 'web.agent_badges': false };
      localStorage.setItem('scion-view-project-agents', view);
      element = await createComponent();

      const container = containerFor(element, 'a1');
      expect(container).not.toBeNull();
      expect(element.shadowRoot!.querySelector('scion-agent-badges')).toBeNull();
      expect(container!.querySelector('sl-icon[name="cpu"]')).not.toBeNull();
    });
  }
});
