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
 * Artifact page (ptone/scion#3213): gated on hub.artifacts, renders
 * markdown / text / images, and shows 404 for missing or unreadable
 * artifacts.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse, ArtifactVersion } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { resetPrincipalNames } from '../../client/principal-names.js';

const ID = '5f1c2d3e-0000-4000-8000-000000000001';

function artifact(path: string, mediaType: string): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'user',
      ownerRef: 'u-1',
      title: 'Design',
      currentSeq: 1,
      createdAt: '2026-10-05T12:00:00Z',
      updatedAt: '2026-10-05T12:00:00Z',
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: path,
      totalBytes: 5,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path, size: 5, sha256: 'ab', mediaType }],
    },
  };
}

interface MockOptions {
  /** Status of file reads (default 200). */
  fileStatus?: number;
  /** Status of the owner-agent lookup (default 404). */
  agentStatus?: number;
  /** Versions listed by GET .../versions (default: meta's version). */
  versions?: ArtifactVersion[];
  /** expiresAt of a minted view (default: far in the future). */
  viewExpiresAt?: string;
  /** Answers POST/PUT requests other than the view mint. */
  write?: (method: string, url: string) => Response;
  /** File body at the time of the read (overrides body). */
  bodyFn?: () => string;
}

const VIEW_URL = `/api/v1/artifacts/view/${ID}.1.9999999999.c2lnbmF0dXJl/`;

/** Mocks fetch: metadata answers meta (or 404 when null), file reads answer body. */
function mockFetch(
  meta: ArtifactResponse | null,
  body = '# Hello',
  opts: MockOptions = {}
): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      urls.push(method === 'GET' ? url : `${method} ${url}`);
      if (method !== 'GET' && !url.endsWith('/view')) {
        return Promise.resolve(
          opts.write ? opts.write(method, url) : new Response(null, { status: 500 })
        );
      }
      if (url.startsWith('/api/v1/agents/')) {
        const status = opts.agentStatus ?? 404;
        return Promise.resolve(
          new Response('{"error":{"code":"forbidden","message":"denied"}}', { status })
        );
      }
      if (url.endsWith('/view')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              url: VIEW_URL,
              expiresAt: opts.viewExpiresAt ?? '2999-01-01T00:00:00Z',
            }),
            {
              status: 200,
            }
          )
        );
      }
      if (/\/versions(\?|$)/.test(url)) {
        const versions = opts.versions ?? (meta?.version ? [{ ...meta.version, files: [] }] : []);
        return Promise.resolve(new Response(JSON.stringify({ versions }), { status: 200 }));
      }
      if (url.includes('/files/')) {
        const status = opts.fileStatus ?? 200;
        return Promise.resolve(
          status === 200
            ? new Response(opts.bodyFn ? opts.bodyFn() : body, { status })
            : new Response('{"error":{"code":"internal","message":"boom"}}', { status })
        );
      }
      if (meta === null) {
        return Promise.resolve(
          new Response('{"error":{"code":"not_found","message":"not found"}}', { status: 404 })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify(meta), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
  return urls;
}

async function mount(
  flag: boolean,
  path = `/projects/p-1/artifacts/${ID}`
): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': flag };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = { path } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
  return el;
}

describe('artifact page', () => {
  beforeAll(async () => {
    // Frames are checked by their attributes; nothing should load them.
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-detail.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetPrincipalNames();
  });

  it('shows nothing but a 404 when the experiment is off', async () => {
    const urls = mockFetch(artifact('a.md', 'text/markdown'));
    const el = await mount(false);
    expect(el.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(urls).toHaveLength(0);
  });

  it('renders markdown, fetching the bytes through the hub', async () => {
    const urls = mockFetch(artifact('design.md', 'text/markdown'), '# Hello');
    const el = await mount(true);
    const preview = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as
      | (HTMLElement & { content: string })
      | null;
    expect(preview).not.toBeNull();
    expect(preview!.content).toBe('# Hello');
    expect(urls).toContain(`/api/v1/artifacts/${ID}/versions/1/files/design.md?stream=1`);
    expect(el.shadowRoot!.querySelector('h1')!.textContent).toBe('Design');
  });

  it('renders text read-only in the code editor', async () => {
    mockFetch(artifact('notes.json', 'application/json'), '{"a":1}');
    const el = await mount(true);
    const editor = el.shadowRoot!.querySelector('scion-code-editor') as
      | (HTMLElement & { content: string; readonly: boolean; language: string })
      | null;
    expect(editor).not.toBeNull();
    expect(editor!.content).toBe('{"a":1}');
    expect(editor!.readonly).toBe(true);
  });

  it('renders images with <img> pointing at the file route', async () => {
    const urls = mockFetch(artifact('shot.png', 'image/png'));
    const el = await mount(true);
    const img = el.shadowRoot!.querySelector('img');
    expect(img?.getAttribute('src')).toBe(`/api/v1/artifacts/${ID}/versions/1/files/shot.png`);
    expect(urls.some((u) => u.includes('/files/'))).toBe(false);
  });

  it('renders HTML in a sandboxed frame under a view capability, marked as untrusted', async () => {
    const urls = mockFetch(artifact('page.html', 'text/html'));
    const el = await mount(true);
    const frame = el.shadowRoot!.querySelector('.untrusted iframe')!;
    expect(frame.getAttribute('sandbox')).toBe('allow-scripts');
    expect(frame.getAttribute('src')).toBe(VIEW_URL);
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer');
    expect(el.shadowRoot!.querySelector('.untrusted-bar')!.textContent).toContain('runs sandboxed');
    expect(urls).toContain(`POST /api/v1/artifacts/${ID}/versions/1/view`);
    expect(urls.some((u) => u.includes('/files/'))).toBe(false);
    const open = Array.from(el.shadowRoot!.querySelectorAll('.entry-bar sl-button')).find((b) =>
      b.textContent!.includes('Open in new tab')
    )!;
    expect(open.getAttribute('href')).toBe(VIEW_URL);
    expect(open.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('shows 404 when the hub answers 404', async () => {
    mockFetch(null);
    const el = await mount(true);
    expect(el.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
  });

  it('offers Open raw in a new tab for inline types, Download by base name otherwise', async () => {
    mockFetch(artifact('design.md', 'text/markdown'));
    let el = await mount(true);
    let btn = el.shadowRoot!.querySelector('.entry-bar sl-button')!;
    expect(btn.textContent).toContain('Open raw');
    expect(btn.getAttribute('target')).toBe('_blank');
    expect(btn.hasAttribute('download')).toBe(false);
    document.body.innerHTML = '';

    mockFetch(artifact('dir/report.pdf', 'application/pdf'));
    el = await mount(true);
    btn = el.shadowRoot!.querySelector('.entry-bar sl-button')!;
    expect(btn.textContent).toContain('Download');
    expect(btn.getAttribute('download')).toBe('report.pdf');
    expect(btn.hasAttribute('target')).toBe(false);
  });

  it('says a text entry over the inline limit is too large, without fetching it', async () => {
    const meta = artifact('huge.md', 'text/markdown');
    meta.version!.files[0].size = 5 * 1024 * 1024;
    const urls = mockFetch(meta);
    const el = await mount(true);
    expect(el.shadowRoot!.querySelector('.download-state')!.textContent).toContain('too large');
    expect(el.shadowRoot!.querySelector('.download-state')!.textContent).toContain('Use Open raw');
    expect(urls.some((u) => u.includes('/files/'))).toBe(false);
  });

  it('names the Download button for a large text entry that is not an inline type', async () => {
    const meta = artifact('bundle.js', 'text/javascript');
    meta.version!.files[0].size = 5 * 1024 * 1024;
    mockFetch(meta);
    const el = await mount(true);
    const text = el.shadowRoot!.querySelector('.download-state')!.textContent!;
    expect(text).toContain('too large');
    expect(text).toContain('Use Download');
    expect(el.shadowRoot!.querySelector('.entry-bar sl-button')!.textContent).toContain('Download');
  });

  it('shows the error state with Retry when the text fetch fails', async () => {
    mockFetch(artifact('design.md', 'text/markdown'), '', { fileStatus: 500 });
    const el = await mount(true);
    const err = el.shadowRoot!.querySelector('.error-state');
    expect(err).not.toBeNull();
    expect(err!.querySelector('sl-button')!.textContent).toContain('Retry');
  });

  it('does not raise an access-denied toast when the owner lookup is refused', async () => {
    const meta = artifact('design.md', 'text/markdown');
    meta.artifact.ownerKind = 'agent';
    meta.artifact.ownerRef = 'agent-1';
    const urls = mockFetch(meta, '# Hello', { agentStatus: 403 });
    const denied = vi.fn();
    window.addEventListener('scion:access-denied', denied);
    try {
      const el = await mount(true);
      expect(urls).toContain('/api/v1/agents/agent-1');
      expect(el.shadowRoot!.querySelector('scion-artifact-markdown-frame')).not.toBeNull();
      expect(denied).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('scion:access-denied', denied);
    }
  });

  it("links back to the artifact's own project, not the one in the URL", async () => {
    mockFetch(artifact('design.md', 'text/markdown'));
    const el = await mount(true); // URL project is p-1; scopeRef is p-1 too
    expect(el.shadowRoot!.querySelector('a.back-link')!.getAttribute('href')).toBe('/projects/p-1');
    document.body.innerHTML = '';

    const meta = artifact('design.md', 'text/markdown');
    meta.artifact.scopeRef = 'home-project';
    mockFetch(meta);
    const el2 = await mount(true);
    expect(el2.shadowRoot!.querySelector('a.back-link')!.getAttribute('href')).toBe(
      '/projects/home-project'
    );
  });

  it('renders markdown in a sandboxed frame that loads images from the version only', async () => {
    const meta = artifact('docs/design.md', 'text/markdown');
    meta.version!.files.push(
      { path: 'docs/img/b.png', size: 3, sha256: 'cd', mediaType: 'image/png' },
      {
        path: `_remote/${'a'.repeat(64)}`,
        size: 3,
        sha256: 'ef',
        mediaType: 'image/png',
        origin: 'remote',
        sourceUrl: 'https://img.example/chart.png',
        fetchStatus: 'ok',
      }
    );
    mockFetch(
      meta,
      [
        '![chart](https://img.example/chart.png)',
        '![elsewhere](https://elsewhere.example/p.png)',
        '![relative](img/b.png)',
      ].join('\n\n')
    );
    const el = await mount(true);
    const frameEl = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
      updateComplete: Promise<unknown>;
    };
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await frameEl.updateComplete;
    }
    const iframe = frameEl.shadowRoot!.querySelector('iframe')!;
    expect(iframe.getAttribute('sandbox')).not.toContain('allow-scripts');
    const doc = iframe.srcdoc;
    expect(doc).toContain(
      `content="default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'"`
    );
    expect(doc).toContain(
      `src="/api/v1/artifacts/${ID}/versions/1/files/_remote/${'a'.repeat(64)}?stream=1"`
    );
    expect(doc).toContain(`src="/api/v1/artifacts/${ID}/versions/1/files/docs/img/b.png?stream=1"`);
    expect(doc).toContain('Image not fetched: elsewhere');
    expect(doc).not.toContain('elsewhere.example');
  });

  it('shows the version in the URL, and offers Edit only on the current version', async () => {
    const meta = artifact('design.md', 'text/markdown');
    meta.artifact.currentSeq = 3;
    const urls = mockFetch(meta, '# Old');
    const el = await mount(true, `/projects/p-1/artifacts/${ID}/v/1`);
    expect(urls[0]).toBe(`/api/v1/artifacts/${ID}/versions/1`);
    const labels = Array.from(el.shadowRoot!.querySelectorAll('.actions sl-button')).map((b) =>
      b.textContent!.trim()
    );
    expect(labels.some((l) => l.includes('Version v1'))).toBe(true);
    expect(labels.some((l) => l.includes('Edit'))).toBe(false);
    document.body.innerHTML = '';

    mockFetch(artifact('design.md', 'text/markdown'), '# Now');
    const cur = await mount(true);
    const curLabels = Array.from(cur.shadowRoot!.querySelectorAll('.actions sl-button')).map((b) =>
      b.textContent!.trim()
    );
    expect(curLabels.some((l) => l.includes('Version v1 (current)'))).toBe(true);
    expect(curLabels.some((l) => l.includes('Edit'))).toBe(true);
  });

  it('lists the version files without the remote image rows', async () => {
    const meta = artifact('index.md', 'text/markdown');
    meta.version!.files.push(
      { path: 'img/a.png', size: 2048, sha256: 'cd', mediaType: 'image/png' },
      {
        path: `_remote/${'a'.repeat(64)}`,
        size: 3,
        sha256: 'ef',
        mediaType: 'image/png',
        origin: 'remote',
        sourceUrl: 'https://img.example/x.png',
        fetchStatus: 'failed',
      }
    );
    mockFetch(meta);
    const el = await mount(true);
    const rows = Array.from(el.shadowRoot!.querySelectorAll('sl-tab-panel[name="files"] tbody tr'));
    expect(
      rows.map((r) => r.querySelector('.file-path')!.textContent!.replace(/\s+/g, ' ').trim())
    ).toEqual(['index.md entry', 'img/a.png']);
    const summary = el.shadowRoot!.querySelector(
      'sl-tab-panel[name="files"] .summary'
    )!.textContent!;
    expect(summary).toContain('could not be fetched: 1');
  });

  it('lists versions in History and offers a new version when there is only one', async () => {
    mockFetch(artifact('design.md', 'text/markdown'));
    const el = await mount(true);
    const panel = el.shadowRoot!.querySelector('sl-tab-panel[name="history"]')!;
    expect(panel.querySelectorAll('tbody tr')).toHaveLength(1);
    const single = panel.querySelector('.single-version')!;
    expect(single.textContent).toContain('Only one version so far.');
    expect(single.textContent).toContain('Upload new version');
    expect(single.textContent).not.toContain('scion artifact');
  });

  it('shows the new version in place after publishing an edit from the current URL', async () => {
    window.history.replaceState({}, '', `/projects/p-1/artifacts/${ID}`);
    const meta = artifact('design.md', 'text/markdown');
    const urls = mockFetch(meta, '# Old', {
      write: (method, url) => {
        if (url.endsWith('/finalize')) {
          meta.artifact.currentSeq = 2;
          meta.version = { ...meta.version!, seq: 2, ref: `scion://artifact/${ID}@2` };
          return new Response(JSON.stringify(meta), { status: 200 });
        }
        if (method === 'PUT') return new Response(null, { status: 204 });
        return new Response(
          JSON.stringify({
            artifact: meta.artifact,
            version: { seq: 2 },
            upload: { required: ['design.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const el = await mount(true);
    const editBtn = Array.from(el.shadowRoot!.querySelectorAll('.actions sl-button')).find((b) =>
      b.textContent!.includes('Edit')
    ) as HTMLElement;
    editBtn.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: '# New' } })
    );
    await el.updateComplete;
    const publish = Array.from(el.shadowRoot!.querySelectorAll('.edit-footer sl-button')).find(
      (b) => b.textContent!.includes('Publish new version')
    ) as HTMLElement;
    publish.click();
    for (let i = 0; i < 30; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(urls.filter((u) => u === `/api/v1/artifacts/${ID}`)).toHaveLength(2);
    expect(
      el.shadowRoot!.querySelector('scion-code-editor[readonly]') ??
        el.shadowRoot!.querySelector('scion-artifact-markdown-frame')
    ).not.toBeNull();
    const labels = Array.from(el.shadowRoot!.querySelectorAll('.actions sl-button')).map((b) =>
      b.textContent!.trim()
    );
    expect(labels.some((l) => l.includes('Version v2 (current)'))).toBe(true);
  });

  it('resumes the pending version when an edit is published again after a failed upload', async () => {
    window.history.replaceState({}, '', `/projects/p-1/artifacts/${ID}`);
    const meta = artifact('design.md', 'text/markdown');
    let putStatus = 500;
    const urls = mockFetch(meta, '# Old', {
      write: (method, url) => {
        if (url.endsWith('/finalize')) return new Response(JSON.stringify(meta), { status: 200 });
        if (method === 'PUT') {
          return putStatus === 204
            ? new Response(null, { status: 204 })
            : new Response('{"error":{"code":"internal","message":"boom"}}', { status: putStatus });
        }
        return new Response(
          JSON.stringify({
            artifact: meta.artifact,
            version: { seq: 2 },
            upload: { required: ['design.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const el = await mount(true);
    (
      Array.from(el.shadowRoot!.querySelectorAll('.actions sl-button')).find((b) =>
        b.textContent!.includes('Edit')
      ) as HTMLElement
    ).click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: '# New' } })
    );
    await el.updateComplete;
    const clickPublish = async (): Promise<void> => {
      (
        Array.from(el.shadowRoot!.querySelectorAll('.edit-footer sl-button')).find((b) =>
          b.textContent!.includes('Publish new version')
        ) as HTMLElement
      ).click();
      for (let i = 0; i < 30; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
    };
    await clickPublish();
    expect(el.shadowRoot!.querySelector('sl-alert[variant="danger"]')!.textContent).toContain(
      'Publish again to retry'
    );
    putStatus = 204;
    await clickPublish();
    expect(urls.filter((u) => u === `POST /api/v1/artifacts/${ID}/versions`)).toHaveLength(1);
    expect(urls.filter((u) => u.startsWith('PUT '))).toHaveLength(2);
    expect(urls).toContain(`POST /api/v1/artifacts/${ID}/versions/2/finalize`);
  });

  it('says when the HTML view has expired and mints a new one on Reload view', async () => {
    const urls = mockFetch(artifact('page.html', 'text/html'), '', {
      viewExpiresAt: '2000-01-01T00:00:00Z',
    });
    const el = await mount(true);
    const alert = el.shadowRoot!.querySelector('sl-alert.expired')!;
    expect(alert.textContent).toContain('This view has expired.');
    expect(
      Array.from(el.shadowRoot!.querySelectorAll('.entry-bar sl-button')).some((b) =>
        b.textContent!.includes('Open in new tab')
      )
    ).toBe(false);
    (alert.querySelector('sl-button') as HTMLElement).click();
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
    expect(urls.filter((u) => u === `POST /api/v1/artifacts/${ID}/versions/1/view`)).toHaveLength(
      2
    );
  });

  it('gives the markdown frame no referrer and single-file versions a download in History', async () => {
    mockFetch(artifact('design.md', 'text/markdown'));
    const el = await mount(true);
    const frameEl = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
      updateComplete: Promise<unknown>;
    };
    await frameEl.updateComplete;
    expect(frameEl.shadowRoot!.querySelector('iframe')!.getAttribute('referrerpolicy')).toBe(
      'no-referrer'
    );
    const dl = el.shadowRoot!.querySelector(
      'sl-tab-panel[name="history"] sl-icon-button[name="download"]'
    )!;
    expect(dl.getAttribute('href')).toBe(`/api/v1/artifacts/${ID}/versions/1/files/design.md`);
    expect(dl.getAttribute('download')).toBe('design.md');
  });

  it('shows the owner and publishers by name', async () => {
    const meta = artifact('page.html', 'text/html');
    meta.artifact.ownerKind = 'agent';
    meta.artifact.ownerRef = 'agent-1';
    meta.version!.createdByKind = 'user';
    meta.version!.createdByRef = 'u-9';
    vi.stubGlobal('fetch', vi.fn());
    mockFetch(meta);
    const inner = globalThis.fetch as unknown as (
      i: RequestInfo | URL,
      n?: RequestInit
    ) => Promise<Response>;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === '/api/v1/agents/agent-1') {
          return Promise.resolve(
            new Response(JSON.stringify({ name: 'metrics-agent' }), { status: 200 })
          );
        }
        if (url === '/api/v1/users/u-9') {
          return Promise.resolve(
            new Response(JSON.stringify({ displayName: 'Jane Doe' }), { status: 200 })
          );
        }
        return inner(input, init);
      })
    );
    const el = await mount(true);
    expect(el.shadowRoot!.querySelector('.meta')!.textContent).toContain(
      'Owner: metrics-agent (agent)'
    );
    expect(el.shadowRoot!.querySelector('.untrusted-bar')!.textContent).toContain(
      'Content published by metrics-agent (agent)'
    );
    const history = el.shadowRoot!.querySelector('sl-tab-panel[name="history"] tbody tr')!;
    expect(history.textContent).toContain('Jane Doe');
  });

  it('forgets the HTML view timer when the page reloads', async () => {
    const soon = new Date(Date.now() + 60_000).toISOString();
    mockFetch(artifact('page.html', 'text/html'), '', { viewExpiresAt: soon });
    const el = await mount(true);
    const priv = el as unknown as {
      viewTimer: ReturnType<typeof setTimeout> | null;
      viewExpired: boolean;
      load(): Promise<void>;
    };
    expect(priv.viewTimer).not.toBeNull();
    mockFetch(artifact('design.md', 'text/markdown'));
    await priv.load();
    expect(priv.viewTimer).toBeNull();
    expect(priv.viewExpired).toBe(false);
  });

  it('keeps the newer result when an earlier load answers last', async () => {
    mockFetch(artifact('a.png', 'image/png'));
    const el = await mount(true);
    const priv = el as unknown as { load(): Promise<void>; loading: boolean };
    const meta = (title: string): ArtifactResponse => {
      const m = artifact('a.png', 'image/png');
      m.artifact.title = title;
      return m;
    };
    // Each GET of the artifact waits until the test releases it.
    const release: ((m: ArtifactResponse) => void)[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input) === `/api/v1/artifacts/${ID}`) {
          return new Promise<Response>((resolve) => {
            release.push((m) => resolve(new Response(JSON.stringify(m), { status: 200 })));
          });
        }
        return Promise.resolve(new Response(JSON.stringify({ versions: [] }), { status: 200 }));
      })
    );
    const settle = async (): Promise<void> => {
      for (let i = 0; i < 10; i++) {
        await new Promise((r) => setTimeout(r, 0));
        await el.updateComplete;
      }
    };

    // The earlier load answers first, while the newer one is still waiting:
    // it must not end the loading state or show its data.
    const olderFirst = priv.load();
    const newerSecond = priv.load();
    await settle();
    release[0](meta('Older'));
    await olderFirst;
    await settle();
    expect(priv.loading).toBe(true);
    release[1](meta('Newer'));
    await newerSecond;
    await settle();
    expect(priv.loading).toBe(false);
    expect(el.shadowRoot!.querySelector('h1')!.textContent).toBe('Newer');

    // The earlier load answers last: the newer result and state stay.
    const olderLast = priv.load();
    const newerFirst = priv.load();
    await settle();
    release[3](meta('Newest'));
    await newerFirst;
    await settle();
    expect(priv.loading).toBe(false);
    expect(el.shadowRoot!.querySelector('h1')!.textContent).toBe('Newest');
    release[2](meta('Stale'));
    await olderLast;
    await settle();
    expect(priv.loading).toBe(false);
    expect(el.shadowRoot!.querySelector('h1')!.textContent).toBe('Newest');
  });

  it('keeps the newer view when an earlier mint answers last', async () => {
    mockFetch(artifact('page.html', 'text/html'));
    const el = await mount(true);
    const priv = el as unknown as {
      loadView(seq: number): Promise<void>;
      view: { url: string } | null;
    };
    let releaseOlder: (() => void) | null = null;
    let mints = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        mints++;
        const body = (url: string): Response =>
          new Response(JSON.stringify({ url, expiresAt: '2999-01-01T00:00:00Z' }), { status: 200 });
        if (mints === 1) {
          return new Promise<Response>((resolve) => {
            releaseOlder = (): void => resolve(body('/older/'));
          });
        }
        return Promise.resolve(body('/newer/'));
      })
    );
    const first = priv.loadView(1);
    await priv.loadView(1);
    releaseOlder!();
    await first;
    expect(priv.view!.url).toBe('/newer/');
  });
  /** A review version (current) of a markdown artifact owned by an agent. */
  function reviewMeta(): ArtifactResponse {
    const meta = artifact('plan.md', 'text/markdown');
    meta.artifact.ownerKind = 'agent';
    meta.artifact.ownerRef = 'agent-1';
    meta.artifact.currentSeq = 2;
    meta.version = {
      ...meta.version!,
      seq: 2,
      ref: `scion://artifact/${ID}@2`,
      kind: 'review',
      createdByKind: 'user',
      createdByRef: 'u-2',
    };
    return meta;
  }
  const MARKED = 'We {~~ship~>launch~~} in Q3.{>>date?<<}\n';

  function buttons(el: ScionPageArtifactDetail, sel: string): HTMLElement[] {
    return Array.from(el.shadowRoot!.querySelectorAll(sel));
  }
  function button(el: ScionPageArtifactDetail, sel: string, text: string): HTMLElement | undefined {
    return buttons(el, sel).find((b) => b.textContent!.includes(text));
  }
  async function settle(el: ScionPageArtifactDetail, n = 30): Promise<void> {
    for (let i = 0; i < n; i++) {
      await new Promise((r) => setTimeout(r, 0));
      await el.updateComplete;
    }
  }

  it('shows a current review as pending, with its marks and a Marks / Clean / Accepted switch', async () => {
    const meta = reviewMeta();
    mockFetch(meta, MARKED, {
      versions: [
        { ...meta.version!, files: [] },
        { ...meta.version!, seq: 1, kind: 'publish', files: [] },
      ],
    });
    const el = await mount(true);
    const badge = el.shadowRoot!.querySelector('.title sl-badge');
    expect(badge?.textContent).toContain('Review pending');
    const banner = el.shadowRoot!.querySelector('sl-alert.review-banner');
    expect(banner?.textContent).toContain('1 comment and 1 suggestion on v1');
    expect(banner?.textContent).toContain('was notified');
    const frame = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
      critic: string;
    };
    expect(frame.critic).toBe('marks');
    const toggle = el.shadowRoot!.querySelector('sl-radio-group.critic-toggle') as HTMLElement & {
      value: string;
    };
    expect(toggle).not.toBeNull();
    toggle.value = 'clean';
    toggle.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(frame.critic).toBe('clean');
  });

  it('shows no review state for a published version and offers Review for markdown only', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    expect(el.shadowRoot!.querySelector('.title sl-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-alert.review-banner')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-radio-group.critic-toggle')).toBeNull();
    const frame = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
      critic: string;
    };
    expect(frame.critic).toBe('off');
    expect(button(el, '.actions sl-button', 'Review')).toBeDefined();
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    mockFetch(artifact('notes.txt', 'text/plain'), 'hello');
    const txt = await mount(true);
    expect(button(txt, '.actions sl-button', 'Edit')).toBeDefined();
    expect(button(txt, '.actions sl-button', 'Review')).toBeUndefined();
  });

  it('saves a review of marks only as a version of kind review', async () => {
    window.history.replaceState({}, '', `/projects/p-1/artifacts/${ID}`);
    const meta = artifact('plan.md', 'text/markdown');
    const bodies: string[] = [];
    const finalizeBodies: string[] = [];
    const urls = mockFetch(meta, 'We ship in Q3.\n', {
      write: (method, url) => {
        if (url.endsWith('/finalize')) {
          meta.artifact.currentSeq = 2;
          meta.version = {
            ...meta.version!,
            seq: 2,
            kind: 'review',
            ref: `scion://artifact/${ID}@2`,
          };
          return new Response(JSON.stringify(meta), { status: 200 });
        }
        if (method === 'PUT') return new Response(null, { status: 204 });
        return new Response(
          JSON.stringify({
            artifact: meta.artifact,
            version: { seq: 2 },
            upload: { required: ['plan.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const realFetch = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if (init?.method === 'POST' && String(input).endsWith('/versions'))
          bodies.push(String(init.body));
        if (init?.method === 'POST' && String(input).endsWith('/finalize'))
          finalizeBodies.push(String(init.body));
        return realFetch(input, init);
      })
    );
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    // While reviewing, Edit and Review are hidden.
    expect(button(el, '.actions sl-button', 'Edit')).toBeUndefined();
    const save = (): HTMLElement & { disabled: boolean } =>
      button(el, '.edit-footer sl-button', 'Save review') as HTMLElement & { disabled: boolean };
    expect(save().hasAttribute('disabled')).toBe(true);
    // Empty state: the hint replaces the counts and the preview margin
    // shows the placeholder.
    expect(el.shadowRoot!.querySelector('.review-toolbar .counts')!.textContent).toContain(
      'Select text and choose Comment or Suggest.'
    );
    const reviewFrame = el.shadowRoot!.querySelector(
      '.review-panes scion-artifact-markdown-frame'
    ) as HTMLElement & { sideNote: string; noteAuthor: string; sideNoteEmpty: boolean };
    expect(reviewFrame.sideNote).toBe('No comments or suggestions yet.');
    expect(reviewFrame.sideNoteEmpty).toBe(true);
    expect(reviewFrame.noteAuthor).toBe('You');
    const editor = el.shadowRoot!.querySelector('scion-code-editor.review-editor')!;
    // An edit outside marks is flagged and cannot be saved.
    editor.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: 'We ship in Q4.{>>x<<}\n' } })
    );
    await el.updateComplete;
    expect(save().hasAttribute('disabled')).toBe(true);
    expect(
      buttons(el, 'sl-alert').some((a) => a.textContent!.includes('Text outside marks has changed'))
    ).toBe(true);
    editor.dispatchEvent(new CustomEvent('content-changed', { detail: { content: MARKED } }));
    await el.updateComplete;
    expect(save().hasAttribute('disabled')).toBe(false);
    save().click();
    await settle(el);
    expect(bodies).toHaveLength(1);
    expect(JSON.parse(bodies[0]!).kind).toBe('review');
    // The review names the version it was made against.
    expect(finalizeBodies.map((b) => JSON.parse(b) as { base: number })).toEqual([{ base: 1 }]);
    expect(urls.some((u) => u.startsWith('PUT ') && u.includes('/versions/2/files/plan.md'))).toBe(
      true
    );
    expect(el.shadowRoot!.querySelector('.title sl-badge')?.textContent).toContain(
      'Review pending'
    );
  });

  it('explains a review the hub refused for unmarked changes', async () => {
    const meta = artifact('plan.md', 'text/markdown');
    mockFetch(meta, 'We ship in Q3.\n', {
      write: (method, url) => {
        if (url.endsWith('/finalize')) {
          return new Response(
            JSON.stringify({
              error: {
                code: 'unmarked_changes',
                message: 'x',
                details: { files: [{ path: 'plan.md', change: 'modified', hunks: [{ line: 1 }] }] },
              },
            }),
            { status: 422 }
          );
        }
        if (method === 'PUT') return new Response(null, { status: 204 });
        return new Response(
          JSON.stringify({
            artifact: meta.artifact,
            version: { seq: 2 },
            upload: { required: ['plan.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    await el.updateComplete;
    button(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    const alert = buttons(el, 'sl-alert').find((a) =>
      a.textContent!.includes('outside marks: plan.md (line 1)')
    );
    expect(alert).toBeDefined();
    expect(alert!.textContent).not.toContain('Publish again');
    expect(alert!.textContent).not.toContain('Edit');
  });
  /** Mocks a review flow whose finalize answers with status and body. */
  function reviewWrites(meta: ArtifactResponse, finalize: () => Response, creates: string[]) {
    return (method: string, url: string): Response => {
      if (url.endsWith('/finalize')) return finalize();
      if (method === 'PUT') return new Response(null, { status: 204 });
      creates.push(url);
      return new Response(
        JSON.stringify({
          artifact: meta.artifact,
          version: { seq: 2 + creates.length - 1 },
          upload: { required: ['plan.md'] },
        }),
        { status: 201 }
      );
    };
  }

  it('restarts a stale review from the current text and keeps the discarded text read-only', async () => {
    const meta = artifact('plan.md', 'text/markdown');
    const creates: string[] = [];
    let text = 'We ship in Q3.\n';
    // Meanwhile another reviewer's review (v2) became current.
    const othersReview = 'We ship in Q3.{>>someone else<<}\n';
    const urls = mockFetch(meta, '', {
      bodyFn: () => text,
      write: reviewWrites(
        meta,
        () => {
          meta.artifact.currentSeq = 2;
          meta.version = {
            ...meta.version!,
            seq: 2,
            kind: 'review',
            ref: `scion://artifact/${ID}@2`,
          };
          text = othersReview;
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            {
              status: 409,
            }
          );
        },
        creates
      ),
    });
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const editor = (): HTMLElement & { content: string } =>
      el.shadowRoot!.querySelector('scion-code-editor.review-editor') as HTMLElement & {
        content: string;
      };
    editor().dispatchEvent(new CustomEvent('content-changed', { detail: { content: MARKED } }));
    await el.updateComplete;
    const metaLoads = (): number => urls.filter((u) => u === `/api/v1/artifacts/${ID}`).length;
    const before = metaLoads();
    button(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    const alert = buttons(el, 'sl-alert').find((a) => a.textContent!.includes('was not saved'));
    expect(alert).toBeDefined();
    expect(alert!.textContent).toContain('newer version');
    expect(alert!.textContent).not.toContain('Edit');
    expect(metaLoads()).toBe(before + 1);
    // The buffer is the new current text, with the other reviewer's marks;
    // the discarded text is shown read-only.
    expect(editor().content).toBe(othersReview);
    expect(el.shadowRoot!.querySelector('.discarded-review pre')!.textContent).toBe(MARKED);
    const save = (): HTMLElement => button(el, '.edit-footer sl-button', 'Save review')!;
    expect(save().hasAttribute('disabled')).toBe(true);
    // New marks on the current text can be saved, as a new version.
    editor().dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: 'We {~~ship~>launch~~} in Q3.{>>someone else<<}\n' },
      })
    );
    await el.updateComplete;
    expect(save().hasAttribute('disabled')).toBe(false);
    save().click();
    await settle(el);
    expect(creates).toHaveLength(2);
  });

  it('leaves Review mode with a notice when the current version cannot be reviewed after a stale review', async () => {
    const meta = artifact('plan.md', 'text/markdown');
    const creates: string[] = [];
    mockFetch(meta, 'We ship in Q3.\n', {
      write: reviewWrites(
        meta,
        () => {
          meta.artifact.currentSeq = 2;
          meta.version = {
            ...meta.version!,
            seq: 2,
            entryPath: 'plan.txt',
            files: [{ path: 'plan.txt', size: 5, sha256: 'cd', mediaType: 'text/plain' }],
          };
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            {
              status: 409,
            }
          );
        },
        creates
      ),
    });
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    await el.updateComplete;
    button(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    expect(el.shadowRoot!.querySelector('.review-editor')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-alert.review-notice')!.textContent).toContain(
      'cannot be reviewed here'
    );
    // The discarded text stays readable, with Copy, outside Review mode.
    expect(el.shadowRoot!.querySelector('.discarded-review pre')!.textContent).toBe(MARKED);
    expect(button(el, '.discarded-review sl-button', 'Copy')).toBeDefined();
  });

  it('keeps the discarded text and says so when the reload after a stale review fails', async () => {
    const meta = artifact('plan.md', 'text/markdown');
    const creates: string[] = [];
    let fileStatus = 200;
    const urls = mockFetch(meta, 'We ship in Q3.\n', {
      write: reviewWrites(
        meta,
        () => {
          fileStatus = 500;
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            {
              status: 409,
            }
          );
        },
        creates
      ),
    });
    const inner = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
      fileStatus !== 200 && String(input).includes('/files/')
        ? Promise.resolve(
            new Response('{"error":{"code":"internal","message":"boom"}}', { status: 500 })
          )
        : inner(input, init)
    );
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    await el.updateComplete;
    button(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    expect(urls.length).toBeGreaterThan(0);
    expect(el.shadowRoot!.querySelector('.review-editor')).toBeNull();
    const notice = el.shadowRoot!.querySelector('sl-alert.review-notice')!;
    expect(notice.textContent).toContain('could not be loaded');
    expect(notice.textContent).not.toContain('newer version was published');
    expect(el.shadowRoot!.querySelector('.discarded-review pre')!.textContent).toBe(MARKED);
    expect(el.shadowRoot!.querySelector('.error-state')).not.toBeNull();
  });

  it('shows the margin placeholder only while the review has no marks at all', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const frame = el.shadowRoot!.querySelector(
      '.review-panes scion-artifact-markdown-frame'
    ) as HTMLElement & { sideNote: string };
    expect(frame.sideNote).toBe('No comments or suggestions yet.');
    // A suggestion without any comment: no placeholder.
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: 'We {~~ship~>launch~~} in Q3.\n' } })
    );
    await new Promise((r) => setTimeout(r, 300));
    await el.updateComplete;
    expect(frame.sideNote).toBe('');
  });

  it('clears the pending preview update on Cancel', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    const cleared = vi.spyOn(globalThis, 'clearTimeout');
    button(el, '.edit-footer sl-button', 'Cancel')!.click();
    await el.updateComplete;
    expect(cleared).toHaveBeenCalled();
    cleared.mockRestore();
    expect(el.shadowRoot!.querySelector('.review-editor')).toBeNull();
  });

  it('shows Review pending while the current version is a review, also on an older version', async () => {
    const meta = reviewMeta();
    const old = {
      ...meta,
      version: { ...meta.version!, seq: 1, kind: 'publish', ref: `scion://artifact/${ID}@1` },
    };
    mockFetch(old, 'We ship in Q3.\n', {
      versions: [
        { ...meta.version!, files: [] },
        { ...meta.version!, seq: 1, kind: 'publish', files: [] },
      ],
    });
    const el = await mount(true, `/projects/p-1/artifacts/${ID}/v/1`);
    expect(el.shadowRoot!.querySelector('.title sl-badge')?.textContent).toContain(
      'Review pending'
    );
  });

  it('does not claim the owner was notified of its own review', async () => {
    const meta = reviewMeta();
    meta.version!.createdByKind = 'agent';
    meta.version!.createdByRef = 'agent-1';
    mockFetch(meta, MARKED);
    const el = await mount(true);
    const banner = el.shadowRoot!.querySelector('sl-alert.review-banner')!;
    expect(banner.textContent).toContain('Review pending');
    expect(banner.textContent).not.toContain('was notified');
  });

  it('labels a review with its base, its notes with the reviewer, and Clean with a side note', async () => {
    const meta = reviewMeta();
    mockFetch(meta, MARKED, {
      versions: [
        { ...meta.version!, files: [] },
        { ...meta.version!, seq: 1, kind: 'publish', files: [] },
      ],
    });
    const el = await mount(true);
    expect(el.shadowRoot!.querySelector('.entry-bar')!.textContent).toContain('review of v1');
    const frame = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
      noteAuthor: string;
      sideNote: string;
    };
    expect(frame.noteAuthor).not.toBe('');
    expect(frame.sideNote).toBe('');
    const toggle = el.shadowRoot!.querySelector('sl-radio-group.critic-toggle') as HTMLElement & {
      value: string;
    };
    toggle.value = 'clean';
    toggle.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(frame.sideNote).toBe(
      'Clean: all marks rejected. Matches the version under review (v1).'
    );
  });

  it('shows the kind of each version in History, with the review note', async () => {
    const meta = reviewMeta();
    mockFetch(meta, MARKED, {
      versions: [
        { ...meta.version!, files: [] },
        { ...meta.version!, seq: 1, kind: 'publish', files: [] },
      ],
    });
    const el = await mount(true);
    const heads = buttons(el, 'th').map((th) => th.textContent!.trim());
    expect(heads).toContain('Kind');
    const kinds = buttons(el, 'sl-badge.kind-review, sl-badge.kind-publish').map(
      (b) => b.className
    );
    expect(kinds).toEqual(['kind-review', 'kind-publish']);
    expect(
      el.shadowRoot!.querySelector('.history-note')!.textContent!.replace(/\s+/g, ' ')
    ).toContain('Publishing after a review resolves it and clears the badge.');
  });

  it('lays notes out in a margin from 1100px on the page and from 1400px in the Review pane', async () => {
    for (const width of [1000, 1200, 1500]) {
      vi.stubGlobal('matchMedia', (q: string) => ({
        matches: Number(/min-width: (\d+)px/.exec(q)?.[1] ?? Infinity) <= width,
        addEventListener: (): void => {},
        removeEventListener: (): void => {},
      }));
      mockFetch(reviewMeta(), MARKED);
      const el = await mount(true);
      const page = el.shadowRoot!.querySelector('scion-artifact-markdown-frame') as HTMLElement & {
        marginNotes: boolean;
      };
      expect(page.marginNotes, `page at ${width}`).toBe(width >= 1100);
      button(el, '.actions sl-button', 'Review')!.click();
      await el.updateComplete;
      const pane = el.shadowRoot!.querySelector(
        '.review-panes scion-artifact-markdown-frame'
      ) as HTMLElement & { marginNotes: boolean };
      expect(pane.marginNotes, `review pane at ${width}`).toBe(width >= 1400);
      document.body.innerHTML = '';
      vi.unstubAllGlobals();
    }
  });

  it('updates the Review preview after typing pauses, the counts at once', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    await el.updateComplete;
    const frame = el.shadowRoot!.querySelector(
      '.review-panes scion-artifact-markdown-frame'
    ) as HTMLElement & { content: string };
    expect(
      el.shadowRoot!.querySelector('.review-toolbar .counts')!.textContent!.replace(/\s+/g, ' ')
    ).toContain('1 suggestion · 1 comment');
    expect(frame.content).toBe('We ship in Q3.\n');
    await new Promise((r) => setTimeout(r, 300));
    await el.updateComplete;
    expect(frame.content).toBe(MARKED);
  });

  it('applies toolbar marks to plain text and refuses a selection inside a mark', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const editor = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & {
      getSelection: () => { from: number; to: number; text: string } | null;
      replaceRange: (...args: unknown[]) => void;
    };
    const replaced: unknown[][] = [];
    editor.replaceRange = (...args: unknown[]): void => {
      replaced.push(args);
    };
    editor.getSelection = (): { from: number; to: number; text: string } => ({
      from: 3,
      to: 7,
      text: 'ship',
    });
    button(el, '.review-toolbar sl-button', 'Comment')!.click();
    await el.updateComplete;
    expect(replaced).toEqual([[3, 7, '{==ship==}{>>comment<<}', 16, 23]]);
    // Inside an existing mark: refused with a hint, nothing inserted.
    editor.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: 'We {--ship--} in Q3.\n' } })
    );
    await el.updateComplete;
    editor.getSelection = (): { from: number; to: number; text: string } => ({
      from: 6,
      to: 10,
      text: 'ship',
    });
    button(el, '.review-toolbar sl-button', 'Suggest')!.click();
    await el.updateComplete;
    expect(replaced).toHaveLength(1);
    expect(el.shadowRoot!.querySelector('.review-toolbar .hint')!.textContent).toContain(
      'inside or across a mark'
    );
  });

  /** The Review editor with its selection and replace stubbed. */
  function stubEditor(el: ScionPageArtifactDetail): {
    editor: HTMLElement & { content: string };
    replaced: unknown[][];
    select: (from: number, to: number) => void;
  } {
    const editor = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & {
      content: string;
      getSelection: () => { from: number; to: number; text: string };
      replaceRange: (...args: unknown[]) => void;
      getContent: () => string;
    };
    const replaced: unknown[][] = [];
    editor.replaceRange = (...args: unknown[]): void => {
      replaced.push(args);
    };
    const select = (from: number, to: number): void => {
      editor.getSelection = () => ({ from, to, text: editor.getContent().slice(from, to) });
    };
    return { editor, replaced, select };
  }

  it('reviews a CRLF file in LF form, so toolbar offsets match the editor', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship.\r\nLine two Q3.\r\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const { editor, replaced, select } = stubEditor(el);
    expect(editor.content).toBe('We ship.\nLine two Q3.\n');
    expect(button(el, '.edit-footer sl-button', 'Save review')!.hasAttribute('disabled')).toBe(
      true
    );
    const at = editor.content.indexOf('Q3');
    select(at, at + 2);
    button(el, '.review-toolbar sl-button', 'Comment')!.click();
    await el.updateComplete;
    expect(replaced).toHaveLength(1);
  });

  it('refuses a mark an opener would swallow in a CRLF file', async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), '{\r\n{\r\n{++-');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const { editor, replaced, select } = stubEditor(el);
    const at = editor.content.indexOf('++');
    select(at, at + 2);
    button(el, '.review-toolbar sl-button', 'Insert')!.click();
    await el.updateComplete;
    expect(replaced).toHaveLength(0);
    expect(el.shadowRoot!.querySelector('.review-toolbar .hint')!.textContent).toContain(
      'unclosed'
    );
  });

  it("checks toolbar actions against the editor's content", async () => {
    mockFetch(artifact('plan.md', 'text/markdown'), 'We ship in Q3.\n');
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    const { editor, replaced, select } = stubEditor(el);
    // The editor holds text whose change event has not reached the page.
    editor.content = '{\n{\n{++-';
    select(5, 7);
    button(el, '.review-toolbar sl-button', 'Insert')!.click();
    await el.updateComplete;
    expect(replaced).toHaveLength(0);
  });

  it('keeps the discarded text and Copy above the not-found page when the reload after a stale review answers 404', async () => {
    const meta = artifact('plan.md', 'text/markdown');
    const creates: string[] = [];
    let gone = false;
    mockFetch(meta, 'We ship in Q3.\n', {
      write: reviewWrites(
        meta,
        () => {
          gone = true;
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            {
              status: 409,
            }
          );
        },
        creates
      ),
    });
    const inner = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
      gone && String(input) === `/api/v1/artifacts/${ID}` && (init?.method ?? 'GET') === 'GET'
        ? Promise.resolve(
            new Response('{"error":{"code":"not_found","message":"not found"}}', { status: 404 })
          )
        : inner(input, init)
    );
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED } })
    );
    await el.updateComplete;
    button(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    expect(el.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('sl-alert.review-notice')!.textContent).toContain(
      'could not be loaded'
    );
    expect(el.shadowRoot!.querySelector('.discarded-review pre')!.textContent).toBe(MARKED);
    expect(button(el, '.discarded-review sl-button', 'Copy')).toBeDefined();
  });

  it('does not offer to save an unchanged review of a CRLF review version', async () => {
    mockFetch(reviewMeta(), MARKED.replace(/\n/g, '\r\n'));
    const el = await mount(true);
    button(el, '.actions sl-button', 'Review')!.click();
    await el.updateComplete;
    // The marks are there and only marks differ from the baseline, but
    // nothing was changed: Save stays disabled.
    expect(button(el, '.edit-footer sl-button', 'Save review')!.hasAttribute('disabled')).toBe(
      true
    );
  });
});
