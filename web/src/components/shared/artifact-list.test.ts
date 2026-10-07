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
 * Project artifacts list and the publish dialog helpers.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import { defaultEntry, folderFiles } from './artifact-publish-dialog.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import type { ArtifactListItem } from '../../client/artifacts.js';

function item(id: string, extra: Partial<ArtifactListItem> = {}): ArtifactListItem {
  return {
    id,
    ref: `scion://artifact/${id}`,
    scopeKind: 'project',
    scopeRef: 'p-1',
    ownerKind: 'agent',
    ownerRef: 'agent-0123456789abcdef',
    title: `Title ${id}`,
    currentSeq: 2,
    createdAt: '2026-10-05T12:00:00Z',
    updatedAt: '2026-10-05T12:00:00Z',
    reviewPending: false,
    ...extra,
  };
}

type ListElement = HTMLElement & { projectId: string; updateComplete: Promise<boolean> };

async function mountList(
  pages: Record<string, unknown>,
  me = ''
): Promise<{ el: ListElement; urls: string[] }> {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      const key = Object.keys(pages).find((k) => url.includes(k)) ?? '';
      return Promise.resolve(
        new Response(JSON.stringify(pages[key] ?? { artifacts: [] }), { status: 200 })
      );
    })
  );
  const el = document.createElement('scion-artifact-list') as ListElement & {
    currentUserId: string;
  };
  el.projectId = 'p-1';
  el.currentUserId = me;
  document.body.appendChild(el);
  for (let i = 0; i < 10; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
  return { el, urls };
}

describe('artifact list', () => {
  beforeAll(async () => {
    await import('./artifact-list.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    resetPrincipalNames();
  });

  it('shows rows with version, owner and the review badge, and pages with Load more', async () => {
    const { el, urls } = await mountList({
      'cursor=c1': { artifacts: [item('c')] },
      'mine=1': {
        artifacts: [item('a', { key: 'k-a' }), item('b', { reviewPending: true })],
        nextCursor: 'c1',
      },
    });
    const rows = el.shadowRoot!.querySelectorAll('tbody tr');
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('Title a');
    expect(rows[0].textContent).toContain('k-a');
    expect(rows[0].textContent).toContain('v2');
    expect(rows[0].textContent).toContain('agent-01… (agent)');
    expect(rows[1].querySelector('sl-badge')!.textContent).toContain('Review pending');
    expect(rows[0].querySelector('a.title')!.getAttribute('href')).toBe(
      '/projects/p-1/artifacts/a'
    );

    const more = el.shadowRoot!.querySelector('.more sl-button') as HTMLElement;
    more.click();
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(urls).toContain('/api/v1/artifacts?mine=1&scope=p-1&cursor=c1');
    expect(el.shadowRoot!.querySelectorAll('tbody tr')).toHaveLength(3);
  });

  it('opens the artifact page when a row is clicked', async () => {
    const { el } = await mountList({ 'mine=1': { artifacts: [item('a')] } });
    const nav = vi.fn();
    document.addEventListener('nav-click', nav);
    try {
      (el.shadowRoot!.querySelector('tbody tr') as HTMLElement).click();
      expect((nav.mock.calls[0][0] as CustomEvent<{ path: string }>).detail.path).toBe(
        '/projects/p-1/artifacts/a'
      );
    } finally {
      document.removeEventListener('nav-click', nav);
    }
  });

  it('has an empty state with a button, one sentence, and the agent command only in the help', async () => {
    const { el } = await mountList({});
    const empty = el.shadowRoot!.querySelector('.empty')!;
    expect(empty.querySelector('h3')!.textContent).toBe('No artifacts in this project yet');
    expect(empty.querySelectorAll(':scope > p')).toHaveLength(1);
    expect(empty.querySelector(':scope > sl-button')!.textContent).toContain('New artifact');
    const help = empty.querySelector('.help sl-dropdown .help-panel')!;
    expect(help.textContent).toContain('scion artifact publish');
    const outsideHelp = Array.from(empty.childNodes)
      .filter((n) => !(n instanceof Element && n.classList.contains('help')))
      .map((n) => n.textContent)
      .join('');
    expect(outsideHelp).not.toContain('scion artifact');
  });

  it('searches with the typed text after a pause', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const { el, urls } = await (async () => {
        vi.useRealTimers();
        const r = await mountList({});
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
        return r;
      })();
      const input = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
      input.value = 'rep';
      input.dispatchEvent(new CustomEvent('sl-input'));
      input.value = 'report';
      input.dispatchEvent(new CustomEvent('sl-input'));
      vi.advanceTimersByTime(300);
      vi.useRealTimers();
      await new Promise((r) => setTimeout(r, 0));
      expect(urls.filter((u) => u.includes('q='))).toEqual([
        '/api/v1/artifacts?mine=1&scope=p-1&q=report',
      ]);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('artifact list names', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    resetPrincipalNames();
  });

  it('shows owner names, You for the signed-in user, and looks each owner up once', async () => {
    const { el, urls } = await mountList(
      {
        '/api/v1/agents/agent-0123456789abcdef': { name: 'docs-writer' },
        '/api/v1/users/u-2': { displayName: 'Jane Doe' },
        'mine=1': {
          artifacts: [
            item('a'),
            item('b'),
            item('c', { ownerKind: 'user', ownerRef: 'u-1' }),
            item('d', { ownerKind: 'user', ownerRef: 'u-2' }),
          ],
        },
      },
      'u-1'
    );
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    const owners = Array.from(el.shadowRoot!.querySelectorAll('tbody tr')).map((r) =>
      r.querySelectorAll('td')[1].textContent!.trim()
    );
    expect(owners).toEqual(['docs-writer (agent)', 'docs-writer (agent)', 'You', 'Jane Doe']);
    expect(urls.filter((u) => u.startsWith('/api/v1/agents/'))).toHaveLength(1);
    expect(urls.some((u) => u === '/api/v1/users/u-1')).toBe(false);
  });
});

describe('publish dialog retry', () => {
  beforeAll(async () => {
    await import('./artifact-publish-dialog.js');
  });
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it('resumes the version a failed attempt left instead of creating another artifact', async () => {
    const calls: string[] = [];
    let putStatus = 500;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? 'GET';
        calls.push(`${method} ${url}`);
        if (method === 'POST' && url === '/api/v1/artifacts') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                artifact: { id: 'n-1' },
                version: { seq: 1 },
                upload: { required: ['a.md'] },
              }),
              { status: 201 }
            )
          );
        }
        if (method === 'PUT') {
          return Promise.resolve(
            putStatus === 204
              ? new Response(null, { status: 204 })
              : new Response('{"error":{"code":"internal","message":"boom"}}', {
                  status: putStatus,
                })
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({ artifact: { id: 'n-1', scopeRef: 'p-1' }, version: { seq: 1 } }),
            {
              status: 200,
            }
          )
        );
      })
    );
    const el = document.createElement('scion-artifact-publish-dialog') as HTMLElement & {
      projectId: string;
      open: boolean;
      updateComplete: Promise<boolean>;
    };
    el.projectId = 'p-1';
    document.body.appendChild(el);
    el.open = true;
    await el.updateComplete;
    const priv = el as unknown as {
      picked: { path: string; file: File }[];
      entry: string;
      titleValue: string;
    };
    priv.picked = [{ path: 'a.md', file: new File(['x'], 'a.md') }];
    priv.entry = 'a.md';
    priv.titleValue = 'Notes';
    await el.updateComplete;
    const published = vi.fn();
    el.addEventListener('artifact-published', published);
    const clickPublish = async (): Promise<void> => {
      const btn = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
        (b) => b.textContent!.trim() === 'Publish'
      ) as HTMLElement;
      btn.click();
      for (let i = 0; i < 20; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
    };
    await clickPublish();
    expect(el.shadowRoot!.querySelector('sl-alert')!.textContent).toContain(
      'Publish again to retry'
    );
    putStatus = 204;
    await clickPublish();
    expect(calls.filter((c) => c === 'POST /api/v1/artifacts')).toHaveLength(1);
    expect(calls).toContain('POST /api/v1/artifacts/n-1/versions/1/finalize');
    expect(published).toHaveBeenCalledTimes(1);
  });
});

describe('artifact list paging', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    resetPrincipalNames();
  });

  it('follows the cursor past empty pages instead of showing the empty state', async () => {
    const { el, urls } = await mountList({
      'cursor=c2': { artifacts: [item('z')] },
      'cursor=c1': { artifacts: [], nextCursor: 'c2' },
      'mine=1': { artifacts: [], nextCursor: 'c1' },
    });
    expect(urls.filter((u) => u.startsWith('/api/v1/artifacts'))).toEqual([
      '/api/v1/artifacts?mine=1&scope=p-1',
      '/api/v1/artifacts?mine=1&scope=p-1&cursor=c1',
      '/api/v1/artifacts?mine=1&scope=p-1&cursor=c2',
    ]);
    expect(el.shadowRoot!.querySelector('.empty')).toBeNull();
    expect(el.shadowRoot!.querySelectorAll('tbody tr')).toHaveLength(1);
  });

  it('drops Load more as soon as the search text changes', async () => {
    const { el } = await mountList({ 'mine=1': { artifacts: [item('a')], nextCursor: 'c1' } });
    expect(el.shadowRoot!.querySelector('.more sl-button')).not.toBeNull();
    const input = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
    input.value = 'x';
    input.dispatchEvent(new CustomEvent('sl-input'));
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.more sl-button')).toBeNull();
  });
});

describe('publish dialog helpers', () => {
  function f(rel: string): File {
    const file = new File(['x'], rel.split('/').pop()!);
    Object.defineProperty(file, 'webkitRelativePath', { value: rel });
    return file;
  }

  it('strips the folder name and skips hidden files and folders', () => {
    const { folder, files } = folderFiles([
      f('site/index.html'),
      f('site/css/a.css'),
      f('site/.git/config'),
      f('site/.env'),
      f('site/img/.DS_Store'),
    ]);
    expect(folder).toBe('site');
    expect(files.map((x) => x.path)).toEqual(['css/a.css', 'index.html']);
  });

  it('picks an index or README entry, else a top-level file', () => {
    expect(defaultEntry(['a/b.md', 'README.md', 'index.html'])).toBe('index.html');
    expect(defaultEntry(['a/b.md', 'README.md'])).toBe('README.md');
    expect(defaultEntry(['a/b.md', 'notes.txt'])).toBe('notes.txt');
    expect(defaultEntry(['a/b.md'])).toBe('a/b.md');
    expect(defaultEntry([])).toBe('');
  });
});
