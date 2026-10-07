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
 * Artifacts list page: gated on hub.artifacts, lists GET
 * /api/v1/artifacts?mine=1 with search, review-pending and owned-by-me
 * filters and "Load more" paging; rows open the artifact page; the empty
 * state offers buttons, not CLI commands.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ArtifactListItem, ArtifactListResponse } from '../../client/artifacts.js';
import type { ScionPageArtifacts } from './artifacts.js';

const ME = 'user-me';

function item(n: number, extra: Partial<ArtifactListItem> = {}): ArtifactListItem {
  const id = `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
  return {
    id,
    ref: `scion://artifact/${id}`,
    scopeKind: 'project',
    scopeRef: 'proj-1',
    ownerKind: 'agent',
    ownerRef: 'agent-1',
    title: `Artifact ${n}`,
    currentSeq: 1,
    createdAt: '2026-10-05T12:00:00Z',
    updatedAt: '2026-10-05T12:00:00Z',
    reviewPending: false,
    ...extra,
  };
}

interface Mock {
  urls: string[];
}

/**
 * Mocks fetch. pages maps a list URL's cursor ('' for the first page) to its
 * response; lookups of agents, users and projects answer from names (404
 * when absent). listStatus overrides the list response status.
 */
function mockFetch(
  pages: Record<string, ArtifactListResponse>,
  names: Record<string, string> = {},
  listStatus = 200
): Mock {
  const m: Mock = { urls: [] };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      m.urls.push(url);
      if (url.startsWith('/api/v1/artifacts?')) {
        if (listStatus !== 200) {
          return Promise.resolve(
            new Response('{"error":{"code":"internal","message":"boom"}}', { status: listStatus })
          );
        }
        const cursor = new URL(url, 'http://x').searchParams.get('cursor') ?? '';
        const page = pages[cursor] ?? { artifacts: [] };
        return Promise.resolve(
          new Response(JSON.stringify(page), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      const name = names[url];
      if (name !== undefined) {
        return Promise.resolve(
          new Response(JSON.stringify({ name, displayName: name }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response('{"error":{"code":"forbidden","message":"denied"}}', { status: 403 })
      );
    })
  );
  return m;
}

async function settle(el: ScionPageArtifacts): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(flag: boolean): Promise<ScionPageArtifacts> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': flag };
  const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
  el.pageData = {
    path: '/artifacts',
    title: 'Artifacts',
    user: { id: ME, email: 'me@example.com', name: 'Me' },
  } as ScionPageArtifacts['pageData'];
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function listUrls(m: Mock): URLSearchParams[] {
  return m.urls
    .filter((u) => u.startsWith('/api/v1/artifacts?'))
    .map((u) => new URL(u, 'http://x').searchParams);
}

function rows(el: ScionPageArtifacts): HTMLTableRowElement[] {
  return Array.from(el.shadowRoot!.querySelectorAll('tbody tr'));
}

describe('artifacts list page', () => {
  beforeAll(async () => {
    await import('./artifacts.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.useRealTimers();
    delete window.__SCION_FEATURES__;
  });

  it('shows only a 404 and calls nothing when the experiment is off', async () => {
    const m = mockFetch({ '': { artifacts: [item(1)] } });
    const el = await mount(false);
    expect(el.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('table')).toBeNull();
    expect(m.urls).toHaveLength(0);
  });

  it('lists the caller’s artifacts with owner, project, status and a link to each page', async () => {
    const mine = item(1, {
      ownerKind: 'user',
      ownerRef: ME,
      key: 'weekly-report',
      title: 'Weekly',
    });
    const review = item(2, { reviewPending: true, title: 'Design notes' });
    const m = mockFetch(
      { '': { artifacts: [mine, review] } },
      {
        '/api/v1/agents/agent-1': 'docs-writer',
        '/api/v1/projects/proj-1': 'Web Frontend',
      }
    );
    const el = await mount(true);

    expect(listUrls(m)[0].get('mine')).toBe('1');
    expect(el.shadowRoot!.querySelector('h1')!.textContent).toBe('Artifacts');
    const r = rows(el);
    expect(r).toHaveLength(2);

    const link = r[0].querySelector('a')!;
    expect(link.getAttribute('href')).toBe(`/projects/proj-1/artifacts/${mine.id}`);
    expect(link.textContent).toBe('Weekly');
    expect(r[0].querySelector('.key')!.textContent).toBe('weekly-report');
    expect(r[0].textContent).toContain('You');
    expect(r[0].querySelector('sl-badge')).toBeNull();

    expect(r[1].textContent).toContain('docs-writer');
    expect(r[1].textContent).toContain('(agent)');
    expect(r[1].textContent).toContain('Web Frontend');
    expect(r[1].querySelector('sl-badge')!.textContent).toContain('Review pending');

    // The caller is never looked up; names are fetched once each.
    expect(m.urls.filter((u) => u.startsWith('/api/v1/users/'))).toHaveLength(0);
    expect(m.urls.filter((u) => u === '/api/v1/projects/proj-1')).toHaveLength(1);
  });

  it('shows ids when a name lookup is not allowed', async () => {
    mockFetch({ '': { artifacts: [item(1, { ownerKind: 'user', ownerRef: 'user-other' })] } });
    const el = await mount(true);
    const r = rows(el)[0];
    expect(r.textContent).toContain('user-other');
    expect(r.textContent).toContain('proj-1');
  });

  it('opens the artifact page when a row is clicked', async () => {
    const a = item(7, { scopeRef: 'proj-9' });
    mockFetch({ '': { artifacts: [a] } });
    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      const el = await mount(true);
      rows(el)[0].click();
      expect(paths).toEqual([`/projects/proj-9/artifacts/${a.id}`]);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
  });

  it('empty state offers buttons, not CLI commands', async () => {
    mockFetch({ '': { artifacts: [] } });
    const el = await mount(true);
    const empty = el.shadowRoot!.querySelector('.empty-state')!;
    expect(empty.textContent).toContain('No Artifacts Found');
    expect(empty.textContent).not.toContain('scion ');
    expect(empty.querySelector('code, pre')).toBeNull();
    const buttons = Array.from(empty.querySelectorAll('sl-button'));
    expect(buttons.map((b) => b.textContent!.trim())).toEqual([
      'Browse projects',
      'Learn about artifacts',
    ]);
    expect(buttons[0].getAttribute('href')).toBe('/projects');
    expect(buttons[1].getAttribute('href')).toContain('/reference/artifacts/');
  });

  it('filters by search (debounced), review pending and owned by me', async () => {
    const m = mockFetch({ '': { artifacts: [item(1)] } });
    const el = await mount(true);

    vi.useFakeTimers();
    const search = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
    search.value = 'notes';
    search.dispatchEvent(new Event('sl-input'));
    search.value = 'notes q3';
    search.dispatchEvent(new Event('sl-input'));
    await vi.advanceTimersByTimeAsync(350);
    vi.useRealTimers();
    await settle(el);
    let last = listUrls(m).at(-1)!;
    expect(last.get('q')).toBe('notes q3');
    expect(listUrls(m)).toHaveLength(2); // initial + one debounced search

    const select = el.shadowRoot!.querySelector('sl-select') as HTMLElement & { value: string };
    select.value = 'review';
    select.dispatchEvent(new Event('sl-change'));
    await settle(el);
    last = listUrls(m).at(-1)!;
    expect(last.get('review_pending')).toBe('1');
    expect(last.get('q')).toBe('notes q3');

    const owned = el.shadowRoot!.querySelector('sl-checkbox') as HTMLElement & { checked: boolean };
    owned.checked = true;
    owned.dispatchEvent(new Event('sl-change'));
    await settle(el);
    last = listUrls(m).at(-1)!;
    expect(last.get('owner')).toBe('me');
    expect(last.get('review_pending')).toBe('1');
  });

  it('says no match, not "no artifacts", when filters exclude everything', async () => {
    mockFetch({ '': { artifacts: [] } });
    const el = await mount(true);
    const owned = el.shadowRoot!.querySelector('sl-checkbox') as HTMLElement & { checked: boolean };
    owned.checked = true;
    owned.dispatchEvent(new Event('sl-change'));
    await settle(el);
    const empty = el.shadowRoot!.querySelector('.empty-state')!;
    expect(empty.textContent).toContain('No Matching Artifacts');
    expect(empty.querySelector('sl-button')).toBeNull();
  });

  it('loads more pages with the cursor and appends them', async () => {
    const m = mockFetch({
      '': { artifacts: [item(1), item(2)], nextCursor: 'c1.page2' },
      'c1.page2': { artifacts: [item(3)] },
    });
    const el = await mount(true);
    expect(rows(el)).toHaveLength(2);
    const more = el.shadowRoot!.querySelector('.load-more sl-button') as HTMLElement;
    expect(more.textContent).toContain('Load more');
    more.click();
    await settle(el);
    expect(rows(el)).toHaveLength(3);
    expect(listUrls(m).at(-1)!.get('cursor')).toBe('c1.page2');
    expect(el.shadowRoot!.querySelector('.load-more')).toBeNull();
  });

  it('shows the error with a retry', async () => {
    mockFetch({}, {}, 500);
    const el = await mount(true);
    const err = el.shadowRoot!.querySelector('.error-state')!;
    expect(err.textContent).toContain('boom');
    expect(err.querySelector('sl-button')!.textContent).toContain('Retry');
  });

  it('drops a slow response for filters that changed meanwhile', async () => {
    let releaseFirst: (() => void) | null = null;
    const first = new Promise<void>((r) => (releaseFirst = r));
    let calls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (!url.startsWith('/api/v1/artifacts?')) return new Response('{}', { status: 404 });
        calls++;
        if (calls === 1) await first;
        const title = calls === 1 ? 'stale' : 'fresh';
        return new Response(JSON.stringify({ artifacts: [item(calls, { title })] }), {
          status: 200,
        });
      })
    );
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = { path: '/artifacts', title: 'Artifacts' } as ScionPageArtifacts['pageData'];
    document.body.appendChild(el);
    await el.updateComplete;
    const owned = el.shadowRoot!.querySelector('sl-checkbox') as HTMLElement & { checked: boolean };
    owned.checked = true;
    owned.dispatchEvent(new Event('sl-change'));
    await settle(el);
    releaseFirst!();
    await settle(el);
    expect(rows(el).map((r) => r.querySelector('a')!.textContent)).toEqual(['fresh']);
  });
});
