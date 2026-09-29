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
 * Tests for the Created / Updated stats in the project-detail header.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { PageData } from '../../shared/types.js';
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

const PROJECT_ID = 'p-dates';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function renderProject(project: Record<string, unknown>): Promise<HTMLElement> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
        return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
      }
      if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
        return Promise.resolve(
          jsonResponse({
            id: PROJECT_ID,
            name: 'Dated Project',
            slug: 'dated-project',
            _capabilities: { actions: ['read'] },
            ...project,
          })
        );
      }
      return Promise.resolve(jsonResponse({}, 404));
    })
  );
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
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

/** Text of the stat value under the given label. */
function statValue(el: HTMLElement, label: string): string {
  const stats = Array.from(el.shadowRoot?.querySelectorAll('.stat') ?? []);
  const stat = stats.find((s) => s.querySelector('.stat-label')?.textContent?.trim() === label);
  return stat?.querySelector('.stat-value')?.textContent?.trim() ?? '';
}

describe('scion-page-project-detail — Created / Updated stats', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows the dates the hub sends as created / updated', async () => {
    element = await renderProject({
      created: '2026-08-14T12:00:00Z',
      updated: '2026-09-28T12:00:00Z',
    });
    expect(statValue(element, 'Created')).toMatch(/Aug 14, 2026/);
    expect(statValue(element, 'Updated')).toMatch(/Sep 28, 2026/);
  });

  it('still reads the older createdAt / updatedAt names', async () => {
    element = await renderProject({
      createdAt: '2026-07-01T12:00:00Z',
      updatedAt: '2026-07-02T12:00:00Z',
    });
    expect(statValue(element, 'Created')).toMatch(/Jul 1, 2026/);
    expect(statValue(element, 'Updated')).toMatch(/Jul 2, 2026/);
  });

  it('shows a dash instead of a blank when a date is missing or invalid', async () => {
    element = await renderProject({ created: 'not-a-date' });
    expect(statValue(element, 'Created')).toBe('—');
    expect(statValue(element, 'Updated')).toBe('—');
  });
});
