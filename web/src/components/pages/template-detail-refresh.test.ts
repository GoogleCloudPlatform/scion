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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageTemplateDetail } from './template-detail.js';

const GH_SOURCE = 'https://github.com/acme/repo/tree/main/.scion/templates/my-template';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeTemplate(overrides: Record<string, unknown> = {}) {
  return {
    id: 'tmpl-1',
    name: 'my-template',
    slug: 'my-template',
    harness: 'claude',
    status: 'active',
    scope: 'global',
    ...overrides,
  };
}

async function flush(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function createElement(): Promise<ScionPageTemplateDetail> {
  window.history.pushState({}, '', '/settings/templates/tmpl-1');
  const el = document.createElement('scion-page-template-detail') as ScionPageTemplateDetail;
  document.body.appendChild(el);
  await flush(el);
  return el;
}

function refreshButton(el: HTMLElement): HTMLElement | null {
  return el.shadowRoot?.querySelector('sl-button.refresh-from-source') ?? null;
}

describe('template detail: Refresh from Source', () => {
  let element: ScionPageTemplateDetail | null = null;

  beforeAll(async () => {
    await import('./template-detail.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows the button and the source link for a GitHub source', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeTemplate({ sourceUrl: GH_SOURCE }))))
    );
    element = await createElement();
    expect(refreshButton(element)).not.toBeNull();
    const link = element.shadowRoot?.querySelector('.source-url a') as HTMLAnchorElement | null;
    expect(link?.getAttribute('href')).toBe(GH_SOURCE);
  });

  it('hides the button for a built-in template', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeTemplate({ sourceUrl: 'builtin://scion/1.0/template/default' }))
        )
      )
    );
    element = await createElement();
    expect(refreshButton(element)).toBeNull();
    // Shown as text, never as a link.
    expect(element.shadowRoot?.querySelector('.source-url a')).toBeNull();
  });

  it('hides the button when there is no source', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeTemplate())))
    );
    element = await createElement();
    expect(refreshButton(element)).toBeNull();
  });

  it('does not show credentials from a stored source', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeTemplate({ sourceUrl: 'https://user:secret@github.com/acme/repo' }))
        )
      )
    );
    element = await createElement();
    expect(element.shadowRoot?.textContent ?? '').not.toContain('secret');
    expect(refreshButton(element)).toBeNull();
  });

  // The display never shows credentials embedded in a source string.
  it('never shows credentials embedded in a non-https source', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(
            makeTemplate({
              sourceUrl: ':s3,access_key_id=AKIA,secret_access_key=secret:bucket/templates',
            })
          )
        )
      )
    );
    element = await createElement();
    const text = element.shadowRoot?.textContent ?? '';
    expect(text).not.toContain('secret');
    expect(text).not.toContain('AKIA');
    expect(text).toContain('non-web source');
    expect(element.shadowRoot?.querySelector('.source-url a')).toBeNull();
    expect(refreshButton(element)).toBeNull();
  });

  it('posts to the reimport endpoint and reports success', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url.endsWith('/reimport') && init?.method === 'POST') {
        return Promise.resolve(jsonResponse({ templates: ['my-template'], count: 1 }));
      }
      return Promise.resolve(jsonResponse(makeTemplate({ sourceUrl: GH_SOURCE })));
    });
    vi.stubGlobal('fetch', fetchMock);
    element = await createElement();

    refreshButton(element)!.click();
    await flush(element);

    const posted = fetchMock.mock.calls.filter(
      ([url, init]) => url.endsWith('/api/v1/templates/tmpl-1/reimport') && init?.method === 'POST'
    );
    expect(posted).toHaveLength(1);
    expect(element.shadowRoot?.querySelector('.reimport-status.success')?.textContent).toContain(
      'Refreshed'
    );
  });

  it('shows the hub error when the refresh is refused', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url.endsWith('/reimport') && init?.method === 'POST') {
        return Promise.resolve(
          jsonResponse(
            { error: { code: 'unsupported_source', message: 'not a GitHub folder URL' } },
            400
          )
        );
      }
      return Promise.resolve(jsonResponse(makeTemplate({ sourceUrl: GH_SOURCE })));
    });
    vi.stubGlobal('fetch', fetchMock);
    element = await createElement();

    refreshButton(element)!.click();
    await flush(element);

    expect(element.shadowRoot?.querySelector('.reimport-status.error')?.textContent).toContain(
      'not a GitHub folder URL'
    );
  });
});
