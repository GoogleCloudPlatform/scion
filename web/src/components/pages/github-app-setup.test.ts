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
 * Tests for github-app-setup.ts: the "Get Started" card links to
 * /projects/new, so it is hidden without hub-scope project.create.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(opts: { hubCaps?: Capabilities; hubStatus?: number }) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/projects?limit=1')) {
      if (opts.hubStatus && opts.hubStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: { code: 'x' } }, opts.hubStatus));
      }
      return Promise.resolve(
        jsonResponse({ projects: [], ...(opts.hubCaps ? { _capabilities: opts.hubCaps } : {}) })
      );
    }
    if (path.includes('/api/v1/github-app/installations/discover')) {
      return Promise.resolve(jsonResponse({ installations: [], total: 0 }));
    }
    if (path.includes('/api/v1/projects?mine=true')) {
      return Promise.resolve(jsonResponse({ projects: [] }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

async function createComponent(opts: {
  hubCaps?: Capabilities;
  hubStatus?: number;
}): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts)));
  const el = document.createElement('scion-page-github-app-setup') as HTMLElement & {
    updateComplete: Promise<boolean>;
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return (el.shadowRoot?.textContent ?? '').replace(/\s+/g, ' ');
}

describe('scion-page-github-app-setup — Create New Project gate', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./github-app-setup.js');
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

  it('shows the Get Started card with hub create', async () => {
    element = await createComponent({ hubCaps: { actions: ['list', 'create'] } });

    expect(element.shadowRoot?.querySelector('.actions-card')).not.toBeNull();
    expect(shadowText(element)).toContain('Create New Project');
  });

  it('hides the Get Started card without hub create (viewer)', async () => {
    element = await createComponent({ hubCaps: { actions: ['list'] } });

    expect(element.shadowRoot?.querySelector('.actions-card')).toBeNull();
    expect(shadowText(element)).not.toContain('Create New Project');
    // The rest of the page still renders.
    expect(element.shadowRoot?.querySelector('.projects-card')).not.toBeNull();
  });

  it('fails closed when hub capabilities cannot be loaded', async () => {
    element = await createComponent({ hubStatus: 500 });

    expect(element.shadowRoot?.querySelector('.actions-card')).toBeNull();
  });
});
