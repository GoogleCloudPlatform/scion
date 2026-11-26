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
import { requestUrl } from '../../client/__fixtures__/request-url.js';

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
    reviewOnLatest: false,
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
      const url = requestUrl(input);
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
    window.history.replaceState({}, '', '/');
  });

  it('shows rows with version, owner and the review badge, and pages with Load more', async () => {
    const { el, urls } = await mountList({
      'cursor=c1': { artifacts: [item('c')] },
      'mine=1': {
        artifacts: [item('a', { key: 'k-a' }), item('b', { reviewOnLatest: true })],
        nextCursor: 'c1',
      },
    });
    const rows = el.shadowRoot!.querySelectorAll('tbody tr');
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('Title a');
    expect(rows[0].textContent).toContain('k-a');
    expect(rows[0].textContent).toContain('v2');
    expect(rows[0].textContent).toContain('agent-01… (agent)');
    expect(rows[1].querySelector('sl-badge')!.textContent).toContain('Review on latest');
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

  it('S1: a row whose owner was deleted has the Owner deleted badge and a struck owner', async () => {
    const { el } = await mountList({
      'mine=1': {
        artifacts: [item('a', { ownerDeleted: true, reviewOnLatest: true }), item('b')],
      },
    });
    const [gone, live] = Array.from(el.shadowRoot!.querySelectorAll('tbody tr'));
    expect(el.shadowRoot!.querySelectorAll('thead th')[4]!.textContent).toBe('Status');
    const badge = gone!.querySelector('sl-badge.owner-deleted')!;
    expect(badge.textContent).toContain('Owner deleted');
    expect(badge.querySelector('sl-icon')!.getAttribute('name')).toBe('person-x');
    // Beside Review on latest, not instead of it.
    expect(gone!.textContent).toContain('Review on latest');
    // The deleted agent's name cannot be looked up: its short id, struck through.
    // Only the name is struck through; the (agent) tag stays outside (mock S1).
    expect(gone!.querySelector('.owner-gone')!.textContent).toBe('agent-01…');
    expect((gone as HTMLTableRowElement).cells[1]!.textContent).toContain('agent-01… (agent)');
    // The badge's explanation is reachable by keyboard and screen readers.
    const b = gone!.querySelector('sl-badge.owner-deleted')!;
    expect(b.getAttribute('tabindex')).toBe('0');
    expect(b.getAttribute('aria-label')).toContain('stays readable until a custodian deletes it');
    expect(live!.querySelector('sl-badge.owner-deleted')).toBeNull();
    expect(live!.querySelector('.owner-gone')).toBeNull();
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
    const { el } = await mountList({ 'mine=1': { artifacts: [], canCreate: true } });
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

  it('searches with the typed text on filter commit', async () => {
    const { el, urls } = await mountList({});
    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'report',
          query: { mode: 'text', text: 'report' },
          serverQ: 'report',
        },
        bubbles: true,
        composed: true,
      })
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(urls.filter((u) => u.includes('q='))).toEqual([
      '/api/v1/artifacts?mine=1&scope=p-1&q=report',
    ]);
  });

  it('shows New artifact only when the list says the caller may publish here (ptone/scion#4584)', async () => {
    const newButtons = (el: ListElement): string[] =>
      Array.from(el.shadowRoot!.querySelectorAll('sl-button'))
        .map((b) => b.textContent!.trim())
        .filter((t) => t === 'New artifact');
    const { el: empty } = await mountList({ 'mine=1': { artifacts: [] } });
    expect(empty.shadowRoot!.querySelector('.empty h3')).not.toBeNull();
    expect(newButtons(empty)).toEqual([]);
    empty.remove();

    const { el: listed } = await mountList({ 'mine=1': { artifacts: [item('a')] } });
    expect(listed.shadowRoot!.querySelectorAll('tbody tr').length).toBeGreaterThan(0);
    expect(newButtons(listed)).toEqual([]);
    listed.remove();

    const { el: allowed } = await mountList({
      'mine=1': { artifacts: [item('a')], canCreate: true },
    });
    expect(newButtons(allowed)).toEqual(['New artifact']);
  });
});

describe('artifact list names', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    resetPrincipalNames();
    window.history.replaceState({}, '', '/');
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
        const url = requestUrl(input);
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
    window.history.replaceState({}, '', '/');
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

  it('drops a Load more still in flight when a new filter query is committed', async () => {
    const urls: string[] = [];
    let releaseMore: (() => void) | null = null;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = requestUrl(input);
        urls.push(url);
        if (url.includes('cursor=c1')) {
          // Answers only when released, like a slow page; the abort does not reject it.
          return new Promise<Response>((resolve) => {
            releaseMore = (): void =>
              resolve(new Response(JSON.stringify({ artifacts: [item('old')], nextCursor: 'c2' })));
            void init;
          });
        }
        if (url.includes('q=x')) {
          return Promise.resolve(
            new Response(JSON.stringify({ artifacts: [item('matched')] }), { status: 200 })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ artifacts: [item('a')], nextCursor: 'c1' }), {
            status: 200,
          })
        );
      })
    );
    const el = document.createElement('scion-artifact-list') as ListElement;
    el.projectId = 'p-1';
    document.body.appendChild(el);
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    (el.shadowRoot!.querySelector('.more sl-button') as HTMLElement).click();
    await el.updateComplete;
    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: { value: 'x', query: { mode: 'text', text: 'x' }, serverQ: 'x' },
        bubbles: true,
        composed: true,
      })
    );
    releaseMore!();
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(el.shadowRoot!.textContent).not.toContain('Title old');
    expect(el.shadowRoot!.textContent).toContain('Title matched');
    expect(el.shadowRoot!.querySelector('.more sl-button')).toBeNull();
  });

  it('keeps Load more on an invalid edit and when typing then reverting to the committed query', async () => {
    const { el, urls } = await mountList({
      'mine=1': { artifacts: [item('a')], nextCursor: 'c1' },
    });
    expect(el.shadowRoot!.querySelector('.more sl-button')).not.toBeNull();
    const callsBefore = urls.length;
    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };

    // Incomplete/invalid edit emits filter-input (valid: false) with no commit:
    // Load more stays visible and no request fires.
    box.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: { value: 'is:', query: { mode: 'text', text: '' }, valid: false },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.more sl-button')).not.toBeNull();

    // Reverting back to the committed query emits filter-commit with the same serverQ:
    // Load more remains available and no reload is issued.
    box.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: { value: '', query: { mode: 'text', text: '' }, valid: true },
        bubbles: true,
        composed: true,
      })
    );
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: { value: '', query: { mode: 'text', text: '' }, serverQ: '' },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.more sl-button')).not.toBeNull();
    expect(urls.length).toBe(callsBefore);
  });

  it('does not abort the initial load on a keystroke before commit', async () => {
    let resolveInitial: ((r: Response) => void) | null = null;
    let initialAborted = false;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = requestUrl(input);
        if (url.startsWith('/api/v1/artifacts')) {
          init?.signal?.addEventListener('abort', () => {
            initialAborted = true;
          });
          return new Promise<Response>((resolve) => {
            resolveInitial = resolve;
          });
        }
        return Promise.resolve(new Response('{}', { status: 404 }));
      })
    );
    const el = document.createElement('scion-artifact-list') as ListElement;
    el.projectId = 'p-1';
    document.body.appendChild(el);
    await el.updateComplete;

    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };
    box.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: { value: 'is:', query: { mode: 'text', text: '' }, valid: false },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(initialAborted).toBe(false);

    resolveInitial!(
      new Response(JSON.stringify({ artifacts: [item('a')], nextCursor: 'c1' }), { status: 200 })
    );
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(el.shadowRoot!.textContent).toContain('Title a');
    expect(el.shadowRoot!.querySelector('.more sl-button')).not.toBeNull();
  });

  it('badges artifacts shared with the project, names their project and filters to them', async () => {
    const { el, urls } = await mountList({
      'q=is%3Ashared': { artifacts: [item('s', { scopeRef: 'p-2', sharedWithScope: true })] },
      'api/v1/projects/p-2': { name: 'web-frontend' },
      'mine=1': {
        artifacts: [item('a'), item('s', { scopeRef: 'p-2', sharedWithScope: true })],
      },
    });
    let rows = el.shadowRoot!.querySelectorAll('tbody tr');
    expect(rows[0].querySelector('sl-badge.shared')).toBeNull();
    expect(rows[1].querySelector('sl-badge.shared')!.textContent).toContain(
      'Shared with this project'
    );
    expect(rows[1].querySelector('.from')!.textContent!.replace(/\s+/g, ' ')).toContain(
      'from web-frontend'
    );
    // The shared artifact opens under its own project.
    expect(rows[1].querySelector('a.title')!.getAttribute('href')).toBe(
      '/projects/p-2/artifacts/s'
    );

    const chip = el.shadowRoot!.querySelector<HTMLButtonElement>('.filter-chip')!;
    expect(chip.textContent).toContain('Shared with this project');
    chip.click();
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(urls.at(-1)).toContain('q=is%3Ashared');
    rows = el.shadowRoot!.querySelectorAll('tbody tr');
    expect(rows).toHaveLength(1);
  });

  it('renders <scion-filter-box> and query-editing chip', async () => {
    window.history.replaceState({}, '', '/projects/p-1?tab=artifacts');
    try {
      const { el, urls } = await mountList({
        'q=is%3Ashared': { artifacts: [item('s', { scopeRef: 'p-2', sharedWithScope: true })] },
        'mine=1': {
          artifacts: [item('a'), item('s', { scopeRef: 'p-2', sharedWithScope: true })],
        },
      });
      const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & {
        view: string;
        value: string;
      };
      expect(box).not.toBeNull();
      expect(box.view).toBe('artifacts-project');
      expect(el.shadowRoot!.querySelector('.toolbar sl-checkbox')).toBeNull();

      const chip = el.shadowRoot!.querySelector<HTMLButtonElement>('.filter-chip')!;
      expect(chip.textContent?.trim()).toBe('Shared with this project');
      expect(chip.getAttribute('aria-pressed')).toBe('false');

      chip.click();
      for (let i = 0; i < 10; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
      expect(box.value).toBe('is:shared');
      expect(chip.getAttribute('aria-pressed')).toBe('true');
      expect(new URLSearchParams(window.location.search).get('q')).toBe('is:shared');
      expect(new URLSearchParams(window.location.search).get('tab')).toBe('artifacts');
      expect(urls.at(-1)).toContain('q=is%3Ashared');
      expect(urls.at(-1)).not.toContain('shared=1');
      expect(el.shadowRoot!.querySelectorAll('tbody tr')).toHaveLength(1);
    } finally {
      delete window.__SCION_FEATURES__;
      window.history.replaceState({}, '', '/');
    }
  });

  it('uses custom urlParam (aq) without touching q when configured', async () => {
    window.history.replaceState({}, '', '/projects/p-1?q=phase%3Arunning&aq=is%3Ashared');
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        urls.push(url);
        return Promise.resolve(
          new Response(JSON.stringify({ artifacts: [item('s')] }), { status: 200 })
        );
      })
    );
    try {
      const el = document.createElement('scion-artifact-list') as ListElement & {
        urlParam: string;
      };
      el.projectId = 'p-1';
      el.urlParam = 'aq';
      document.body.appendChild(el);
      for (let i = 0; i < 10; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
      const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & {
        value: string;
      };
      expect(box.value).toBe('is:shared');
      expect(urls[0]).toContain('q=is%3Ashared');

      // Toggling the chip clears `aq` while leaving `q=phase:running` untouched.
      const chip = el.shadowRoot!.querySelector<HTMLButtonElement>('.filter-chip')!;
      chip.click();
      for (let i = 0; i < 10; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
      const params = new URLSearchParams(window.location.search);
      expect(params.get('aq')).toBeNull();
      expect(params.get('q')).toBe('phase:running');
    } finally {
      delete window.__SCION_FEATURES__;
      window.history.replaceState({}, '', '/');
    }
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

  it('skips hidden folders at the top of the picked folder; the picked folder name is not a path', () => {
    const { folder, files } = folderFiles([
      f('.github/workflows/ci.yml'),
      f('.github/.cache/x'),
      f('.github/README.md'),
    ]);
    expect(folder).toBe('.github');
    expect(files.map((x) => x.path)).toEqual(['README.md', 'workflows/ci.yml']);
    const nested = folderFiles([f('site/.hidden/a.txt'), f('site/ok.txt')]);
    expect(nested.files.map((x) => x.path)).toEqual(['ok.txt']);
  });

  it('picks an index or README entry, else a top-level file', () => {
    expect(defaultEntry(['a/b.md', 'README.md', 'index.html'])).toBe('index.html');
    expect(defaultEntry(['a/b.md', 'README.md'])).toBe('README.md');
    expect(defaultEntry(['a/b.md', 'notes.txt'])).toBe('notes.txt');
    expect(defaultEntry(['a/b.md'])).toBe('a/b.md');
    expect(defaultEntry([])).toBe('');
  });
});
