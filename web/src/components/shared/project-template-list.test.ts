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
 * Tests for project-template-list.ts: "Create Template" and "Create From"
 * both clone a project, so they need hub-scope project.create. Rename and
 * delete are unaffected.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

const TEMPLATES = [
  { id: 't-1', name: 'Alpha Template', slug: 'alpha-template', created: '2026-01-01T00:00:00Z' },
];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(opts: {
  hubCaps?: Capabilities;
  hubStatus?: number;
  templates?: unknown[];
}) {
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
    if (path.includes('/api/v1/projects?isTemplate=true')) {
      return Promise.resolve(jsonResponse({ projects: opts.templates ?? TEMPLATES }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

async function createComponent(opts: {
  hubCaps?: Capabilities;
  hubStatus?: number;
  templates?: unknown[];
}): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts)));
  const el = document.createElement('scion-project-template-list') as HTMLElement & {
    updateComplete: Promise<boolean>;
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function text(el: HTMLElement, selector: string): string {
  return (el.shadowRoot?.querySelector(selector)?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function menuItems(el: HTMLElement): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.template-row sl-menu-item') ?? []).map(
    (i) => i.textContent?.trim() ?? ''
  );
}

describe('scion-project-template-list — hub project.create gate', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-template-list.js');
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

  it('shows Create Template and Create From with hub create', async () => {
    element = await createComponent({ hubCaps: { actions: ['list', 'create'] } });

    expect(text(element, '.create-btn')).toContain('Create Template');
    expect(menuItems(element)).toEqual(['Create From', 'Rename', 'Delete']);
  });

  it('hides Create Template and Create From without hub create, keeping rename and delete', async () => {
    element = await createComponent({ hubCaps: { actions: ['list'] } });

    expect(element.shadowRoot?.querySelector('.create-btn')).toBeNull();
    expect(menuItems(element)).toEqual(['Rename', 'Delete']);
    // The template itself is still listed.
    expect(text(element, '.template-name')).toBe('Alpha Template');
  });

  it('fails closed when hub capabilities cannot be loaded', async () => {
    element = await createComponent({ hubStatus: 500 });

    expect(element.shadowRoot?.querySelector('.create-btn')).toBeNull();
    expect(menuItems(element)).not.toContain('Create From');
  });

  it('empty state omits the "create one" hint without hub create', async () => {
    element = await createComponent({ hubCaps: { actions: [] }, templates: [] });

    const empty = text(element, '.empty');
    expect(empty).toContain('No project templates yet.');
    expect(empty).not.toContain('Create one from an existing project.');
  });

  it('empty state keeps the "create one" hint with hub create', async () => {
    element = await createComponent({ hubCaps: { actions: ['create'] }, templates: [] });

    expect(text(element, '.empty')).toContain(
      'No project templates yet. Create one from an existing project.'
    );
  });
});
