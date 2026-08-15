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
 * Tests for <scion-chat-message>: clickable @mentions and attachment previews.
 *
 * Rendered mentions carry the slug in `data-mention` and report a click as a
 * composed `mention-click` event — the message cannot resolve a slug itself,
 * only the chat page knows the member roster.
 *
 * Text attachments render as a short read-only editor slice fetched from the
 * attachment endpoint; everything else stays a download chip.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { vi } from 'vitest';

// A stand-in for marked + DOMPurify. It reproduces the shapes the mention
// post-processing has to cope with — paragraphs, fenced code, inline code and
// links — without pulling the real parser into the test.
vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (markdown: string) =>
        `<p>${markdown
          .replace(/```([\s\S]*?)```/g, '</p><pre><code>$1</code></pre><p>')
          .replace(/`([^`]+)`/g, '<code>$1</code>')
          .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" title="see $1">$1</a>')}</p>`,
    }),
}));

// The real editor pulls the CodeMirror bundle in on connect; the preview
// tests only care about what the message hands it.
vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: (path: string) => (path.endsWith('.go') ? 'go' : 'plaintext'),
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
}));

await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;
type AttachmentRefInfo = import('./chat-message.js').AttachmentRefInfo;

/** Mount a message and wait for the async markdown render to land. */
async function mount(body: string): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  document.body.appendChild(el);
  await el.updateComplete;
  // renderContent() resolves the renderer promise, then re-renders.
  await Promise.resolve();
  await el.updateComplete;
  return el;
}

function mentions(el: ScionChatMessage): HTMLElement[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.md-content .mention') ?? []);
}

describe('scion-chat-message @mentions', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders mentions as clickable spans carrying the slug', async () => {
    const el = await mount('ping @native-chat-lead about this');
    const spans = mentions(el);

    expect(spans).toHaveLength(1);
    expect(spans[0].classList.contains('clickable')).toBe(true);
    expect(spans[0].getAttribute('data-mention')).toBe('native-chat-lead');
    expect(spans[0].textContent).toBe('@native-chat-lead');
  });

  it('emits a composed mention-click with the slug when a mention is clicked', async () => {
    const el = await mount('hello @coder');
    const seen: string[] = [];
    document.addEventListener('mention-click', (e) => {
      seen.push((e as CustomEvent<{ slug: string }>).detail.slug);
    });

    mentions(el)[0].dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(seen).toEqual(['coder']);
  });

  it('leaves @mentions inside a fenced code block as literal text', async () => {
    const el = await mount('run this:\n```\ngit commit --author @coder\n```\nthanks @lead');

    // Only the mention outside the fence is a reference to anyone.
    expect(mentions(el).map((m) => m.getAttribute('data-mention'))).toEqual(['lead']);

    const pre = el.shadowRoot?.querySelector('.md-content pre');
    expect(pre?.querySelector('.mention')).toBeNull();
    expect(pre?.textContent).toContain('git commit --author @coder');
  });

  it('leaves @mentions inside an inline code span as literal text', async () => {
    const el = await mount('pass `--to @coder` when you ping @lead');

    expect(mentions(el).map((m) => m.getAttribute('data-mention'))).toEqual(['lead']);
    expect(el.shadowRoot?.querySelector('.md-content code')?.textContent).toBe('--to @coder');
  });

  it('does not rewrite @mentions sitting inside tag attributes', async () => {
    const el = await mount('see [the docs](https://example.com/@coder)');

    expect(mentions(el)).toHaveLength(0);
    const link = el.shadowRoot?.querySelector('.md-content a');
    expect(link?.getAttribute('href')).toBe('https://example.com/@coder');
    expect(link?.getAttribute('title')).toBe('see the docs');
  });

  it('styles a mention that opens the message body', async () => {
    const el = await mount('@lead please review');

    expect(mentions(el).map((m) => m.getAttribute('data-mention'))).toEqual(['lead']);
  });

  it('stays silent when the click misses a mention', async () => {
    const el = await mount('no mentions here');
    const listener = vi.fn();
    document.addEventListener('mention-click', listener);

    const content = el.shadowRoot?.querySelector('.md-content') as HTMLElement;
    content.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(listener).not.toHaveBeenCalled();
  });
});

describe('scion-chat-message attachment previews', () => {
  /** Mount a message carrying attachment refs and let the preview fetch land. */
  async function mountAttachments(refs: AttachmentRefInfo[]): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.attachmentRefs = refs;
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  /** Drain the fetch microtasks and the renders they trigger. */
  async function settle(el: ScionChatMessage): Promise<void> {
    for (let i = 0; i < 5; i++) {
      await Promise.resolve();
      await el.updateComplete;
    }
  }

  function editorIn(root: ParentNode | null | undefined): HTMLElement | null {
    return root?.querySelector('scion-code-editor') ?? null;
  }

  function respondWith(text: string): void {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve(text) });
  }

  beforeEach(() => {
    document.body.innerHTML = '';
    apiFetchMock.mockReset();
    // Without an observer the component fetches straight away, which is the
    // path these tests exercise.
    vi.stubGlobal('IntersectionObserver', undefined);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
  });

  it('previews a text attachment as a read-only editor slice', async () => {
    respondWith('package main\n\nfunc main() {}\n');
    const el = await mountAttachments([
      { id: 'att-go', name: 'main.go', mime: 'text/plain', size: 42 },
    ]);

    expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/chat/attachments/att-go', undefined);

    const preview = el.shadowRoot?.querySelector('.attachment-preview');
    expect(preview?.querySelector('.preview-filename')?.textContent).toBe('main.go');

    const editor = editorIn(preview);
    expect(editor).not.toBeNull();
    expect(editor?.hasAttribute('readonly')).toBe(true);
    expect(editor?.getAttribute('language')).toBe('go');
    expect((editor as unknown as { content: string }).content).toBe(
      'package main\n\nfunc main() {}\n'
    );
  });

  it('clips the slice to the first lines of the file', async () => {
    const lines = Array.from({ length: 60 }, (_, i) => `line ${i + 1}`);
    respondWith(lines.join('\n'));
    const el = await mountAttachments([
      { id: 'att-long', name: 'notes.txt', mime: 'text/plain', size: 600 },
    ]);

    const editor = editorIn(el.shadowRoot?.querySelector('.attachment-preview'));
    const shown = (editor as unknown as { content: string }).content.split('\n');
    expect(shown).toHaveLength(40);
    expect(shown[39]).toBe('line 40');
  });

  it('expands to an overlay holding the whole file', async () => {
    const lines = Array.from({ length: 60 }, (_, i) => `line ${i + 1}`);
    respondWith(lines.join('\n'));
    const el = await mountAttachments([
      { id: 'att-expand', name: 'notes.txt', mime: 'text/plain', size: 600 },
    ]);

    expect(el.shadowRoot?.querySelector('sl-dialog')).toBeNull();

    const expand = el.shadowRoot?.querySelector(
      'sl-icon-button[name="arrows-angle-expand"]'
    ) as HTMLElement;
    expand.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const dialog = el.shadowRoot?.querySelector('sl-dialog.full-preview');
    expect(dialog?.getAttribute('label')).toBe('notes.txt');
    expect((editorIn(dialog) as unknown as { content: string }).content.split('\n')).toHaveLength(
      60
    );
    expect(dialog?.querySelector('sl-button')?.getAttribute('href')).toBe(
      '/api/v1/chat/attachments/att-expand'
    );
  });

  it('offers a download link beside every preview', async () => {
    respondWith('hello');
    const el = await mountAttachments([
      { id: 'att-dl', name: 'hello.txt', mime: 'text/plain', size: 5 },
    ]);

    const download = el.shadowRoot?.querySelector(
      '.preview-actions sl-icon-button[name="download"]'
    );
    expect(download?.getAttribute('href')).toBe('/api/v1/chat/attachments/att-dl');
    expect(download?.getAttribute('download')).toBe('hello.txt');
  });

  it('leaves binary and oversized attachments as download chips', async () => {
    const el = await mountAttachments([
      { id: 'att-zip', name: 'bundle.zip', mime: 'application/zip', size: 1024 },
      { id: 'att-pdf', name: 'report.pdf', mime: 'application/pdf', size: 1024 },
      { id: 'att-huge', name: 'huge.log', mime: 'text/plain', size: 5 * 1024 * 1024 },
    ]);

    expect(el.shadowRoot?.querySelectorAll('.attachment-preview')).toHaveLength(0);
    expect(el.shadowRoot?.querySelectorAll('.download-chip')).toHaveLength(3);
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('expands an image into the overlay rather than a new tab', async () => {
    const el = await mountAttachments([
      { id: 'att-img', name: 'shot.png', mime: 'image/png', size: 2048 },
    ]);

    // No anchor around the thumbnail — the click stays in the page.
    expect(el.shadowRoot?.querySelector('.attachment-images a')).toBeNull();
    expect(el.shadowRoot?.querySelector('sl-dialog')).toBeNull();

    const button = el.shadowRoot?.querySelector('.image-expand') as HTMLElement;
    expect(button.querySelector('img.attachment-image')?.getAttribute('src')).toBe(
      '/api/v1/chat/attachments/att-img'
    );

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const dialog = el.shadowRoot?.querySelector('sl-dialog.full-preview');
    expect(dialog?.getAttribute('label')).toBe('shot.png');
    expect(dialog?.querySelector('img.full-image')?.getAttribute('src')).toBe(
      '/api/v1/chat/attachments/att-img'
    );
    // Images are rendered by the browser; nothing is fetched as text.
    expect(editorIn(dialog)).toBeNull();
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('gives an image thumbnail an expand and a download action', async () => {
    const el = await mountAttachments([
      { id: 'att-img', name: 'shot.png', mime: 'image/png', size: 2048 },
    ]);

    const actions = el.shadowRoot?.querySelector('.image-preview-wrapper .image-actions');
    expect(actions).not.toBeNull();

    const download = actions?.querySelector('sl-icon-button[name="download"]');
    expect(download?.getAttribute('href')).toBe('/api/v1/chat/attachments/att-img');
    expect(download?.getAttribute('download')).toBe('shot.png');

    // The toolbar's expand opens the same overlay the thumbnail does.
    const expand = actions?.querySelector(
      'sl-icon-button[name="arrows-angle-expand"]'
    ) as HTMLElement;
    expand.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const dialog = el.shadowRoot?.querySelector('sl-dialog.full-preview');
    expect(dialog?.getAttribute('label')).toBe('shot.png');
    expect(dialog?.querySelector('img.full-image')?.getAttribute('src')).toBe(
      '/api/v1/chat/attachments/att-img'
    );
  });

  it('closes the overlay when it is dismissed, so a click outside ends it', async () => {
    const el = await mountAttachments([
      { id: 'att-img', name: 'shot.png', mime: 'image/png', size: 2048 },
    ]);

    const button = el.shadowRoot?.querySelector('.image-expand') as HTMLElement;
    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const dialog = el.shadowRoot?.querySelector('sl-dialog.full-preview') as HTMLElement;
    expect(dialog).not.toBeNull();

    // sl-dialog closes itself on an overlay click and reports sl-after-hide.
    dialog.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));
    await settle(el);

    expect(el.shadowRoot?.querySelector('sl-dialog')).toBeNull();
  });

  it('reports a failed fetch inside the preview instead of an empty editor', async () => {
    apiFetchMock.mockResolvedValue({ ok: false, status: 404, text: () => Promise.resolve('') });
    const el = await mountAttachments([
      { id: 'att-gone', name: 'gone.txt', mime: 'text/plain', size: 12 },
    ]);

    const preview = el.shadowRoot?.querySelector('.attachment-preview');
    expect(editorIn(preview)).toBeNull();
    expect(preview?.querySelector('.preview-placeholder.error')?.textContent).toContain('404');
  });
});
