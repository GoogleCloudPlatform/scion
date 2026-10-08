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
 * The Default Template dropdown in project settings (ptone/scion#3465):
 * when the template list fails to load, the field says so and the saved
 * default is kept.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

let templatesStatus = 500;

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function handler(url: string | URL | Request): Promise<Response> {
  const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
  if (path.includes('/settings/resolved')) {
    return json({ project: { defaultTemplate: 'saved-tmpl' }, settings: {} });
  }
  if (path.includes('/templates')) {
    return json(
      { templates: [{ id: 't1', name: 'saved-tmpl' }], page: 1, pageSize: 100, total: 1 },
      templatesStatus
    );
  }
  if (path.match(/\/api\/v1\/projects\/[^/?]+($|\?)/)) {
    return json({
      id: 'proj-1',
      name: 'Test Project',
      slug: 'test-project',
      _capabilities: { actions: ['update', 'manage'] },
    });
  }
  return json({});
}

type SettingsEl = HTMLElement & {
  projectId: string;
  updateComplete: Promise<unknown>;
  configDefaultTemplate: string;
};

beforeAll(async () => {
  vi.stubGlobal('fetch', vi.fn(handler));
  await import('./project-settings.js');
}, 30_000);

beforeEach(() => {
  templatesStatus = 500;
  vi.stubGlobal('fetch', vi.fn(handler));
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function mount(): Promise<SettingsEl> {
  const el = document.createElement('scion-page-project-settings') as SettingsEl;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

function text(el: HTMLElement): string {
  return el.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';
}

describe('project settings Default Template load failure', () => {
  it('shows the load error and keeps the saved default', async () => {
    const el = await mount();

    expect(text(el)).toContain('Could not load templates.');
    expect(text(el)).toContain('The saved default (saved-tmpl) is kept.');
    expect(text(el)).not.toContain('Template used when creating agents without specifying one.');
    expect(el.configDefaultTemplate).toBe('saved-tmpl');
  });

  it('shows the usual help text when the list loads', async () => {
    templatesStatus = 200;
    const el = await mount();

    expect(text(el)).not.toContain('Could not load templates.');
    expect(text(el)).toContain('Template used when creating agents without specifying one.');
  });
});
