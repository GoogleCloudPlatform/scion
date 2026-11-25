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
 * Project settings > Resources > Pre-Start Hooks: the hook list is rendered
 * read-only unless the caller holds project.manage_pre_start_hooks
 * (the manage_pre_start_hooks capability), even when they hold update/manage.
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
      return json({ projectId: 'proj-1', settings: {}, resolvedSettings: {} });
    }
    if (path.includes('/pre-start-hooks')) {
      return json({ hooks: [] });
    }
    if (path.includes('/templates')) {
      return json({ templates: [], page: 1, pageSize: 100, total: 0 });
    }
    return json({});
  };
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

function hookList(el: HTMLElement): (HTMLElement & { readonly?: boolean }) | null {
  return el.shadowRoot?.querySelector('scion-pre-start-hook-list') ?? null;
}

describe('project-settings: Pre-Start Hooks readonly gating', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('renders pre-start-hook-list readonly when caller has update/manage but not manage_pre_start_hooks', async () => {
    element = await createComponent(['read', 'update', 'manage']);

    const list = hookList(element);
    expect(list).not.toBeNull();
    expect(list?.hasAttribute('readonly')).toBe(true);
    expect(list?.readonly).toBe(true);
  });

  it('renders pre-start-hook-list editable when caller holds manage_pre_start_hooks', async () => {
    element = await createComponent(['read', 'update', 'manage', 'manage_pre_start_hooks']);

    const list = hookList(element);
    expect(list).not.toBeNull();
    expect(list?.hasAttribute('readonly')).toBe(false);
    expect(list?.readonly).toBe(false);
  });
});
