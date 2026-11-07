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
 * The hub value on the project settings page is a fallback, not the value
 * an agent is sure to get (ptone/scion#3925): a template or harness config
 * can set its own. The resolved endpoint reports no effective value, so the
 * page labels the hub value as a fallback, and says so when the hub does not
 * report one ("unknown").
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { HUB_FALLBACK_UNKNOWN_TOOLTIP, hubFallbackTooltip } from './project-settings.js';

const MAX_TURNS = 'scion.io/default-max-turns';

let resolved: Record<string, unknown> = {};

function stubFetch(): void {
  const json = (body: unknown): Promise<Response> =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const path = new URL(requestUrl(input), 'http://hub.example.com').pathname;
      if (path.endsWith('/settings/resolved')) {
        return json({ projectId: 'proj-1', project: {}, settings: resolved });
      }
      if (path.endsWith('/settings')) return json({});
      if (path.includes('/templates')) {
        return json({ templates: [], page: 1, pageSize: 100, total: 0 });
      }
      if (/\/api\/v1\/projects\/[^/]+$/.test(path)) {
        return json({
          id: 'proj-1',
          name: 'Test Project',
          slug: 'test-project',
          _capabilities: { actions: ['update', 'manage'] },
        });
      }
      return json({});
    })
  );
}

type PageElement = HTMLElement & { projectId: string; updateComplete: Promise<unknown> };

beforeAll(async () => {
  stubFetch();
  await import('./project-settings.js');
}, 30_000);

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function mount(settings: Record<string, unknown>): Promise<PageElement> {
  resolved = settings;
  stubFetch();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  const el = document.createElement('scion-page-project-settings') as PageElement;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

/** The count-limit field whose label starts with the text. */
function field(el: PageElement, label: string): Element {
  const f = Array.from(el.shadowRoot!.querySelectorAll('.config-field')).find((c) =>
    (c.querySelector('label')?.textContent ?? '').trim().startsWith(label)
  );
  if (!f) throw new Error(`no field ${label}`);
  return f;
}

describe('hub fallback wording', () => {
  it('the tooltip names the hub value a fallback that templates can outrank', () => {
    expect(hubFallbackTooltip('200')).toBe(
      'Hub fallback: 200. A template or harness config can set its own value, which wins over it.'
    );
    expect(hubFallbackTooltip('200')).not.toMatch(/default|in effect/i);
  });

  it('a present hub value shows as a fallback placeholder and tooltip', async () => {
    const el = await mount({
      [MAX_TURNS]: { projectSet: false, projectValue: null, hubDefault: 'present', hubValue: 200 },
    });
    const f = field(el, 'Default Max Turns');
    expect(f.querySelector('sl-input')?.getAttribute('placeholder')).toBe('Hub fallback: 200');
    expect(f.querySelector('sl-tooltip')?.getAttribute('content')).toBe(hubFallbackTooltip('200'));
    expect(el.shadowRoot!.innerHTML).not.toContain('Hub default: 200');
  });

  it('an unknown hub value shows the not-reported tooltip and no value', async () => {
    const el = await mount({
      [MAX_TURNS]: { projectSet: false, projectValue: null, hubDefault: 'unknown', hubValue: null },
    });
    const f = field(el, 'Default Max Turns');
    expect(f.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      HUB_FALLBACK_UNKNOWN_TOOLTIP
    );
    expect(f.querySelector('sl-input')?.getAttribute('placeholder')).toBe('Inherit');
  });

  it('an absent hub value shows no indicator (control)', async () => {
    const el = await mount({
      [MAX_TURNS]: { projectSet: false, projectValue: null, hubDefault: 'absent', hubValue: null },
    });
    expect(field(el, 'Default Max Turns').querySelector('sl-tooltip')).toBeNull();
  });

  it('a project value hides the indicator, known or unknown (control)', async () => {
    const el = await mount({
      [MAX_TURNS]: { projectSet: true, projectValue: '5', hubDefault: 'unknown', hubValue: null },
    });
    expect(field(el, 'Default Max Turns').querySelector('sl-tooltip')).toBeNull();
  });
});
