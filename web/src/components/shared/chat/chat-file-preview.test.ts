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

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { TEXT_PREVIEW_MAX_BYTES } from '../../../utils/chat-file-links.js';

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: (path: string) => (path.endsWith('.go') ? 'go' : 'plaintext'),
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
  extractApiError: async (res: Response, fallback: string) => {
    try {
      const body = (await res.json()) as { error?: string };
      return body?.error ?? fallback;
    } catch {
      return fallback;
    }
  },
}));

await import('./chat-file-preview.js');
type ScionChatFilePreview = import('./chat-file-preview.js').ScionChatFilePreview;
type PreviewTarget = import('./chat-file-preview.js').PreviewTarget;

async function mount(): Promise<ScionChatFilePreview> {
  const el = document.createElement('scion-chat-file-preview') as ScionChatFilePreview;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** Drain the fetch microtasks and the renders they trigger. */
async function settle(el: ScionChatFilePreview): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

function dialog(el: ScionChatFilePreview): Element | null {
  return el.shadowRoot?.querySelector('sl-dialog.file-preview-dialog') ?? null;
}

const TEXT_ATTACHMENT: PreviewTarget = {
  kind: 'attachment',
  id: 'att-1',
  name: 'notes.txt',
  mime: 'text/plain',
  size: 5,
};

const MD_ATTACHMENT: PreviewTarget = {
  kind: 'attachment',
  id: 'att-md',
  name: 'readme.md',
  mime: 'text/markdown',
  size: 5,
};

const IMAGE_ATTACHMENT: PreviewTarget = {
  kind: 'attachment',
  id: 'att-img',
  name: 'shot.png',
  mime: 'image/png',
  size: 2048,
};

const PATH_TARGET: PreviewTarget = {
  kind: 'path',
  projectId: 'proj-1',
  containerPath: '/workspace/notes.txt',
  location: { kind: 'workspace', filePath: 'notes.txt' },
  name: 'notes.txt',
};

describe('scion-chat-file-preview', () => {
  beforeEach(() => {
    apiFetchMock.mockReset();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders nothing when target is null', async () => {
    const el = await mount();
    expect(dialog(el)).toBeNull();
  });

  it('loads and renders a text attachment as a read-only editor', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('hello') });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/chat/attachments/att-1', expect.anything());
    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('hello');
    expect(dialog(el)?.getAttribute('label')).toBe('notes.txt');
  });

  it('fetches a path target with ?format=json and shows its content', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'file body', size: 9 }),
    });
    const el = await mount();
    el.target = PATH_TARGET;
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/projects/proj-1/workspace/files/notes.txt?format=json',
      expect.anything()
    );
    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('file body');
  });

  it('falls back to download-only for a path whose content exceeds the text-preview limit', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: '', size: 600 * 1024 }),
    });
    const el = await mount();
    el.target = PATH_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain("can't be shown here");
  });

  it('falls back to download-only for a small attachment whose MIME type is not a recognized text type, without ever fetching its body', async () => {
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-zip',
      name: 'bundle.zip',
      mime: 'application/zip',
      size: 2048, // well under the size limit — this must be classified by MIME, not size
    };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("can't be shown here");
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    // The whole point is to never fetch binary bytes as text — a build that
    // only classified by size would still call apiFetch here.
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('falls back to download-only for a small path whose extension is not a recognized text type, without ever fetching its body', async () => {
    const el = await mount();
    el.target = {
      kind: 'path',
      projectId: 'proj-1',
      containerPath: '/workspace/archive.tar.gz',
      location: { kind: 'workspace', filePath: 'archive.tar.gz' },
      name: 'archive.tar.gz',
    };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("can't be shown here");
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('still fetches and renders a small attachment whose MIME is an unrecognized-extension but known text application type', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve('{"a":1}'),
    });
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-json',
      name: 'data.blob', // an extension this app doesn't otherwise recognize
      mime: 'application/json',
      size: 7,
    };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('{"a":1}');
  });

  it('renders a path with an ordinary text extension this app does not otherwise enumerate — no client allow-list rejects it', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'module scion.example.com/foo\n', size: 30 }),
    });
    const el = await mount();
    el.target = {
      kind: 'path',
      projectId: 'proj-1',
      containerPath: '/workspace/go.mod',
      location: { kind: 'workspace', filePath: 'go.mod' },
      name: 'go.mod',
    };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe(
      'module scion.example.com/foo\n'
    );
  });

  it('renders a path with no extension at all that is not a well-known name — the deny-list has nothing to reject it on', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'node_modules/\n*.log\n', size: 20 }),
    });
    const el = await mount();
    el.target = {
      kind: 'path',
      projectId: 'proj-1',
      containerPath: '/workspace/.gitignore',
      location: { kind: 'workspace', filePath: '.gitignore' },
      name: '.gitignore',
    };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('node_modules/\n*.log\n');
  });

  it('renders a generic application/octet-stream attachment whose name is a recognized text file — the shape a real agent attachment with an unrecognized extension reports', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve('#!/bin/sh\necho hi\n'),
    });
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-run-sh',
      name: 'run.sh',
      mime: 'application/octet-stream',
      size: 19,
    };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('#!/bin/sh\necho hi\n');
  });

  it('renders an application/octet-stream attachment with a MIME parameter (e.g. charset) whose name is a recognized text file', async () => {
    // The MIME comparison must normalize case and strip `;`-delimited
    // parameters before comparing to `application/octet-stream` — a real
    // server or proxy can report either variant.
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve('#!/bin/sh\necho hi\n'),
    });
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-run-sh-charset',
      name: 'run.sh',
      mime: 'Application/Octet-Stream; charset=binary',
      size: 19,
    };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('#!/bin/sh\necho hi\n');
  });

  it('still falls back to download-only for a generic application/octet-stream attachment whose name is not a recognized text file', async () => {
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-bin',
      name: 'archive.bin',
      mime: 'application/octet-stream',
      size: 19,
    };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("can't be shown here");
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('falls back to download-only for a text-named attachment whose MIME is not application/octet-stream, without ever fetching its body', async () => {
    // The recognized-text-name fallback only applies to a generic
    // application/octet-stream MIME (the shape an unmapped-extension agent
    // attachment reports) — a text-looking name with some other binary MIME
    // (e.g. a mislabeled or actually-compressed notes.txt) must still be
    // classified by that MIME, not waved through on name alone.
    const el = await mount();
    el.target = {
      kind: 'attachment',
      id: 'att-mislabeled',
      name: 'notes.txt',
      mime: 'application/zip',
      size: 19,
    };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("can't be shown here");
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('renders a markdown attachment as rendered preview by default, with a source toggle', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('# hi') });
    const el = await mount();
    el.target = MD_ATTACHMENT;
    await settle(el);

    expect(dialog(el)?.querySelector('scion-markdown-preview')).not.toBeNull();
    const toggle = dialog(el)
      ?.querySelector('sl-button sl-icon[name="code"]')
      ?.closest('sl-button');
    (toggle as HTMLElement).dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true })
    );
    await settle(el);
    expect(dialog(el)?.querySelector('scion-code-editor')).not.toBeNull();
  });

  it('loads an image via a fetched object URL, not the bare attachment URL', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
    });
    const el = await mount();
    el.target = IMAGE_ATTACHMENT;
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/attachments/att-img?view=true',
      expect.anything()
    );
    const img = dialog(el)?.querySelector('img.file-preview-image');
    expect(img?.getAttribute('src')).toMatch(/^blob:/);
  });

  it('loads an image whose MIME has different case and a parameter, as an inline image, not the binary placeholder', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
    });
    const el = await mount();
    el.target = { ...IMAGE_ATTACHMENT, id: 'att-img-case', mime: 'Image/PNG; foo=bar' };
    await settle(el);

    const img = dialog(el)?.querySelector('img.file-preview-image');
    expect(img?.getAttribute('src')).toMatch(/^blob:/);
    expect(dialog(el)?.textContent).not.toContain("can't be shown here");
  });

  it('revokes the previous object URL when the target changes to a different image', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
    });
    const el = await mount();
    el.target = IMAGE_ATTACHMENT;
    await settle(el);
    const firstUrl = dialog(el)?.querySelector('img.file-preview-image')?.getAttribute('src');

    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    el.target = { ...IMAGE_ATTACHMENT, id: 'att-img-2' };
    await settle(el);

    expect(revokeSpy).toHaveBeenCalledWith(firstUrl);
    revokeSpy.mockRestore();
  });

  it('shows a 403-specific message', async () => {
    apiFetchMock.mockResolvedValue({ ok: false, status: 403, json: () => Promise.resolve({}) });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);
    expect(dialog(el)?.textContent).toContain("don't have permission");
  });

  it('shows a 404-specific message', async () => {
    apiFetchMock.mockResolvedValue({ ok: false, status: 404, json: () => Promise.resolve({}) });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);
    expect(dialog(el)?.textContent).toContain('could not be found');
  });

  it('shows a Retry action on error, which re-issues the request', async () => {
    apiFetchMock
      .mockResolvedValueOnce({ ok: false, status: 500, json: () => Promise.resolve({}) })
      .mockResolvedValueOnce({ ok: true, status: 200, text: () => Promise.resolve('recovered') });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);
    expect(dialog(el)?.textContent).toContain('Retry');

    const retryButton = Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Retry')
    ) as HTMLElement;
    retryButton.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('recovered');
  });

  it('shows an error with Close/Retry, not a crash, for a target with an unsafe project id', async () => {
    const el = await mount();
    el.target = { ...PATH_TARGET, projectId: '..' };
    await settle(el);

    // The dialog itself must still render (sl-dialog's own header close
    // button is available) rather than render() throwing and producing
    // nothing at all.
    expect(dialog(el)).not.toBeNull();
    expect(apiFetchMock).not.toHaveBeenCalled();
    expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).not.toBeNull();
    expect(dialog(el)?.textContent).toContain('Retry');
    // No Download link for a target whose URL could not safely be built.
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(false);
    // A fixed, generic message — never the builder's own internal error
    // text (e.g. "buildFileApiUrl: unsafe project id"), which is meant for
    // developers reading a stack trace, not for a user in a dialog.
    expect(dialog(el)?.textContent).toContain("This file link can't be opened.");
    expect(dialog(el)?.textContent).not.toContain('unsafe project id');
    expect(dialog(el)?.textContent).not.toContain('buildFileApiUrl');
  });

  it('shows the same generic link error, not the binary placeholder, for an unsafe project id whose name is not a recognized text type', async () => {
    // The URL-safety check must run before binary classification: a target
    // this unsafe can never be fetched or downloaded regardless of what its
    // name suggests, so it must always land on the "link can't be opened"
    // error (with its own Retry), never the binary placeholder (which offers
    // a Download button — and one that couldn't work here anyway).
    const el = await mount();
    el.target = {
      ...PATH_TARGET,
      projectId: '..',
      containerPath: '/workspace/archive.tar.gz',
      location: { kind: 'workspace', filePath: 'archive.tar.gz' },
      name: 'archive.tar.gz',
    };
    await settle(el);

    expect(apiFetchMock).not.toHaveBeenCalled();
    expect(dialog(el)?.textContent).toContain("This file link can't be opened.");
    expect(dialog(el)?.textContent).not.toContain("can't be shown here");
    expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).not.toBeNull();
  });

  it('does not publish an error for an intentionally aborted (superseded) load', async () => {
    let rejectFirst!: (err: unknown) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise((_, reject) => {
          rejectFirst = reject;
        })
    );
    apiFetchMock.mockResolvedValueOnce({
      ok: true,
      status: 200,
      text: () => Promise.resolve('second'),
    });

    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    // Switch targets before the first request resolves — this aborts it.
    el.target = { ...TEXT_ATTACHMENT, id: 'att-2', name: 'second.txt' };
    await settle(el);

    // The first request's controller was aborted; simulate the browser
    // delivering the resulting AbortError after the switch already happened.
    rejectFirst(new DOMException('aborted', 'AbortError'));
    await settle(el);

    expect(dialog(el)?.textContent).not.toContain('error');
    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second');
  });

  it('does not publish an error for an AbortError within the current (unsuperseded) generation', async () => {
    // Isolates the AbortError check from the generation guard above: the
    // browser/fetch layer can itself surface AbortError (e.g. the page's own
    // teardown aborting the signal) without this component having started a
    // newer load — the generation would still match, so only the explicit
    // "whatever the error's type" AbortError check protects this case.
    apiFetchMock.mockImplementationOnce(() =>
      Promise.reject(new DOMException('aborted', 'AbortError'))
    );
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);

    expect(dialog(el)?.textContent).not.toContain('error');
    expect(dialog(el)?.querySelector('sl-button')?.textContent).not.toContain('Retry');
  });

  it('shows an error, not a silently-stuck spinner, for a non-abort DOMException', async () => {
    // e.g. blob.text() can itself throw a DOMException that is not an
    // AbortError — narrowing the catch's special case to "any DOMException"
    // would swallow this one too, leaving the panel spinning forever.
    apiFetchMock.mockImplementationOnce(() =>
      Promise.reject(new DOMException('bad encoding', 'EncodingError'))
    );
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);

    expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).not.toBeNull();
  });

  it('renders a file of exactly the inline size limit as text, not a download-only fallback', async () => {
    const TEXT_PREVIEW_MAX_BYTES = 512 * 1024;
    apiFetchMock.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({ content: 'exactly at the limit', size: TEXT_PREVIEW_MAX_BYTES }),
    });
    const el = await mount();
    el.target = PATH_TARGET;
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('exactly at the limit');
    expect(dialog(el)?.textContent).not.toContain('download');
  });

  it('does not show a Source toggle while a markdown target is still loading', async () => {
    apiFetchMock.mockImplementationOnce(() => new Promise(() => {})); // never resolves
    const el = await mount();
    el.target = MD_ATTACHMENT;
    await settle(el);

    const sourceButton = Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Source')
    );
    expect(sourceButton).toBeUndefined();
  });

  it('does not show a Source toggle while a markdown target is in an error state', async () => {
    apiFetchMock.mockResolvedValueOnce({ ok: false, status: 404, text: () => Promise.resolve('') });
    const el = await mount();
    el.target = MD_ATTACHMENT;
    await settle(el);

    expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).not.toBeNull();
    const sourceButton = Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Source')
    );
    expect(sourceButton).toBeUndefined();
  });

  it('dispatches chat-file-preview-close when the dialog is dismissed', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('hi') });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);

    const seen = vi.fn();
    el.addEventListener('chat-file-preview-close', seen);
    dialog(el)!.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));

    expect(seen).toHaveBeenCalledTimes(1);
  });

  it('ignores a sl-after-hide bubbling up from a nested element, not the dialog itself', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('hi') });
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await settle(el);

    const seen = vi.fn();
    el.addEventListener('chat-file-preview-close', seen);
    const nested = dialog(el)!.querySelector('scion-code-editor')!;
    nested.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));

    expect(seen).not.toHaveBeenCalled();
  });

  it('revokes the object URL on disconnect', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
    });
    const el = await mount();
    el.target = IMAGE_ATTACHMENT;
    await settle(el);
    const url = dialog(el)?.querySelector('img.file-preview-image')?.getAttribute('src');

    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    el.remove();
    expect(revokeSpy).toHaveBeenCalledWith(url);
    revokeSpy.mockRestore();
  });

  describe('reconnect after disconnect (e.g. a keyed repeat move)', () => {
    function imageResponse() {
      return {
        ok: true,
        status: 200,
        blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
      };
    }

    function imgSrc(el: ScionChatFilePreview): string | null | undefined {
      return dialog(el)?.querySelector('img.file-preview-image')?.getAttribute('src');
    }

    it('never renders a revoked object URL after the same instance is moved', async () => {
      const revoked = new Set<string>();
      const revokeSpy = vi
        .spyOn(URL, 'revokeObjectURL')
        .mockImplementation((url: string) => void revoked.add(url));
      apiFetchMock.mockImplementation(() => Promise.resolve(imageResponse()));
      const host = document.createElement('div');
      const other = document.createElement('div');
      document.body.append(host, other);
      const el = document.createElement('scion-chat-file-preview') as ScionChatFilePreview;
      host.appendChild(el);
      el.target = IMAGE_ATTACHMENT;
      await settle(el);
      const firstUrl = imgSrc(el);
      expect(firstUrl).toMatch(/^blob:/);

      // A DOM move (what lit's keyed repeat does on reorder) is a
      // disconnect followed by a connect of the very same instance.
      other.appendChild(el);
      await settle(el);

      expect(revoked.has(firstUrl as string)).toBe(true);
      const src = imgSrc(el);
      expect(src).toMatch(/^blob:/);
      expect(revoked.has(src as string)).toBe(false);
      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      revokeSpy.mockRestore();
    });

    it('does not refetch while detached, and revokes the reloaded URL on final removal', async () => {
      const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
      apiFetchMock.mockImplementation(() => Promise.resolve(imageResponse()));
      const el = await mount();
      el.target = IMAGE_ATTACHMENT;
      await settle(el);

      el.remove();
      await settle(el);
      expect(apiFetchMock).toHaveBeenCalledTimes(1);
      expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();

      document.body.appendChild(el);
      await settle(el);
      const reloaded = imgSrc(el);
      expect(apiFetchMock).toHaveBeenCalledTimes(2);

      el.remove();
      expect(revokeSpy).toHaveBeenCalledWith(reloaded);
      revokeSpy.mockRestore();
    });

    it('reloads, rather than spinning forever, when disconnected mid-fetch', async () => {
      apiFetchMock
        .mockImplementationOnce(
          (_path: string, options?: RequestInit) =>
            new Promise((_resolve, reject) => {
              options?.signal?.addEventListener('abort', () =>
                reject(new DOMException('aborted', 'AbortError'))
              );
            })
        )
        .mockImplementation(() => Promise.resolve(imageResponse()));
      const el = await mount();
      el.target = IMAGE_ATTACHMENT;
      await settle(el);

      el.remove();
      document.body.appendChild(el);
      await settle(el);

      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      expect(imgSrc(el)).toMatch(/^blob:/);
    });

    it('leaves no unrevoked object URL when removed while blob() is pending', async () => {
      const pendingBlob = deferred<Blob>();
      apiFetchMock.mockImplementationOnce(() =>
        Promise.resolve({ ok: true, status: 200, blob: () => pendingBlob.promise })
      );
      const created: string[] = [];
      const revoked = new Set<string>();
      const createSpy = vi.spyOn(URL, 'createObjectURL').mockImplementation(() => {
        const url = `blob:test-${created.length}`;
        created.push(url);
        return url;
      });
      const revokeSpy = vi
        .spyOn(URL, 'revokeObjectURL')
        .mockImplementation((url: string) => void revoked.add(url));
      try {
        const el = await mount();
        el.target = IMAGE_ATTACHMENT;
        await settle(el);

        el.remove();
        pendingBlob.resolve(new Blob(['x'], { type: 'image/png' }));
        await settle(el);

        expect(created.filter((url) => !revoked.has(url))).toEqual([]);
      } finally {
        createSpy.mockRestore();
        revokeSpy.mockRestore();
      }
    });

    it('drops the pending reload when the target changes while detached', async () => {
      apiFetchMock.mockImplementationOnce(() => Promise.resolve(imageResponse()));
      apiFetchMock.mockImplementation(() =>
        Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('hello') })
      );
      const el = await mount();
      el.target = IMAGE_ATTACHMENT;
      await settle(el);

      el.remove();
      el.target = TEXT_ATTACHMENT;
      await settle(el);
      expect(apiFetchMock).toHaveBeenCalledTimes(2);

      document.body.appendChild(el);
      await settle(el);

      // The target change already loaded the new target; reconnecting must
      // not load it a second time.
      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      const editor = dialog(el)?.querySelector('scion-code-editor');
      expect((editor as unknown as { content: string })?.content).toBe('hello');
    });

    it('fetches once when reconnect and a target change land in one update', async () => {
      apiFetchMock.mockImplementationOnce(() => Promise.resolve(imageResponse()));
      apiFetchMock.mockImplementation(() =>
        Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('hello') })
      );
      const el = await mount();
      el.target = IMAGE_ATTACHMENT;
      await settle(el);
      expect(apiFetchMock).toHaveBeenCalledTimes(1);

      el.remove();
      document.body.appendChild(el);
      el.target = TEXT_ATTACHMENT;
      await settle(el);

      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      expect(apiFetchMock).toHaveBeenLastCalledWith(
        '/api/v1/chat/attachments/att-1',
        expect.anything()
      );
    });

    it('does not refetch a text preview on reconnect: its content is still valid', async () => {
      apiFetchMock.mockResolvedValue({
        ok: true,
        status: 200,
        text: () => Promise.resolve('hello'),
      });
      const el = await mount();
      el.target = TEXT_ATTACHMENT;
      await settle(el);

      el.remove();
      document.body.appendChild(el);
      await settle(el);

      expect(apiFetchMock).toHaveBeenCalledTimes(1);
      const editor = dialog(el)?.querySelector('scion-code-editor');
      expect((editor as unknown as { content: string })?.content).toBe('hello');
    });
  });

  // -- isolated concurrency guards ------------------------------------------
  //
  // A fast-resolving mock lets every generation check downstream of the first
  // one redundantly catch the same staleness, so mutating any single check
  // alone never changes the *final* state in the tests above. Each test here
  // controls exactly when one specific request settles (via a deferred
  // promise) relative to a full second load completing, so only the one
  // guard under test can prevent the stale response from clobbering the
  // newer target's already-settled state.

  interface Deferred<T> {
    promise: Promise<T>;
    resolve: (value: T) => void;
    reject: (reason: unknown) => void;
  }
  function deferred<T>(): Deferred<T> {
    let resolve!: (value: T) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }

  const SECOND_TEXT_ATTACHMENT: PreviewTarget = {
    kind: 'attachment',
    id: 'att-second',
    name: 'second.txt',
    mime: 'text/plain',
    size: 5,
  };

  it('an error response for a superseded image fetch does not clobber the newer target', async () => {
    const firstFetch = deferred<{ ok: boolean; status: number; json: () => Promise<unknown> }>();
    apiFetchMock.mockImplementationOnce(() => firstFetch.promise);
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('second content') })
    );

    const el = await mount();
    el.target = IMAGE_ATTACHMENT; // slow; will error later
    await Promise.resolve();
    el.target = SECOND_TEXT_ATTACHMENT; // fast; completes fully first
    await settle(el);
    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');

    firstFetch.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) });
    await settle(el);

    const editorAfter = dialog(el)?.querySelector('scion-code-editor');
    expect((editorAfter as unknown as { content: string })?.content).toBe('second content');
    expect(dialog(el)?.textContent).not.toContain('Retry');
  });

  it('a slow image blob read for a superseded load does not clobber the newer target', async () => {
    const firstBlob = deferred<Blob>();
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, blob: () => firstBlob.promise })
    );
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('second content') })
    );

    const el = await mount();
    el.target = IMAGE_ATTACHMENT;
    await Promise.resolve();
    el.target = SECOND_TEXT_ATTACHMENT;
    await settle(el);

    firstBlob.resolve(new Blob(['x'], { type: 'image/png' }));
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');
    expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
  });

  it('an error response for a superseded text fetch does not clobber the newer target', async () => {
    const firstFetch = deferred<{ ok: boolean; status: number; json: () => Promise<unknown> }>();
    apiFetchMock.mockImplementationOnce(() => firstFetch.promise);
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('second content') })
    );

    const el = await mount();
    el.target = TEXT_ATTACHMENT; // slow; will error later
    await Promise.resolve();
    el.target = SECOND_TEXT_ATTACHMENT; // fast; completes fully first
    await settle(el);

    firstFetch.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) });
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');
    expect(dialog(el)?.textContent).not.toContain('Retry');
  });

  it('a slow text body read for a superseded load does not clobber the newer target', async () => {
    const firstText = deferred<string>();
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => firstText.promise })
    );
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('second content') })
    );

    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    el.target = SECOND_TEXT_ATTACHMENT;
    await settle(el);

    firstText.resolve('first content — should never be shown');
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');
  });

  it('a rejected fetch for a superseded load does not clobber the newer target (catch-block guard)', async () => {
    const firstFetch = deferred<never>();
    apiFetchMock.mockImplementationOnce(() => firstFetch.promise);
    apiFetchMock.mockImplementationOnce(() =>
      Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('second content') })
    );

    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    el.target = SECOND_TEXT_ATTACHMENT;
    await settle(el);

    firstFetch.reject(new Error('network down'));
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');
    expect(dialog(el)?.textContent).not.toContain('Retry');
  });

  it('aborts the previous controller when the target changes', async () => {
    const abortSpy = vi.spyOn(AbortController.prototype, 'abort');
    apiFetchMock.mockResolvedValue(new Promise(() => {})); // never resolves
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    abortSpy.mockClear();

    el.target = SECOND_TEXT_ATTACHMENT;
    await Promise.resolve();

    expect(abortSpy).toHaveBeenCalledTimes(1);
    abortSpy.mockRestore();
  });

  it('aborts the in-flight controller when the target is cleared', async () => {
    const abortSpy = vi.spyOn(AbortController.prototype, 'abort');
    apiFetchMock.mockResolvedValue(new Promise(() => {}));
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    abortSpy.mockClear();

    el.target = null;
    await Promise.resolve();

    expect(abortSpy).toHaveBeenCalledTimes(1);
    abortSpy.mockRestore();
  });

  it('aborts the in-flight controller on disconnect', async () => {
    const abortSpy = vi.spyOn(AbortController.prototype, 'abort');
    apiFetchMock.mockResolvedValue(new Promise(() => {}));
    const el = await mount();
    el.target = TEXT_ATTACHMENT;
    await Promise.resolve();
    abortSpy.mockClear();

    el.remove();

    expect(abortSpy).toHaveBeenCalledTimes(1);
    abortSpy.mockRestore();
  });

  describe('copy-feedback timer', () => {
    afterEach(() => {
      vi.useRealTimers();
    });

    /** Toggle Source on (or re-toggle after a target change reset it) and click Copy. */
    async function toggleSourceAndCopy(el: ScionChatFilePreview): Promise<void> {
      const sourceToggle = dialog(el)
        ?.querySelector('sl-button sl-icon[name="code"]')
        ?.closest('sl-button');
      (sourceToggle as HTMLElement).dispatchEvent(
        new MouseEvent('click', { bubbles: true, composed: true })
      );
      await settle(el);
      const copyButton = dialog(el)
        ?.querySelector('sl-button sl-icon[name="clipboard"]')
        ?.closest('sl-button');
      (copyButton as HTMLElement).dispatchEvent(
        new MouseEvent('click', { bubbles: true, composed: true })
      );
      await settle(el);
    }

    it("clears the old target's pending copy-feedback timer as soon as the target changes", async () => {
      // A click on Copy always clears whatever timer is currently pending
      // before scheduling its own, so a stale timer alone can't corrupt a
      // *later* copy action on the new target — that path already
      // self-clears. What's left uncovered is a target change with no
      // further copy action on the new target: if the old timer is not
      // cancelled here, it stays scheduled for a target that's no longer
      // showing. `vi.getTimerCount()` reads the fake-timer scheduler
      // directly, checked immediately after the target change — a variant
      // that reassigns the field to `null` without calling `clearTimeout`
      // still leaves the timer scheduled at that point, so `getTimerCount()`
      // catches it where a field-only check would not.
      apiFetchMock.mockResolvedValue({
        ok: true,
        status: 200,
        text: () => Promise.resolve('# hi'),
      });
      vi.useFakeTimers();
      const el = await mount();
      el.target = MD_ATTACHMENT;
      await settle(el);
      await toggleSourceAndCopy(el);
      expect(vi.getTimerCount()).toBe(1);

      el.target = { ...MD_ATTACHMENT, id: 'att-md-2' };
      await settle(el);

      expect(vi.getTimerCount()).toBe(0);
    });

    it('clears the copy-feedback timer and state on disconnect, so a later reconnect shows Copy, not a stuck Copied!', async () => {
      apiFetchMock.mockResolvedValue({
        ok: true,
        status: 200,
        text: () => Promise.resolve('# hi'),
      });
      vi.useFakeTimers();
      const el = await mount();
      el.target = MD_ATTACHMENT;
      await settle(el);
      await toggleSourceAndCopy(el);
      expect(dialog(el)?.textContent).toContain('Copied!');

      el.remove();
      expect(vi.getTimerCount()).toBe(0);

      // Reconnect the same instance well before the original 1500ms
      // feedback window would have elapsed (its state, including `copied`,
      // survives a disconnect — this is not a fresh instance). Without
      // resetting `copied` on disconnect, this would still read "Copied!",
      // even though the copy action it reflects happened in a previous
      // connection the user can no longer relate it to.
      document.body.appendChild(el);
      await settle(el);

      expect(dialog(el)?.textContent).toContain('Copy');
      expect(dialog(el)?.textContent).not.toContain('Copied!');
    });

    it('clears its own copyTimer field when the timer fires normally, not just when cancelled', async () => {
      apiFetchMock.mockResolvedValue({
        ok: true,
        status: 200,
        text: () => Promise.resolve('# hi'),
      });
      vi.useFakeTimers();
      const el = await mount();
      el.target = MD_ATTACHMENT;
      await settle(el);
      await toggleSourceAndCopy(el);
      // Precondition: a timer was actually scheduled. Without this, a
      // build with no copy-feedback timer at all would also read `null`
      // here — trivially, since the field would never be set to begin
      // with — and this test would pass for the wrong reason.
      expect(vi.getTimerCount()).toBe(1);

      await vi.advanceTimersByTimeAsync(1500);

      expect((el as unknown as { copyTimer: unknown }).copyTimer).toBeNull();
    });
  });

  it('resets showSource when the target changes', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('# hi') });
    const el = await mount();
    el.target = MD_ATTACHMENT;
    await settle(el);
    const toggle = dialog(el)
      ?.querySelector('sl-button sl-icon[name="code"]')
      ?.closest('sl-button');
    (toggle as HTMLElement).dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true })
    );
    await settle(el);
    expect(dialog(el)?.querySelector('scion-code-editor')).not.toBeNull();

    el.target = { ...MD_ATTACHMENT, id: 'att-md-2' };
    await settle(el);

    expect(dialog(el)?.querySelector('scion-markdown-preview')).not.toBeNull();
  });

  it('does not show Copy for a markdown target before Source is toggled on', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('# hi') });
    const el = await mount();
    el.target = MD_ATTACHMENT;
    await settle(el);

    expect(dialog(el)?.textContent).not.toContain('Copy');
  });

  it('classifies a path target with an image extension as an image, not text', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['x'], { type: 'image/png' })),
    });
    const el = await mount();
    el.target = { ...PATH_TARGET, name: 'diagram.png', containerPath: '/workspace/diagram.png' };
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith(
      expect.stringContaining('?view=true'),
      expect.anything()
    );
    expect(dialog(el)?.querySelector('img.file-preview-image')).not.toBeNull();
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
  });
});

const GCS_MD_TARGET: PreviewTarget = {
  kind: 'gcs',
  messageId: '11111111-2222-4333-8444-555555555555',
  bucket: 'scion-xproject-exchange',
  object: 'workspace-volumes/dev-brief.md',
  name: 'dev-brief.md',
};

const GCS_JSON_TARGET: PreviewTarget = {
  kind: 'gcs',
  messageId: '11111111-2222-4333-8444-555555555555',
  bucket: 'bkt',
  object: 'data.json',
  name: 'data.json',
};

/** A minimal fetch Response-like object with a Content-Length header. */
function gcsTextResponse(body: string, status = 200): unknown {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: {
      get: (name: string) => (name.toLowerCase() === 'content-length' ? String(body.length) : null),
    },
    text: () => Promise.resolve(body),
    json: () => Promise.resolve({}),
  };
}

function gcsErrorResponse(status: number, details?: Record<string, unknown>): unknown {
  return {
    ok: false,
    status,
    headers: { get: () => null },
    json: () => Promise.resolve({ error: { details } }),
    text: () => Promise.resolve(''),
  };
}

describe('scion-chat-file-preview gcs target', () => {
  beforeEach(() => {
    apiFetchMock.mockReset();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('requests the correct URL, carrying the message id', async () => {
    apiFetchMock.mockResolvedValue(gcsTextResponse('# hi'));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/gcs/object?message=11111111-2222-4333-8444-555555555555&bucket=scion-xproject-exchange&object=workspace-volumes%2Fdev-brief.md',
      expect.anything()
    );
  });

  it('renders a .md object as markdown with the Source toggle', async () => {
    apiFetchMock.mockResolvedValue(gcsTextResponse('# hi'));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.querySelector('scion-markdown-preview')).not.toBeNull();
    expect(dialog(el)?.textContent).toContain('Source');
  });

  it('renders a .json object as code', async () => {
    apiFetchMock.mockResolvedValue(gcsTextResponse('{"a":1}'));
    const el = await mount();
    el.target = GCS_JSON_TARGET;
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect(editor).not.toBeNull();
    expect((editor as unknown as { content: string })?.content).toBe('{"a":1}');
  });

  it('fetches a binary-looking gcs name instead of showing the download-only placeholder, and shows the uniform message and Console fallback on 404', async () => {
    // A gcs target's content type is decided by the server response, not the
    // object name: unlike an attachment or a path, a binary-looking gcs name
    // (e.g. a .pdf) must still be fetched, so a denied/not-found/oversized
    // result gets the uniform error state and the Cloud Console fallback
    // instead of silently becoming a Download-only chip with no indication
    // anything went wrong.
    apiFetchMock.mockResolvedValue(gcsErrorResponse(404));
    const el = await mount();
    el.target = { ...GCS_MD_TARGET, object: 'report.pdf', name: 'report.pdf' };
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalled();
    expect(dialog(el)?.textContent).toContain("isn't available");
    expect(dialog(el)?.querySelector('a.console-fallback-link')).not.toBeNull();
  });

  it('shows no Download for a 413 on a binary-looking gcs name', async () => {
    apiFetchMock.mockResolvedValue(
      gcsErrorResponse(413, { size: 12 * 1024 * 1024, limit: 10 * 1024 * 1024 })
    );
    const el = await mount();
    el.target = { ...GCS_MD_TARGET, object: 'archive.zip', name: 'archive.zip' };
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalled();
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(false);
  });

  it('does not classify an image-like extension as an image when the response Content-Type is not image/*', async () => {
    // The response here carries no Content-Type header at all (gcsTextResponse
    // only ever reports Content-Length) — the extension alone must never be
    // enough. See the "renders as an image" cases below for the positive
    // control: the identical extension DOES render as an image once the
    // response Content-Type agrees.
    apiFetchMock.mockResolvedValue(gcsTextResponse('not actually png bytes'));
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'diagram.png', object: 'diagram.png' };
    await settle(el);

    expect(apiFetchMock).not.toHaveBeenCalledWith(
      expect.stringContaining('?view=true'),
      expect.anything()
    );
    expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
    expect(dialog(el)?.querySelector('scion-code-editor')).not.toBeNull();
  });

  for (const status of [403, 404]) {
    it(`shows the uniform not-available message and the Cloud Console fallback for ${status}`, async () => {
      apiFetchMock.mockResolvedValue(gcsErrorResponse(status));
      const el = await mount();
      el.target = GCS_MD_TARGET;
      await settle(el);

      expect(dialog(el)?.textContent).toContain("isn't available");
      const link = dialog(el)?.querySelector('a.console-fallback-link') as HTMLAnchorElement | null;
      expect(link).not.toBeNull();
      expect(link?.getAttribute('href')).toBe(
        'https://console.cloud.google.com/storage/browser/_details/scion-xproject-exchange/workspace-volumes/dev-brief.md'
      );
      expect(link?.getAttribute('target')).toBe('_blank');
      expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
    });
  }

  it('shows the rate-limit message and the console fallback for 429', async () => {
    apiFetchMock.mockResolvedValue(gcsErrorResponse(429));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain('Too many requests');
    expect(dialog(el)?.querySelector('a.console-fallback-link')).not.toBeNull();
  });

  it('shows the upstream-error message and the console fallback for 502', async () => {
    apiFetchMock.mockResolvedValue(gcsErrorResponse(502));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain("Couldn't fetch this object");
    expect(dialog(el)?.querySelector('a.console-fallback-link')).not.toBeNull();
  });

  it('shows the malformed-link message and NO console fallback for 400', async () => {
    apiFetchMock.mockResolvedValue(gcsErrorResponse(400));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain("isn't a valid gs:// URL");
    expect(dialog(el)?.querySelector('a.console-fallback-link')).toBeNull();
  });

  it('shows the size-limit message with the console fallback and no Download for 413', async () => {
    apiFetchMock.mockResolvedValue(
      gcsErrorResponse(413, { size: 12 * 1024 * 1024, limit: 10 * 1024 * 1024 })
    );
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain('too large to open here (12.0 MB, limit 10.0 MB)');
    expect(dialog(el)?.querySelector('a.console-fallback-link')).not.toBeNull();
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(false);
  });

  it('shows a one-decimal size for a non-whole-MiB object, not a misleadingly rounded whole number', async () => {
    // 10.4 MiB over a 10 MiB limit must not round down to "10 MB, limit 10 MB".
    apiFetchMock.mockResolvedValue(
      gcsErrorResponse(413, { size: Math.round(10.4 * 1024 * 1024), limit: 10 * 1024 * 1024 })
    );
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.textContent).toContain('too large to open here (10.4 MB, limit 10.0 MB)');
  });

  it('keeps Download for a non-413 gcs error', async () => {
    apiFetchMock.mockResolvedValue(gcsErrorResponse(404));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(true);
  });

  it('shows the console fallback for a status the hub is not documented to return', async () => {
    apiFetchMock.mockResolvedValue(gcsErrorResponse(500));
    const el = await mount();
    el.target = GCS_MD_TARGET;
    await settle(el);

    expect(dialog(el)?.querySelector('a.console-fallback-link')).not.toBeNull();
  });

  it('Retry re-issues the same gcs request', async () => {
    apiFetchMock
      .mockResolvedValueOnce(gcsErrorResponse(502))
      .mockResolvedValueOnce(gcsTextResponse('recovered'));
    const el = await mount();
    el.target = GCS_JSON_TARGET;
    await settle(el);

    const retryButton = Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Retry')
    ) as HTMLElement;
    retryButton.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('recovered');
  });
});

// ---------------------------------------------------------------------------
// gcs classification: image sniffing, SVG-as-source, octet-stream, and the
// Content-Length preview-size abort.
// ---------------------------------------------------------------------------

/**
 * A gcs response with an explicit Content-Type and Content-Length, plus
 * spies on text()/blob() so a test can assert the body was (or was never)
 * read — the abort-before-read requirement can't be proven just by
 * inspecting the final rendered state.
 */
function gcsTypedResponse(opts: {
  contentType: string;
  contentLength?: number;
  body?: string;
  textResult?: Promise<string>;
  blobResult?: Promise<Blob>;
}): {
  res: unknown;
  textSpy: ReturnType<typeof vi.fn>;
  blobSpy: ReturnType<typeof vi.fn>;
} {
  const body = opts.body ?? '';
  const textSpy = vi.fn(() => opts.textResult ?? Promise.resolve(body));
  const blobSpy = vi.fn(
    () => opts.blobResult ?? Promise.resolve(new Blob([body], { type: opts.contentType }))
  );
  return {
    res: {
      ok: true,
      status: 200,
      headers: {
        get: (name: string) => {
          const n = name.toLowerCase();
          if (n === 'content-type') return opts.contentType;
          if (n === 'content-length') {
            return opts.contentLength === undefined ? null : String(opts.contentLength);
          }
          return null;
        },
      },
      text: textSpy,
      blob: blobSpy,
      json: () => Promise.resolve({}),
    },
    textSpy,
    blobSpy,
  };
}

/** The AbortSignal passed to the most recent apiFetch call. */
function lastFetchSignal(): AbortSignal {
  const init = apiFetchMock.mock.calls.at(-1)?.[1] as RequestInit | undefined;
  const signal = init?.signal;
  if (!signal) throw new Error('apiFetch was called without an AbortSignal');
  return signal;
}

describe('scion-chat-file-preview gcs image/octet-stream/too-large classification', () => {
  beforeEach(() => {
    apiFetchMock.mockReset();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  const imageCases: Array<{ ext: string; contentType: string }> = [
    { ext: 'png', contentType: 'image/png' },
    { ext: 'jpg', contentType: 'image/jpeg' },
    { ext: 'jpeg', contentType: 'image/jpeg' },
    { ext: 'gif', contentType: 'image/gif' },
    { ext: 'webp', contentType: 'image/webp' },
  ];
  for (const { ext, contentType } of imageCases) {
    it(`renders a .${ext} gcs object as an image when the response Content-Type is ${contentType}`, async () => {
      const { res, textSpy } = gcsTypedResponse({ contentType, contentLength: 3 });
      apiFetchMock.mockResolvedValue(res);
      const el = await mount();
      el.target = { ...GCS_JSON_TARGET, name: `pic.${ext}`, object: `pic.${ext}` };
      await settle(el);

      const img = dialog(el)?.querySelector('img.file-preview-image');
      expect(img?.getAttribute('src')).toMatch(/^blob:/);
      expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
      expect(textSpy).not.toHaveBeenCalled();
    });
  }

  it('never renders a .svg gcs object as an image or as text when the response Content-Type is image-like', async () => {
    // The hub's sniffer never actually reports an image/* type for SVG bytes
    // (http.DetectContentType has no SVG signature), but the client-side
    // extension gate is independent of that: GCS_IMAGE_EXTENSIONS excludes
    // .svg outright, so even a hypothetical image/svg+xml response never
    // reaches the <img> branch, and an image/* body is never decoded as text.
    const { res, textSpy, blobSpy } = gcsTypedResponse({
      contentType: 'image/svg+xml',
      contentLength: 40,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'pic.svg', object: 'pic.svg' };
    await settle(el);

    expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(dialog(el)?.textContent).toContain("This file can't be previewed.");
    expect(textSpy).not.toHaveBeenCalled();
    expect(blobSpy).not.toHaveBeenCalled();
  });

  it('shows "This file can\'t be previewed." without reading the body for a .png name whose response Content-Type is image/bmp', async () => {
    const { res, textSpy, blobSpy } = gcsTypedResponse({
      contentType: 'image/bmp',
      contentLength: 24,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'pic.png', object: 'pic.png' };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("This file can't be previewed.");
    expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(textSpy).not.toHaveBeenCalled();
    expect(blobSpy).not.toHaveBeenCalled();
    expect(lastFetchSignal().aborted).toBe(true);
  });

  it('shows "This file can\'t be previewed." with Download, aborting the request without reading the body, for an image/* response on a non-image name', async () => {
    const { res, textSpy, blobSpy } = gcsTypedResponse({
      contentType: 'image/png',
      contentLength: 24,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'report.bin', object: 'report.bin' };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("This file can't be previewed.");
    expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(true);
    expect(textSpy).not.toHaveBeenCalled();
    expect(blobSpy).not.toHaveBeenCalled();
    expect(lastFetchSignal().aborted).toBe(true);
  });

  it('renders a .svg gcs object as source text for its real (text/plain) Content-Type', async () => {
    const { res } = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: 5,
      body: '<svg/>',
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'pic.svg', object: 'pic.svg' };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('<svg/>');
  });

  it('shows "This file can\'t be previewed." with Download, aborting the request without reading the body, for application/octet-stream', async () => {
    const { res, textSpy, blobSpy } = gcsTypedResponse({
      contentType: 'application/octet-stream',
      contentLength: 4,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'report.pdf', object: 'report.pdf' };
    await settle(el);

    expect(dialog(el)?.textContent).toContain("This file can't be previewed.");
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(true);
    expect(textSpy).not.toHaveBeenCalled();
    expect(blobSpy).not.toHaveBeenCalled();
    expect(lastFetchSignal().aborted).toBe(true);
  });

  it('pins the 512 KB threshold: exactly at the limit still previews inline as text, without aborting the request', async () => {
    expect(TEXT_PREVIEW_MAX_BYTES).toBe(524288);
    const body = 'x'.repeat(10);
    const { res, textSpy } = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: TEXT_PREVIEW_MAX_BYTES,
      body,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'big.txt', object: 'big.txt' };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe(body);
    expect(textSpy).toHaveBeenCalled();
    expect(lastFetchSignal().aborted).toBe(false);
  });

  it('pins the 512 KB threshold: one byte over aborts before reading the body and shows the too-large-inline message with Download', async () => {
    const { res, textSpy } = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: TEXT_PREVIEW_MAX_BYTES + 1,
      body: 'x'.repeat(10),
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'big.txt', object: 'big.txt' };
    await settle(el);

    expect(dialog(el)?.textContent).toContain('too large to preview inline');
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(true);
    expect(textSpy).not.toHaveBeenCalled();
    expect(lastFetchSignal().aborted).toBe(true);
  });

  function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((res) => {
      resolve = res;
    });
    return { promise, resolve };
  }

  it('a slow gcs image blob read for a superseded load does not clobber the newer target', async () => {
    const firstBlob = deferred<Blob>();
    const first = gcsTypedResponse({
      contentType: 'image/png',
      contentLength: 3,
      blobResult: firstBlob.promise,
    });
    const second = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: 14,
      body: 'second content',
    });
    apiFetchMock.mockResolvedValueOnce(first.res);
    apiFetchMock.mockResolvedValueOnce(second.res);
    const createObjectURL = vi.spyOn(URL, 'createObjectURL');

    try {
      const el = await mount();
      el.target = { ...GCS_JSON_TARGET, name: 'pic.png', object: 'pic.png' };
      await settle(el);
      expect(first.blobSpy).toHaveBeenCalled();
      el.target = GCS_JSON_TARGET;
      await settle(el);
      const editor = dialog(el)?.querySelector('scion-code-editor');
      expect((editor as unknown as { content: string })?.content).toBe('second content');

      firstBlob.resolve(new Blob(['x'], { type: 'image/png' }));
      await settle(el);

      const editorAfter = dialog(el)?.querySelector('scion-code-editor');
      expect((editorAfter as unknown as { content: string })?.content).toBe('second content');
      expect(dialog(el)?.querySelector('img.file-preview-image')).toBeNull();
      expect(createObjectURL).not.toHaveBeenCalled();
    } finally {
      createObjectURL.mockRestore();
    }
  });

  it('a slow gcs text body read for a superseded load does not clobber the newer target', async () => {
    const firstText = deferred<string>();
    const first = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: 13,
      textResult: firstText.promise,
    });
    const second = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: 14,
      body: 'second content',
    });
    apiFetchMock.mockResolvedValueOnce(first.res);
    apiFetchMock.mockResolvedValueOnce(second.res);

    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'first.txt', object: 'first.txt' };
    await settle(el);
    expect(first.textSpy).toHaveBeenCalled();
    el.target = GCS_JSON_TARGET;
    await settle(el);
    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe('second content');

    firstText.resolve('first content — should never be shown');
    await settle(el);

    const editorAfter = dialog(el)?.querySelector('scion-code-editor');
    expect((editorAfter as unknown as { content: string })?.content).toBe('second content');
  });

  it('shows the too-large-inline message with Download for a body over the limit when the response has no Content-Length', async () => {
    const { res, textSpy } = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      body: 'x'.repeat(TEXT_PREVIEW_MAX_BYTES + 1),
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'big.txt', object: 'big.txt' };
    await settle(el);

    expect(textSpy).toHaveBeenCalled();
    expect(dialog(el)?.textContent).toContain('too large to preview inline');
    expect(dialog(el)?.querySelector('scion-code-editor')).toBeNull();
    expect(
      Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Download')
      )
    ).toBe(true);
  });

  it('previews a body of exactly the limit inline when the response has no Content-Length', async () => {
    const body = 'x'.repeat(TEXT_PREVIEW_MAX_BYTES);
    const { res } = gcsTypedResponse({ contentType: 'text/plain; charset=utf-8', body });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_JSON_TARGET, name: 'big.txt', object: 'big.txt' };
    await settle(el);

    const editor = dialog(el)?.querySelector('scion-code-editor');
    expect((editor as unknown as { content: string })?.content).toBe(body);
  });

  it('does not show a Source toggle for an over-threshold .md object (no content was ever loaded)', async () => {
    const { res, textSpy } = gcsTypedResponse({
      contentType: 'text/plain; charset=utf-8',
      contentLength: TEXT_PREVIEW_MAX_BYTES + 1,
    });
    apiFetchMock.mockResolvedValue(res);
    const el = await mount();
    el.target = { ...GCS_MD_TARGET, name: 'huge.md', object: 'huge.md' };
    await settle(el);

    const sourceButton = Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Source')
    );
    expect(sourceButton).toBeUndefined();
    expect(textSpy).not.toHaveBeenCalled();
  });
});
