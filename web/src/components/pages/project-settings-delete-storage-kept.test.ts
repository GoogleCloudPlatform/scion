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
 * Project settings > Danger Zone > Delete this project:
 * when DELETE /api/v1/projects/:id answers 200 OK with storageKept (for example
 * when a Runtime Broker deferred project directory cleanup with 409,
 * ptone/scion#4474), the UI surfaces a warning toast before navigating back to
 * /projects; a plain 204 No Content navigates without a warning toast.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { ScionPageProjectSettings } from './project-settings.js';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import { showToast } from '../../utils/toast.js';

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

function createFetchHandler(onDelete: () => Promise<Response>) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = requestPath(url);
    const method =
      init?.method?.toUpperCase() ?? (url instanceof Request ? url.method.toUpperCase() : 'GET');
    if (path.match(/\/api\/v1\/projects\/proj-1(\?|$)/)) {
      if (method === 'DELETE') {
        return onDelete();
      }
      return json({
        id: 'proj-1',
        name: 'Test Project',
        slug: 'test-project',
        _capabilities: { actions: ['read', 'update', 'delete', 'manage'] },
      });
    }
    if (path.includes('/settings/resolved')) {
      return json({ projectId: 'proj-1', settings: {}, resolvedSettings: {} });
    }
    if (path.includes('/templates')) {
      return json({ templates: [], page: 1, pageSize: 100, total: 0 });
    }
    return json({});
  };
}

async function createComponent(onDelete: () => Promise<Response>): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(onDelete)));
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

describe('project-settings: Delete project storageKept notice (ptone/scion#4474)', () => {
  let element: HTMLElement | null = null;
  let navPaths: string[] = [];
  const onNavClick = (event: Event): void => {
    const detail = (event as CustomEvent<{ path: string }>).detail;
    if (detail?.path) navPaths.push(detail.path);
  };

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(createFetchHandler(() => Promise.resolve(new Response(null, { status: 204 }))))
    );
    await import('./project-settings.js');
  }, 30_000);

  beforeEach(() => {
    navPaths = [];
    document.addEventListener('nav-click', onNavClick);
    vi.mocked(showToast).mockClear();
  });

  afterEach(() => {
    document.removeEventListener('nav-click', onNavClick);
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('surfaces a warning toast and navigates to /projects when DELETE answers 200 with storageKept', async () => {
    const keptMessage =
      'One or more Runtime Brokers kept their local project directory because agents of the deleted project or another Runtime Broker instance on the same host still use it; cleanup on the Runtime Broker is deferred.';
    element = await createComponent(() =>
      json({
        storageKept: [
          {
            code: 'runtime-broker-project-directory',
            message: keptMessage,
          },
        ],
      })
    );

    const deleteBtn = element.shadowRoot?.querySelector<HTMLElement>('[data-delete-project]');
    expect(deleteBtn).not.toBeNull();
    deleteBtn!.dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true, altKey: true })
    );

    await vi.waitFor(() => expect(navPaths).toEqual(['/projects']));
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith(
      `Project "Test Project" deleted. ${keptMessage}`,
      'warning'
    );
  });

  it('navigates to /projects without a toast when DELETE answers 204 No Content', async () => {
    element = await createComponent(() => Promise.resolve(new Response(null, { status: 204 })));

    const deleteBtn = element.shadowRoot?.querySelector<HTMLElement>('[data-delete-project]');
    expect(deleteBtn).not.toBeNull();
    deleteBtn!.dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true, altKey: true })
    );

    await vi.waitFor(() => expect(navPaths).toEqual(['/projects']));
    expect(showToast).not.toHaveBeenCalled();
  });
});
