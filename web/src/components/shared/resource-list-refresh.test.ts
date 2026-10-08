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
import type { ScionResourceList } from './resource-list.js';
import { REFRESH_ALL_CONCURRENCY, runWithConcurrency } from './resource-list.js';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

const GH_SOURCE = 'https://github.com/acme/repo/tree/main/.scion/templates/a';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(el: ScionResourceList): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function createList(
  kind: 'template' | 'harness-config',
  items: Array<Record<string, unknown>>
): Promise<{ el: ScionResourceList; fetchMock: ReturnType<typeof vi.fn> }> {
  const key = kind === 'template' ? 'templates' : 'harnessConfigs';
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (init?.method === 'POST') return Promise.resolve(jsonResponse({ count: 1 }));
    return Promise.resolve(jsonResponse({ [key]: items }));
  });
  vi.stubGlobal('fetch', fetchMock);
  const el = document.createElement('scion-resource-list') as ScionResourceList;
  el.kind = kind;
  el.scope = 'global';
  document.body.appendChild(el);
  await flush(el);
  return { el, fetchMock };
}

function refreshAllButton(el: HTMLElement): HTMLElement | undefined {
  return Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
    (b.textContent ?? '').includes('Refresh All from Source')
  ) as HTMLElement | undefined;
}

describe('resource list: Refresh All from Source', () => {
  let element: ScionResourceList | null = null;

  beforeAll(async () => {
    await import('./resource-list.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('is offered for templates with a GitHub source', async () => {
    const { el } = await createList('template', [
      { id: 'a', name: 'a', sourceUrl: GH_SOURCE },
      { id: 'b', name: 'b' },
    ]);
    element = el;
    expect(refreshAllButton(el)).toBeDefined();
  });

  it('is not offered when only built-in or source-less templates exist', async () => {
    const { el } = await createList('template', [
      { id: 'd', name: 'default', sourceUrl: 'builtin://scion/1.0/template/default' },
      { id: 'b', name: 'b' },
    ]);
    element = el;
    expect(refreshAllButton(el)).toBeUndefined();
  });

  it('refreshes only templates with a GitHub source', async () => {
    const { el, fetchMock } = await createList('template', [
      { id: 'a', name: 'a', sourceUrl: GH_SOURCE },
      { id: 'd', name: 'default', sourceUrl: 'builtin://scion/1.0/template/default' },
      { id: 'b', name: 'b' },
    ]);
    element = el;
    refreshAllButton(el)!.click();
    await flush(el);
    const posts = fetchMock.mock.calls
      .filter(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
      .map(([url]) => url);
    expect(posts).toEqual([expect.stringContaining('/api/v1/templates/a/reimport')]);
    const { showToast } = await import('../../utils/toast.js');
    expect(showToast).toHaveBeenCalledWith('Refreshed 1 template successfully', 'success');
  });

  it('runs at most a few refreshes at the same time', async () => {
    const items = Array.from({ length: 9 }, (_, i) => ({
      id: `t${i}`,
      name: `t${i}`,
      sourceUrl: GH_SOURCE,
    }));
    let inFlight = 0;
    let maxInFlight = 0;
    let posts = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit) => {
        if (init?.method !== 'POST') return Promise.resolve(jsonResponse({ templates: items }));
        posts++;
        inFlight++;
        maxInFlight = Math.max(maxInFlight, inFlight);
        return new Promise<Response>((resolve) =>
          setTimeout(() => {
            inFlight--;
            resolve(jsonResponse({ count: 1 }));
          }, 5)
        );
      })
    );
    const el = document.createElement('scion-resource-list') as ScionResourceList;
    el.kind = 'template';
    el.scope = 'global';
    document.body.appendChild(el);
    element = el;
    await flush(el);

    refreshAllButton(el)!.click();
    for (let i = 0; i < 100 && (posts < 9 || inFlight > 0); i++) {
      await new Promise((r) => setTimeout(r, 5));
    }
    await flush(el);
    expect(posts).toBe(9);
    expect(REFRESH_ALL_CONCURRENCY).toBeLessThanOrEqual(4);
    expect(maxInFlight).toBeLessThanOrEqual(4);
    expect(maxInFlight).toBeGreaterThan(1);
  });

  it('runWithConcurrency keeps item order and the limit', async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    const out = await runWithConcurrency([1, 2, 3, 4, 5, 6, 7], 3, async (n) => {
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      await new Promise((r) => setTimeout(r, 8 - n));
      inFlight--;
      return n * 10;
    });
    expect(out).toEqual([10, 20, 30, 40, 50, 60, 70]);
    expect(maxInFlight).toBe(3);
    expect(await runWithConcurrency([], 4, async (n: number) => n)).toEqual([]);
  });

  it('still offers it for harness configs with any source', async () => {
    const { el } = await createList('harness-config', [
      { id: 'h', name: 'h', sourceUrl: 'https://example.com/hc.tgz' },
    ]);
    element = el;
    expect(refreshAllButton(el)).toBeDefined();
  });
});
