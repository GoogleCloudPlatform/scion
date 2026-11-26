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
 * Artifacts list page URL filter hydration and `history.replaceState` sync
 * for Inbox drill-in links (`/artifacts?review_pending=1&owner=me`, design §6.5, AC12).
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ScionPageArtifacts } from './artifacts.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

async function settle(el: ScionPageArtifacts): Promise<void> {
  for (let i = 0; i < 15; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('artifacts list page URL filter hydration', () => {
  let fetchedUrls: string[] = [];

  beforeAll(async () => {
    await import('./artifacts.js');
  }, 30_000);

  beforeEach(() => {
    fetchedUrls = [];
    window.history.replaceState(null, '', '/artifacts');
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        fetchedUrls.push(url);
        if (url.startsWith('/api/v1/artifacts?')) {
          return Promise.resolve(
            new Response(JSON.stringify({ artifacts: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(new Response('{}', { status: 403 }));
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    window.history.replaceState(null, '', '/');
  });

  async function mount(): Promise<ScionPageArtifacts> {
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = {
      path: '/artifacts',
      title: 'Artifacts',
      user: { id: 'user-me', email: 'me@example.com', name: 'Me' },
    } as ScionPageArtifacts['pageData'];
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  function listParams(): URLSearchParams[] {
    return fetchedUrls
      .filter((u) => u.startsWith('/api/v1/artifacts?'))
      .map((u) => new URL(u, 'http://x').searchParams);
  }

  it('migrates ?review_pending=1&owner=me to ?q=is:mine is:review on mount', async () => {
    window.history.replaceState(null, '', '/artifacts?review_pending=1&owner=me');
    const el = await mount();

    const params = listParams()[0];
    expect(params.get('q')).toBe('is:mine is:review');

    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };
    expect(box.value).toBe('is:review is:mine');
    expect(new URLSearchParams(window.location.search).get('q')).toBe('is:review is:mine');
    expect(new URLSearchParams(window.location.search).has('review_pending')).toBe(false);
    expect(new URLSearchParams(window.location.search).has('owner')).toBe(false);
  });

  it('keeps ?q= in sync via history.replaceState when quick-filter chips change', async () => {
    const replaceSpy = vi.spyOn(window.history, 'replaceState');
    const el = await mount();
    expect(window.location.search).toBe('');

    const chipByText = (label: string): HTMLButtonElement => {
      const btn = Array.from(
        el.shadowRoot!.querySelectorAll<HTMLButtonElement>('.filter-chip')
      ).find((b) => b.textContent?.trim() === label);
      if (!btn) throw new Error(`Missing chip: ${label}`);
      return btn;
    };

    chipByText('Review pending').click();
    await settle(el);
    expect(new URLSearchParams(window.location.search).get('q')).toBe('is:review');
    expect(replaceSpy).toHaveBeenCalled();

    chipByText('Owned by me').click();
    await settle(el);
    expect(new URLSearchParams(window.location.search).get('q')).toBe('is:mine is:review');

    // Enabling "Shared with me" replaces is:mine with is:shared.
    chipByText('Shared with me').click();
    await settle(el);
    expect(new URLSearchParams(window.location.search).get('q')).toBe('is:shared is:review');

    // Clicking active chips toggles them off and clears ?q=.
    chipByText('Shared with me').click();
    await settle(el);
    chipByText('Review pending').click();
    await settle(el);
    expect(window.location.search).toBe('');
  });
});
