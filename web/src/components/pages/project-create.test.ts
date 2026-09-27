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
 * Tests for project-create.ts: the form is gated on hub-scope project.create
 * (design §5.F). Without it, a notice explains why instead of a form.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities, PageData, UserRole } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(opts: { caps?: Capabilities; projectsStatus?: number }) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/projects?limit=1')) {
      if (opts.projectsStatus && opts.projectsStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: { code: 'x' } }, opts.projectsStatus));
      }
      return Promise.resolve(
        jsonResponse({ projects: [], ...(opts.caps ? { _capabilities: opts.caps } : {}) })
      );
    }
    if (path.includes('/api/v1/system/status')) {
      return Promise.resolve(jsonResponse({}));
    }
    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(jsonResponse({ configured: false }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

async function createComponent(
  opts: { caps?: Capabilities; projectsStatus?: number },
  role?: UserRole
): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts)));
  const el = document.createElement('scion-page-project-create') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
  };
  el.pageData = {
    path: '/projects/new',
    title: 'Create Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', ...(role ? { role } : {}) },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function q(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

describe('scion-page-project-create — hub project.create gate', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-create.js');
  }, 60_000);

  beforeEach(() => {
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders the form when hub caps include create', async () => {
    element = await createComponent({ caps: { actions: ['list', 'create'] } }, 'member');

    expect(q(element, '.form-card')).not.toBeNull();
    expect(q(element, '#name')).not.toBeNull();
    expect(q(element, '.create-denied-notice')).toBeNull();
  });

  it('renders the notice, not the form, when hub caps lack create', async () => {
    element = await createComponent({ caps: { actions: ['list'] } }, 'viewer');

    const notice = q(element, '.create-denied-notice');
    expect(notice).not.toBeNull();
    expect(q(element, '.form-card')).toBeNull();
    expect(q(element, '#name')).toBeNull();

    const text = notice?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain("Your hub role (Viewer) can't create projects.");
    expect(text).toContain(
      'Ask a hub admin to change your role, or to add you to an existing project.'
    );
  });

  it('links the notice to /projects', async () => {
    element = await createComponent({ caps: { actions: ['list'] } }, 'viewer');

    const link = q(element, '.create-denied-notice a[href="/projects"]');
    expect(link).not.toBeNull();
  });

  it('gates on capabilities, not the role string', async () => {
    // A viewer granted project.create by a custom binding sees the form.
    element = await createComponent({ caps: { actions: ['create'] } }, 'viewer');
    expect(q(element, '.form-card')).not.toBeNull();
    element.remove();
    resetHubProjectCapabilitiesCache();

    // A member without project.create sees the notice.
    element = await createComponent({ caps: { actions: ['list'] } }, 'member');
    expect(q(element, '.create-denied-notice')?.textContent).toContain(
      "Your hub role (Member) can't create projects."
    );
    expect(q(element, '.form-card')).toBeNull();
  });

  it('omits the role name when the role is unknown', async () => {
    element = await createComponent({ caps: { actions: [] } });

    const text = q(element, '.create-denied-notice')?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain("Your hub role can't create projects.");
    expect(text).not.toContain('(');
  });

  it('fails closed: shows the notice when capabilities cannot be loaded', async () => {
    element = await createComponent({ projectsStatus: 500 }, 'member');

    expect(q(element, '.create-denied-notice')).not.toBeNull();
    expect(q(element, '.form-card')).toBeNull();
  });

  it('does not redirect away from /projects/new', async () => {
    const pushState = vi.spyOn(window.history, 'pushState');
    element = await createComponent({ caps: { actions: [] } }, 'viewer');

    expect(pushState).not.toHaveBeenCalled();
  });
});
