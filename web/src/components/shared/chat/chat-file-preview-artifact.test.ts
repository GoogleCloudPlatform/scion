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
 * Tests for the artifact target of <scion-chat-file-preview>, the in-place
 * preview opened from artifact chips and links (D23, ptone/scion#3224).
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
  extractApiError: (_res: Response, fallback: string) => Promise.resolve(fallback),
}));

await import('./chat-file-preview.js');
type ScionChatFilePreview = import('./chat-file-preview.js').ScionChatFilePreview;
const { ARTIFACT_UNAVAILABLE_MESSAGE } = await import('./chat-file-preview.js');
const { resetPrincipalNames } = await import('../../../client/principal-names.js');

/** The artifact requests made, without the owner and project name lookups. */
function artifactCalls(): string[] {
  return apiFetchMock.mock.calls.map((c) => c[0] as string).filter((p) => p.startsWith('/api/v1/artifacts/'));
}

const { ScionArtifactMarkdownFrame: ScionArtifactMarkdownFrameCtor } = await import(
  '../artifact-markdown-frame.js'
);
type ScionArtifactMarkdownFrame = import('../artifact-markdown-frame.js').ScionArtifactMarkdownFrame;

const ID = '5f1c2d3e-0000-4000-8000-0000000000aa';

function meta(seq: number, mediaType = 'text/markdown', size = 12) {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeRef: 'proj-1',
      ownerKind: 'agent',
      ownerRef: 'agent-1',
      title: 'Design notes',
      currentSeq: 3,
    },
    version: {
      seq,
      entryPath: 'design.md',
      state: 'ready',
      files: [{ path: 'design.md', size, sha256: 'x', mediaType }],
    },
  };
}

function json(body: unknown, status = 200) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) };
}

function text(body: string) {
  return { ok: true, status: 200, text: () => Promise.resolve(body) };
}

async function open(seq = 0): Promise<ScionChatFilePreview> {
  const el = document.createElement('scion-chat-file-preview') as ScionChatFilePreview;
  document.body.appendChild(el);
  el.target = { kind: 'artifact', id: ID, seq, name: 'Artifact' };
  for (let i = 0; i < 8; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
  return el;
}

function q<T extends Element = Element>(el: ScionChatFilePreview, sel: string): T | null {
  return el.shadowRoot?.querySelector<T>(sel) ?? null;
}

function buttons(el: ScionChatFilePreview): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.footer sl-button') ?? []).map((b) =>
    (b.textContent ?? '').replace(/\s+/g, ' ').trim()
  );
}

describe('scion-chat-file-preview artifact target', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    apiFetchMock.mockReset();
    resetPrincipalNames();
  });
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('loads the current version and renders Markdown in the sandboxed artifact frame, with the artifact actions', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(
        path.includes('/files/')
          ? text('# Title\n\n![x](https://example.com/x.png)')
          : json(meta(3))
      )
    );
    const el = await open();

    expect(artifactCalls()).toEqual([
      `/api/v1/artifacts/${ID}`,
      `/api/v1/artifacts/${ID}/versions/3/files/design.md?stream=1`,
    ]);
    expect(q(el, 'sl-dialog')?.getAttribute('label')).toBe('Design notes');
    // Artifact Markdown renders only through the artifact viewer's own
    // sandboxed frame component, fed the version's files for its images;
    // never the chat's markdown preview, an <img> in the page, or source.
    expect(q(el, 'scion-markdown-preview')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('img')).toHaveLength(0);
    expect(q(el, 'scion-code-editor')).toBeNull();
    const frame = q<ScionArtifactMarkdownFrame>(el, 'scion-artifact-markdown-frame');
    expect(frame).toBeInstanceOf(ScionArtifactMarkdownFrameCtor);
    expect(frame?.content).toBe('# Title\n\n![x](https://example.com/x.png)');
    expect(frame?.artifactId).toBe(ID);
    expect(frame?.seq).toBe(3);
    expect(frame?.entryPath).toBe('design.md');
    expect(frame?.files.map((f) => f.path)).toEqual(['design.md']);
    expect(frame?.critic).toBe('off');
    // No name could be looked up here: the owner shows as its short id.
    expect(q(el, '.footer .path')?.textContent).toBe('design.md · owner agent-1 (agent)');
    expect(q(el, '.version-badge')?.textContent?.trim()).toBe('v3 · current');
    expect(buttons(el)).toEqual(['Copy link', 'Open in artifact viewer']);
    const viewer = el.shadowRoot?.querySelectorAll('.footer sl-button')[1];
    expect(viewer?.getAttribute('href')).toBe(`/projects/proj-1/artifacts/${ID}`);
    expect(viewer?.querySelector('sl-icon')?.getAttribute('name')).toBe('box-arrow-up-right');
  });

  it('names the owner and home project in the footer through the viewer\'s own lookups', async () => {
    apiFetchMock.mockImplementation((path: string) => {
      if (path === '/api/v1/agents/agent-1') return Promise.resolve(json({ name: 'docs-writer' }));
      if (path === '/api/v1/projects/proj-1') return Promise.resolve(json({ name: 'web-frontend' }));
      return Promise.resolve(path.includes('/files/') ? text('# T') : json(meta(3)));
    });
    const el = await open();
    expect(q(el, '.footer .path')?.textContent).toBe(
      'design.md · owner docs-writer (agent) · web-frontend'
    );
  });

  it('leaves out a home project the viewer cannot read and looks nothing up when unavailable', async () => {
    apiFetchMock.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/projects/')) return Promise.resolve(json({}, 403));
      if (path.startsWith('/api/v1/agents/')) return Promise.resolve(json({}, 403));
      return Promise.resolve(path.includes('/files/') ? text('# T') : json(meta(3)));
    });
    const el = await open();
    expect(q(el, '.footer .path')?.textContent).toBe('design.md · owner agent-1 (agent)');

    resetPrincipalNames();
    apiFetchMock.mockReset();
    apiFetchMock.mockResolvedValue(json({}, 404));
    el.target = { kind: 'artifact', id: ID, seq: 2, name: 'Artifact' };
    for (let i = 0; i < 8; i++) {
      await Promise.resolve();
      await el.updateComplete;
    }
    expect(q(el, '.footer .path')?.textContent).toBe('');
    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual([`/api/v1/artifacts/${ID}/versions/2`]);
  });

  it('loads the pinned version for a reference with a seq', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(path.includes('/files/') ? text('v1 text') : json(meta(1, 'text/plain')))
    );
    const el = await open(1);
    expect(artifactCalls()).toEqual([
      `/api/v1/artifacts/${ID}/versions/1`,
      `/api/v1/artifacts/${ID}/versions/1/files/design.md?stream=1`,
    ]);
    // Non-Markdown text is unchanged: read-only text, no Markdown frame.
    expect(q(el, 'scion-code-editor')).toBeTruthy();
    expect(q(el, 'scion-artifact-markdown-frame')).toBeNull();
    expect(q(el, '.version-badge')?.textContent?.trim()).toBe('v1');
    expect(buttons(el)).toEqual(['Copy link', 'Open in artifact viewer']);
  });

  it.each([403, 404])(
    'shows the same unavailable state for %i, with no title, retry or viewer link',
    async (status) => {
      apiFetchMock.mockResolvedValue(json({}, status));
      const el = await open();
      expect(q(el, 'sl-dialog')?.getAttribute('label')).toBe('Artifact unavailable');
      expect(q(el, '.file-preview-placeholder.error')?.textContent).toContain(
        ARTIFACT_UNAVAILABLE_MESSAGE
      );
      expect(buttons(el)).toEqual(['Copy link', 'Close']);
      expect(q(el, '.footer .path')?.textContent).toBe('');
      expect(q(el, '.version-badge')).toBeNull();
      expect(apiFetchMock).toHaveBeenCalledTimes(1);
    }
  );

  it('shows the same unavailable state when the entry file is refused after the metadata loaded', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(path.includes('/files/') ? json({}, 404) : json(meta(3, 'text/plain')))
    );
    const el = await open();
    expect(q(el, 'sl-dialog')?.getAttribute('label')).toBe('Artifact unavailable');
    expect(q(el, '.footer .path')?.textContent).toBe('');
    expect(q(el, '.version-badge')).toBeNull();
    expect(buttons(el)).toEqual(['Copy link', 'Close']);
  });

  it('does not fetch an entry it would not render, and points to the viewer', async () => {
    apiFetchMock.mockResolvedValue(json(meta(3, 'application/pdf')));
    const el = await open();
    expect(artifactCalls()).toHaveLength(1);
    expect(q(el, '.file-preview-placeholder')?.textContent).toContain(
      'Open it in the artifact viewer'
    );
    expect(buttons(el)).toContain('Open in artifact viewer');
  });

  it('revokes the previous image URL when the target changes and on disconnect', async () => {
    const created: string[] = [];
    const revoked: string[] = [];
    const origCreate = URL.createObjectURL;
    const origRevoke = URL.revokeObjectURL;
    URL.createObjectURL = () => {
      const url = `blob:img-${created.length + 1}`;
      created.push(url);
      return url;
    };
    URL.revokeObjectURL = (url: string) => {
      revoked.push(url);
    };
    try {
      apiFetchMock.mockImplementation((path: string) =>
        Promise.resolve(
          path.includes('/files/')
            ? { ok: true, status: 200, blob: () => Promise.resolve(new Blob(['x'])) }
            : json(meta(3, 'image/png'))
        )
      );
      const el = await open();
      expect(created).toEqual(['blob:img-1']);
      expect(q<HTMLImageElement>(el, 'img')?.getAttribute('src')).toBe('blob:img-1');

      // A new target replaces the image: the previous URL is revoked first.
      el.target = { kind: 'artifact', id: ID, seq: 2, name: 'Artifact' };
      for (let i = 0; i < 8; i++) {
        await Promise.resolve();
        await el.updateComplete;
      }
      expect(revoked).toEqual(['blob:img-1']);
      expect(created).toEqual(['blob:img-1', 'blob:img-2']);

      // Disconnecting revokes the current one.
      el.remove();
      expect(revoked).toEqual(['blob:img-1', 'blob:img-2']);
    } finally {
      URL.createObjectURL = origCreate;
      URL.revokeObjectURL = origRevoke;
    }
  });

  it('copies the reference', async () => {
    apiFetchMock.mockResolvedValue(json({}, 404));
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const el = await open(2);
    (el.shadowRoot?.querySelector('.footer sl-button') as HTMLElement).click();
    await Promise.resolve();
    expect(writeText).toHaveBeenCalledWith(`scion://artifact/${ID}@2`);
  });
});
