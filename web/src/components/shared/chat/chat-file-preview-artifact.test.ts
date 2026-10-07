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

const ID = '5f1c2d3e-0000-4000-8000-0000000000aa';

function meta(seq: number, mediaType = 'text/markdown', size = 12) {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeRef: 'proj-1',
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
  });
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('loads the current version and shows Markdown as source only, with the artifact actions', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(
        path.includes('/files/')
          ? text('# Title\n\n![x](https://example.com/x.png)')
          : json(meta(3))
      )
    );
    const el = await open();

    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual([
      `/api/v1/artifacts/${ID}`,
      `/api/v1/artifacts/${ID}/versions/3/files/design.md?stream=1`,
    ]);
    expect(q(el, 'sl-dialog')?.getAttribute('label')).toBe('Design notes');
    // Artifact Markdown is never rendered in the page: no markdown preview,
    // no <img>, only the read-only source and a pointer to the viewer.
    expect(q(el, 'scion-markdown-preview')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('img')).toHaveLength(0);
    expect(q(el, 'scion-code-editor')).toBeTruthy();
    expect(q(el, '.artifact-source-note')?.textContent).toContain('Open in artifact viewer');
    expect(q(el, '.footer .path')?.textContent).toBe('design.md · v3');
    expect(buttons(el)).toEqual(['Copy link', 'Open in artifact viewer']);
    const viewer = el.shadowRoot?.querySelectorAll('.footer sl-button')[1];
    expect(viewer?.getAttribute('href')).toBe(`/projects/proj-1/artifacts/${ID}`);
    expect(viewer?.querySelector('sl-icon')?.getAttribute('name')).toBe('box-arrow-up-right');
  });

  it('loads the pinned version for a reference with a seq', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(path.includes('/files/') ? text('v1 text') : json(meta(1, 'text/plain')))
    );
    const el = await open(1);
    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual([
      `/api/v1/artifacts/${ID}/versions/1`,
      `/api/v1/artifacts/${ID}/versions/1/files/design.md?stream=1`,
    ]);
    expect(q(el, 'scion-code-editor')).toBeTruthy();
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
      expect(buttons(el)).toEqual(['Copy link']);
      expect(apiFetchMock).toHaveBeenCalledTimes(1);
    }
  );

  it('does not fetch an entry it would not render, and points to the viewer', async () => {
    apiFetchMock.mockResolvedValue(json(meta(3, 'application/pdf')));
    const el = await open();
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(q(el, '.file-preview-placeholder')?.textContent).toContain(
      'Open it in the artifact viewer'
    );
    expect(buttons(el)).toContain('Open in artifact viewer');
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
