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
 * Project settings resource labels (ptone/scion#8467):
 * - Configuration section tab `panel="resources"` is labeled "Agent Resources".
 * - Project-scoped resources card heading is "Project Resources".
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageProjectSettings } from './project-settings.js';

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function requestPath(url: string | URL | Request): string {
  return typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
}

function createFetchHandler(actions: string[]) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = requestPath(url);
    if (path.match(/\/api\/v1\/projects\/[^/?]+(\?|$)/)) {
      return json({
        id: 'proj-1',
        name: 'Test Project',
        slug: 'test-project',
        _capabilities: { actions },
      });
    }
    if (path.includes('/settings/resolved')) {
      return json({ project: {}, settings: {} });
    }
    if (path.includes('/templates')) {
      return json({ templates: [], page: 1, pageSize: 100, total: 0 });
    }
    return json({});
  };
}

async function mount(actions = ['read', 'update', 'manage']): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(actions)));
  const el = document.createElement('scion-page-project-settings') as InstanceType<
    typeof ScionPageProjectSettings
  >;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

describe('project-settings: Agent Resources tab and Project Resources card labels (ptone/scion#8467)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('labels the Configuration resources tab "Agent Resources" while preserving panel="resources"', async () => {
    element = await mount();

    const sections = Array.from(element.shadowRoot?.querySelectorAll('.section') ?? []);
    const configSection = sections.find(
      (section) => section.querySelector('h2')?.textContent?.trim() === 'Configuration'
    );
    expect(configSection).toBeDefined();

    const configTabLabels = Array.from(configSection!.querySelectorAll('sl-tab[slot="nav"]')).map(
      (tab) => tab.textContent?.trim()
    );
    expect(configTabLabels).toEqual([
      'General',
      'Auth & Security',
      'Limits',
      'Agent Resources',
      'Runtime Brokers',
    ]);

    const agentResourcesTab = configSection!.querySelector('sl-tab[panel="resources"]');
    expect(agentResourcesTab?.textContent?.trim()).toBe('Agent Resources');
    expect(configSection!.querySelector('sl-tab-panel[name="resources"]')).not.toBeNull();
  });

  it('titles the project-scoped resources card "Project Resources"', async () => {
    element = await mount();

    const headings = Array.from(element.shadowRoot?.querySelectorAll('.section > h2') ?? []).map(
      (h2) => h2.textContent?.trim()
    );
    expect(headings).toContain('Project Resources');
    expect(headings).not.toContain('Resources');

    const sections = Array.from(element.shadowRoot?.querySelectorAll('.section') ?? []);
    const projectResourcesSection = sections.find(
      (section) => section.querySelector('h2')?.textContent?.trim() === 'Project Resources'
    );
    expect(projectResourcesSection).toBeDefined();
    expect(projectResourcesSection!.querySelector('sl-tab[panel="env-vars"]')).not.toBeNull();
  });
});
