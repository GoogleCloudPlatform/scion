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
 * Project settings > Members: the Visitors tab (ptone/scion#4603) is shown
 * only while the federation.project_visitors experiment is on and the
 * caller holds project.manage_visitors (the manage_visitors capability).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageProjectSettings } from './project-settings.js';

const FLAG = 'federation.project_visitors';

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
      return json({ projectId: 'proj-1', settings: {}, resolvedSettings: {} });
    }
    if (path.includes('/templates')) {
      return json({ templates: [], page: 1, pageSize: 100, total: 0 });
    }
    if (path.endsWith('/visitors')) {
      return json({ visitors: [] });
    }
    if (path.endsWith('/visitors/rules')) {
      return json({ rules: [] });
    }
    return json({});
  };
}

function setFlag(value: boolean | undefined): void {
  const bag = { ...(window.__SCION_FEATURES__ ?? {}) };
  if (value === undefined) delete bag[FLAG];
  else bag[FLAG] = value;
  window.__SCION_FEATURES__ = bag;
}

async function createComponent(actions: string[]): Promise<HTMLElement> {
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

function visitorsTab(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector('sl-tab[panel="visitors"]') ?? null;
}

function visitorsEditor(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector('scion-project-visitors-editor') ?? null;
}

function membersEditor(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector('scion-project-members-editor') ?? null;
}

describe('project-settings: Visitors tab gating (ptone/scion#4603)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    setFlag(undefined);
    vi.restoreAllMocks();
  });

  it('shows the Visitors tab with the experiment on and manage_visitors held', async () => {
    setFlag(true);
    element = await createComponent(['read', 'update', 'manage', 'manage_visitors']);

    expect(visitorsTab(element)).not.toBeNull();
    expect(visitorsEditor(element)?.getAttribute('projectid')).toBe('proj-1');
    expect(membersEditor(element)).not.toBeNull();
  });

  it('hides the Visitors tab without manage_visitors, even with the experiment on', async () => {
    setFlag(true);
    element = await createComponent(['read', 'update', 'manage']);

    expect(visitorsTab(element)).toBeNull();
    expect(visitorsEditor(element)).toBeNull();
    expect(membersEditor(element)).not.toBeNull();
  });

  it('hides the Visitors tab while the experiment is off, even with manage_visitors', async () => {
    setFlag(false);
    element = await createComponent(['read', 'update', 'manage', 'manage_visitors']);

    expect(visitorsTab(element)).toBeNull();
    expect(visitorsEditor(element)).toBeNull();
    expect(membersEditor(element)).not.toBeNull();
  });

  it('sends no visitor request when the tab is hidden', async () => {
    setFlag(false);
    element = await createComponent(['read', 'update', 'manage', 'manage_visitors']);

    const urls = vi.mocked(fetch).mock.calls.map(([u]) => requestPath(u));
    expect(urls.some((u) => u.includes('/visitors'))).toBe(false);
  });
});
