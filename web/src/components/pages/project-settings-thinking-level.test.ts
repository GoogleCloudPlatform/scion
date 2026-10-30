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
 * The project Default Thinking Level slider follows the API range, 1-100
 * (ptone/scion#4052): its minimum is 1, and a loaded value outside the
 * range is clamped, so a save never sends 0.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

let storedLevel: number | null = null;

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
    const project = storedLevel === null ? {} : { defaultThinkingLevel: storedLevel };
    return json({ project, settings: {} });
  }
  if (path.includes('/templates')) {
    return json({ templates: [], page: 1, pageSize: 100, total: 0 });
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
};

beforeAll(async () => {
  vi.stubGlobal('fetch', vi.fn(handler));
  await import('./project-settings.js');
}, 30_000);

beforeEach(() => {
  storedLevel = null;
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

function level(el: SettingsEl): number | null {
  return (el as unknown as { defaultThinkingLevel: number | null }).defaultThinkingLevel;
}

describe('project settings Default Thinking Level range', () => {
  it('has a slider minimum of 1 and maximum of 100', async () => {
    storedLevel = 40;
    const el = await mount();
    const range = el.shadowRoot?.querySelector('sl-range');
    expect(range).toBeTruthy();
    expect(range?.getAttribute('min')).toBe('1');
    expect(range?.getAttribute('max')).toBe('100');
    expect(level(el)).toBe(40);
  });

  it('clamps a loaded level below 1 to 1, so a save never sends 0', async () => {
    storedLevel = 0;
    const el = await mount();
    expect(level(el)).toBe(1);
  });

  it('keeps an unset level unset', async () => {
    const el = await mount();
    expect(level(el)).toBeNull();
  });
});
