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

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { vi } from 'vitest';
import { setPreferredTimeZone } from '../../../utils/time.js';

// A stand-in for marked + DOMPurify. It reproduces the shapes the mention
// post-processing has to cope with — paragraphs, fenced code, inline code and
// links — without pulling the real parser into the test.
vi.mock('../../../utils/markdown.js', () => {
  const render = (markdown: string): string =>
    `<p>${markdown
      .replace(/```([\s\S]*?)```/g, '</p><pre><code>$1</code></pre><p>')
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" title="see $1">$1</a>')}</p>`;
  return {
    getMarkdownRenderer: () =>
      Promise.resolve({
        render,
        renderFragment: (markdown: string) => {
          const tpl = document.createElement('template');
          tpl.innerHTML = render(markdown);
          return tpl.content;
        },
      }),
  };
});

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

  function respondWithImage(): void {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(new Blob(['fake-bytes'], { type: 'image/png' })),
    });
  }

  /**
   * The full-attachment overlay is the reusable `<scion-chat-file-preview>`
   * — a nested shadow root, with its own async load. Drain both shadow
   * roots' microtasks/renders.
   */
  async function previewDialog(el: ScionChatMessage): Promise<HTMLElement | null> {
    for (let i = 0; i < 8; i++) {
      const preview = el.shadowRoot?.querySelector('scion-chat-file-preview') as
        | (HTMLElement & { updateComplete: Promise<boolean> })
        | null;
      if (!preview) {
        await Promise.resolve();
        continue;
      }
      await preview.updateComplete;
      await Promise.resolve();
    }
    const preview = el.shadowRoot?.querySelector('scion-chat-file-preview');
    return (
      (preview?.shadowRoot?.querySelector('sl-dialog.file-preview-dialog') as HTMLElement) ?? null
    );
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

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/attachments/att-go?stream=1',
      undefined
    );

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

    expect(el.shadowRoot?.querySelector('scion-chat-file-preview')).toBeNull();

    const expand = el.shadowRoot?.querySelector(
      'sl-icon-button[name="arrows-angle-expand"]'
    ) as HTMLElement;
    expand.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);
    const dialog = await previewDialog(el);

    expect(dialog?.getAttribute('label')).toBe('notes.txt');
    expect((editorIn(dialog) as unknown as { content: string }).content.split('\n')).toHaveLength(
      60
    );
    expect(dialog?.querySelector('sl-button[href]')?.getAttribute('href')).toBe(
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
    respondWithImage();
    const el = await mountAttachments([
      { id: 'att-img', name: 'shot.png', mime: 'image/png', size: 2048 },
    ]);
    apiFetchMock.mockClear();

    // No anchor around the thumbnail — the click stays in the page.
    expect(el.shadowRoot?.querySelector('.attachment-images a')).toBeNull();
    expect(el.shadowRoot?.querySelector('scion-chat-file-preview')).toBeNull();

    const button = el.shadowRoot?.querySelector('.image-expand') as HTMLElement;
    expect(button.querySelector('img.attachment-image')?.getAttribute('src')).toBe(
      '/api/v1/chat/attachments/att-img'
    );

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);
    const dialog = await previewDialog(el);

    expect(dialog?.getAttribute('label')).toBe('shot.png');
    // The overlay fetches the image itself (for uniform 403/404 handling)
    // and renders it from an object URL, not the bare attachment URL.
    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/attachments/att-img?view=true&stream=1',
      expect.anything()
    );
    const img = dialog?.querySelector('img.file-preview-image');
    expect(img?.getAttribute('src')).toMatch(/^blob:/);
    // Nothing is fetched as text for an image.
    expect(editorIn(dialog)).toBeNull();
  });

  it('gives an image thumbnail an expand and a download action', async () => {
    respondWithImage();
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
    const dialog = await previewDialog(el);

    expect(dialog?.getAttribute('label')).toBe('shot.png');
    expect(dialog?.querySelector('img.file-preview-image')?.getAttribute('src')).toMatch(/^blob:/);
  });

  it('closes the overlay when it is dismissed, so a click outside ends it', async () => {
    const el = await mountAttachments([
      { id: 'att-img', name: 'shot.png', mime: 'image/png', size: 2048 },
    ]);

    const button = el.shadowRoot?.querySelector('.image-expand') as HTMLElement;
    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);
    const dialog = await previewDialog(el);
    expect(dialog).not.toBeNull();

    // sl-dialog closes itself on an overlay click and reports sl-after-hide.
    dialog!.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));
    await settle(el);

    expect(el.shadowRoot?.querySelector('scion-chat-file-preview')).toBeNull();
  });

  it('does not refetch or reset the overlay on an unrelated chat-message re-render', async () => {
    respondWith('package main\n\nfunc main() {}\n');
    const el = await mountAttachments([
      { id: 'att-stable', name: 'main.go', mime: 'text/plain', size: 42 },
    ]);

    const expand = el.shadowRoot?.querySelector(
      'sl-icon-button[name="arrows-angle-expand"]'
    ) as HTMLElement;
    expand.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);
    await previewDialog(el);

    apiFetchMock.mockClear();

    // A re-render triggered by something unrelated to the overlay (a
    // reaction, a read receipt, an SSE edit, the parent thread re-rendering)
    // must not rebuild the preview target and refetch it.
    el.requestUpdate();
    await settle(el);

    expect(apiFetchMock).not.toHaveBeenCalled();
    expect(el.shadowRoot?.querySelector('scion-chat-file-preview')).not.toBeNull();
  });

  it('keeps the Source/Rendered toggle across an unrelated chat-message re-render', async () => {
    respondWith('# Heading\n\nBody text.\n');
    const el = await mountAttachments([
      { id: 'att-md', name: 'notes.md', mime: 'text/markdown', size: 30 },
    ]);

    const expand = el.shadowRoot?.querySelector(
      'sl-icon-button[name="arrows-angle-expand"]'
    ) as HTMLElement;
    expand.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);
    const dialog = await previewDialog(el);

    const sourceButton = Array.from(dialog?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Source')
    ) as HTMLElement;
    expect(sourceButton).toBeDefined();
    sourceButton.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle(el);

    const preview = el.shadowRoot?.querySelector('scion-chat-file-preview');
    expect(
      Array.from(preview?.shadowRoot?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Preview')
      )
    ).toBe(true);

    // An unrelated parent re-render must not reset the toggle back to the
    // rendered view.
    el.requestUpdate();
    await settle(el);

    expect(
      Array.from(preview?.shadowRoot?.querySelectorAll('sl-button') ?? []).some((b) =>
        b.textContent?.includes('Preview')
      )
    ).toBe(true);
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

// ---------------------------------------------------------------------------
// Path-link entity patterns (#1148)
// ---------------------------------------------------------------------------

describe('scion-chat-message path links', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  function pathLinks(el: ScionChatMessage): HTMLElement[] {
    return Array.from(el.shadowRoot?.querySelectorAll('.md-content .path-link') ?? []);
  }

  it('renders /workspace/... paths as clickable path links', async () => {
    const el = await mount('check /workspace/src/main.go for details');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/workspace/src/main.go');
    expect(links[0].textContent).toBe('/workspace/src/main.go');
    expect(links[0].classList.contains('entity-link')).toBe(true);
    expect(links[0].classList.contains('path-link')).toBe(true);
  });

  it('renders /scion-volumes/... paths as clickable path links', async () => {
    const el = await mount('see /scion-volumes/scratchpad/reports/summary.md');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/scion-volumes/scratchpad/reports/summary.md');
  });

  it('renders /workspace/.scion-volumes/... paths as clickable path links', async () => {
    const el = await mount('read /workspace/.scion-volumes/data/output.json');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/workspace/.scion-volumes/data/output.json');
  });

  it('does not link directory paths without a file extension', async () => {
    const el = await mount('look in /workspace/src/components for the code');
    const links = pathLinks(el);

    expect(links).toHaveLength(0);
  });

  it('does not link directory paths (no file extension)', async () => {
    const el = await mount('check /scion-volumes/scratchpad/projects for details');
    const links = pathLinks(el);

    expect(links).toHaveLength(0);
  });

  it('stops at spaces (paths with spaces are not linkable)', async () => {
    // Spaces are ambiguous in prose — the regex stops at whitespace.
    const el = await mount('see /workspace/my-file.txt here');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/workspace/my-file.txt');
  });

  it('does not match arbitrary absolute paths like /etc/passwd', async () => {
    const el = await mount('file at /etc/passwd is dangerous');
    const links = pathLinks(el);

    expect(links).toHaveLength(0);
  });

  it('does not match relative paths', async () => {
    const el = await mount('look at src/main.go for the code');
    const links = pathLinks(el);

    expect(links).toHaveLength(0);
  });

  it('leaves path links inside fenced code blocks as literal text', async () => {
    const el = await mount(
      'run:\n```\ncat /workspace/src/main.go\n```\nthen check /workspace/README.md'
    );
    const links = pathLinks(el);

    // Only the one outside the fence should be linked.
    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/workspace/README.md');
  });

  it('emits a composed path-link-click event when a path link is clicked', async () => {
    const el = await mount('see /workspace/src/main.go');
    const seen: string[] = [];
    document.addEventListener('path-link-click', (e) => {
      seen.push((e as CustomEvent<{ path: string }>).detail.path);
    });

    const link = pathLinks(el)[0];
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(seen).toEqual(['/workspace/src/main.go']);
  });

  it('gives path links a "#" href and keeps the click on the page', async () => {
    const el = await mount('see /workspace/src/main.go');
    const link = pathLinks(el)[0];
    // Positive control: the link exists, so the href check is not vacuous.
    expect(link).toBeDefined();
    expect(link.getAttribute('href')).toBe('#');

    const click = new MouseEvent('click', { bubbles: true, composed: true, cancelable: true });
    link.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(true);
  });

  it('keeps a middle-click on a path link from opening a tab', async () => {
    const el = await mount('see /workspace/src/main.go and [the docs](https://example.com/docs)');
    const link = pathLinks(el)[0];
    expect(link).toBeDefined();
    const aux = new MouseEvent('auxclick', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 1,
    });
    link.dispatchEvent(aux);
    expect(aux.defaultPrevented).toBe(true);

    // Positive control: a middle-click on an ordinary external link is left alone.
    const external = el.shadowRoot?.querySelector<HTMLAnchorElement>(
      '.md-content a[href="https://example.com/docs"]'
    );
    expect(external).toBeTruthy();
    const externalAux = new MouseEvent('auxclick', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 1,
    });
    external?.dispatchEvent(externalAux);
    expect(externalAux.defaultPrevented).toBe(false);
  });

  it('does not include trailing sentence punctuation in path links', async () => {
    const el = await mount(
      'results are in /scion-volumes/scratchpad/final-summary.md. Check them.'
    );
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/scion-volumes/scratchpad/final-summary.md');
    expect(links[0].textContent).toBe('/scion-volumes/scratchpad/final-summary.md');
  });

  it('renders multiple path links in the same message', async () => {
    const el = await mount('compare /workspace/a.ts and /scion-volumes/data/b.ts');
    const links = pathLinks(el);

    expect(links).toHaveLength(2);
    expect(links[0].dataset.filePath).toBe('/workspace/a.ts');
    expect(links[1].dataset.filePath).toBe('/scion-volumes/data/b.ts');
  });

  it('links file paths inside backtick code spans', async () => {
    const el = await mount('see `/scion-volumes/scratchpad/report.md` for results');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/scion-volumes/scratchpad/report.md');
  });

  it('links extensionless known filenames like Makefile and Dockerfile', async () => {
    const el = await mount('see /workspace/src/Makefile for build targets');
    const links = pathLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].dataset.filePath).toBe('/workspace/src/Makefile');
  });

  it('does not double-link paths already inside markdown links', async () => {
    // The mocked renderer turns `[text](url)` into `<a href="url">text</a>`.
    // Using the path as both the link text and the URL means the path
    // string appears inside the anchor's text content — exactly the case
    // that would produce a nested <a> tag if path-linking ran unconditionally
    // on that text.
    const el = await mount(
      'check [/scion-volumes/data/report.md](/scion-volumes/data/report.md) for details'
    );
    const links = pathLinks(el);

    // The markdown link produces one <a>; the path inside should NOT be
    // re-wrapped in a nested path-link.
    expect(links).toHaveLength(0);

    const anchor = el.shadowRoot?.querySelector('.md-content a');
    expect(anchor?.querySelector('a')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// GitHub shortform issue/PR reference links (owner/repo#N)
// ---------------------------------------------------------------------------

describe('scion-chat-message GitHub shortform refs', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  function ghRefLinks(el: ScionChatMessage): HTMLAnchorElement[] {
    return Array.from(
      el.shadowRoot?.querySelectorAll('.md-content .gh-ref-link') ?? []
    ) as HTMLAnchorElement[];
  }

  it('renders owner/repo#N as a link to the GitHub issue page', async () => {
    const el = await mount('see ptone/scion#2217 for details');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe('https://github.com/ptone/scion/issues/2217');
    expect(links[0].textContent).toBe('ptone/scion#2217');
    expect(links[0].getAttribute('target')).toBe('_blank');
    expect(links[0].getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('excludes trailing sentence punctuation from the match', async () => {
    const el = await mount('fixed in ptone/scion#2217. Thanks!');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2217');
    expect(links[0].getAttribute('href')).toBe('https://github.com/ptone/scion/issues/2217');
  });

  it('excludes surrounding parentheses from the match', async () => {
    const el = await mount('see the fix (ptone/scion#2217) for context');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2217');
  });

  it('does not double-link a ref already inside an existing link', async () => {
    // The mocked renderer turns `[text](url)` into `<a href="url">text</a>`.
    const el = await mount(
      'see [ptone/scion#2217](https://github.com/ptone/scion/issues/2217) for details'
    );
    const links = ghRefLinks(el);

    expect(links).toHaveLength(0);
    const anchor = el.shadowRoot?.querySelector('.md-content a');
    expect(anchor?.querySelector('a')).toBeNull();
  });

  it('does not link a ref embedded in a URL path (preceded by /)', async () => {
    const el = await mount('see https://example.com/ptone/scion#2217 for details');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(0);
  });

  it('does not link the tail of a longer slash-separated path', async () => {
    // Neither the whole thing nor the `b/c#12` tail is a valid ref: a repo
    // can never contain `/`, and `b`/`c` are each preceded by `/`.
    const el = await mount('path is a/b/c#12 in the tree');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(0);
  });

  it('leaves refs inside an inline code span as literal text', async () => {
    const el = await mount('run `git log ptone/scion#2217` to check');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(0);
    expect(el.shadowRoot?.querySelector('.md-content code')?.textContent).toBe(
      'git log ptone/scion#2217'
    );
  });

  it('leaves refs inside a fenced code block as literal text', async () => {
    const el = await mount('run:\n```\necho ptone/scion#2217\n```\nthen see ptone/scion#2218');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2218');
    const pre = el.shadowRoot?.querySelector('.md-content pre');
    expect(pre?.querySelector('.gh-ref-link')).toBeNull();
  });

  it('does not link a bare #123 with no owner/repo', async () => {
    const el = await mount('see #123 for details');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(0);
  });

  it('links alongside an adjacent file path without interference', async () => {
    const el = await mount('check /workspace/src/main.go and ptone/scion#2217');
    const links = ghRefLinks(el);
    const paths = Array.from(
      el.shadowRoot?.querySelectorAll('.md-content .path-link') ?? []
    ) as HTMLElement[];

    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2217');
    expect(paths).toHaveLength(1);
    expect(paths[0].dataset.filePath).toBe('/workspace/src/main.go');
  });

  it('renders multiple refs in the same message', async () => {
    const el = await mount('see ptone/scion#2217 and GoogleCloudPlatform/scion#2081');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(2);
    expect(links[0].getAttribute('href')).toBe('https://github.com/ptone/scion/issues/2217');
    expect(links[1].getAttribute('href')).toBe(
      'https://github.com/GoogleCloudPlatform/scion/issues/2081'
    );
  });

  it('links a mixed-case owner', async () => {
    const el = await mount('see PTone/Scion#42 for details');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe('https://github.com/PTone/Scion/issues/42');
    expect(links[0].textContent).toBe('PTone/Scion#42');
  });

  it('does not link when a word character trails the number (R1)', async () => {
    const el = await mount('see foo/bar#12abc for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link when an underscore trails the number (R1)', async () => {
    const el = await mount('see foo/bar#12_x for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link a scheme-less host like example.com/foo#12 (O1)', async () => {
    const el = await mount('see example.com/foo#12 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link a dotted prefix like user.name/repo#1 (O1)', async () => {
    const el = await mount('see user.name/repo#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link mid-word after a non-ASCII prefix like äptone/scion#1 (O1)', async () => {
    const el = await mount('see äptone/scion#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('rejects a repo of just dots, e.g. ptone/.#1 (O2)', async () => {
    const el = await mount('see ptone/.#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('rejects a repo of just dots, e.g. ptone/..#1 (O2)', async () => {
    const el = await mount('see ptone/..#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('links a 39-character owner, the GitHub max length (O3)', async () => {
    const owner = 'a'.repeat(39);
    const el = await mount(`see ${owner}/repo#1 for details`);
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe(`https://github.com/${owner}/repo/issues/1`);
  });

  it('does not link a 40-character owner, one past the GitHub max length (O3)', async () => {
    const owner = 'a'.repeat(40);
    const el = await mount(`see ${owner}/repo#1 for details`);
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('links a repo that starts with a dot, e.g. ptone/.github#1 (round 2 R1)', async () => {
    const el = await mount('see ptone/.github#1 for details');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe('https://github.com/ptone/.github/issues/1');
    expect(links[0].textContent).toBe('ptone/.github#1');
  });

  it('does not slide past a hyphen after the dot exclusion blocks it (N1)', async () => {
    const el = await mount('see user.foo-bar/repo#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link when a trailing unicode letter follows the number (N2)', async () => {
    const el = await mount('see ptone/scion#12é for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('does not link when the owner is preceded by an underscore (N2)', async () => {
    const el = await mount('see _ptone/scion#1 for details');
    expect(ghRefLinks(el)).toHaveLength(0);
  });

  it('re-emits the boundary character unchanged (round 3 R1)', async () => {
    const el = await mount('x (a/b#1),c/d#2 e/f#3');
    expect(ghRefLinks(el).map((a) => a.textContent)).toEqual(['a/b#1', 'c/d#2', 'e/f#3']);
    expect(el.shadowRoot?.querySelector('.md-content p')?.textContent).toBe(
      'x (a/b#1),c/d#2 e/f#3'
    );
  });

  it('links a ref at the very start of the message (round 3 R2)', async () => {
    const el = await mount('ptone/scion#2217 is fixed');
    const links = ghRefLinks(el);

    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2217');
  });
});

describe('scion-chat-message cross-project label', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('shows cross-project label when senderProjectSlug is set', async () => {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'Hello from another project';
    el.sender = 'agent:remote-bot';
    el.senderName = 'remote-bot';
    el.fromAgent = true;
    el.showHeader = true;
    el.senderProjectSlug = 'other-project';
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;

    const label = el.shadowRoot?.querySelector('.cross-project-label');
    expect(label).toBeTruthy();
    expect(label?.textContent).toContain('other-project');
  });

  it('hides cross-project label when senderProjectSlug is empty', async () => {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'Hello from same project';
    el.sender = 'agent:local-bot';
    el.senderName = 'local-bot';
    el.fromAgent = true;
    el.showHeader = true;
    el.senderProjectSlug = '';
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;

    const label = el.shadowRoot?.querySelector('.cross-project-label');
    expect(label).toBeNull();
  });
});

// A message sent with an agent delegated credential names the agent as the
// sender and the issuer as "authorized by" (.design/agent-delegation.md
// §14.5). Ordinary messages show no such line.
describe('scion-chat-message authorized-by line', () => {
  const alice = { id: 'u-alice', displayName: 'Alice Example', email: 'alice@example.com' };
  const bob = { id: 'u-bob', displayName: '', email: 'bob@example.com' };
  const found = (user: object) => ({ ok: true, status: 200, json: () => Promise.resolve(user) });
  const notFound = { ok: false, status: 404, json: () => Promise.resolve({}) };
  const forbidden = { ok: false, status: 403, json: () => Promise.resolve({}) };
  const serverError = { ok: false, status: 500, json: () => Promise.resolve({}) };

  beforeEach(async () => {
    document.body.innerHTML = '';
    apiFetchMock.mockReset();
    const { resetUserDirectory } = await import('../../../client/user-directory.js');
    resetUserDirectory();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function mountFromAgent(
    authorizingUserId: string,
    grant: string,
    showHeader = true
  ): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'build is green';
    el.sender = 'agent:builder';
    el.senderName = 'builder';
    el.fromAgent = true;
    el.showHeader = showHeader;
    el.authorizingUserId = authorizingUserId;
    el.delegationGrantId = grant;
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;
    return el;
  }

  function lineText(el: ScionChatMessage): string | undefined {
    return el.shadowRoot?.querySelector('.authorized-by')?.textContent?.trim();
  }

  it('shows the agent as sender and the issuer by display name', async () => {
    apiFetchMock.mockResolvedValue(found(alice));
    const el = await mountFromAgent('u-alice', 'grant-1');

    expect(el.shadowRoot?.querySelector('.sender-name')?.textContent).toContain('builder');
    await vi.waitFor(() => expect(lineText(el)).toBe('authorized by Alice Example'));
    const line = el.shadowRoot?.querySelector('.authorized-by');
    expect(line?.getAttribute('title')).toBe('via delegation grant-1');
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(apiFetchMock.mock.calls[0]?.[0]).toBe('/api/v1/users/u-alice');
  });

  it('falls back to the email, then the ID', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(path === '/api/v1/users/u-bob' ? found(bob) : notFound)
    );
    const bobMsg = await mountFromAgent('u-bob', 'grant-2');
    await vi.waitFor(() => expect(lineText(bobMsg)).toBe('authorized by bob@example.com'));

    const goneMsg = await mountFromAgent('u-gone', 'grant-3');
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    await Promise.resolve();
    await goneMsg.updateComplete;
    expect(lineText(goneMsg)).toBe('authorized by u-gone');
  });

  it('shows the ID when the lookup is forbidden or fails', async () => {
    apiFetchMock.mockImplementation((path: string) =>
      Promise.resolve(path === '/api/v1/users/u-denied' ? forbidden : serverError)
    );
    const denied = await mountFromAgent('u-denied', 'grant-4');
    const broken = await mountFromAgent('u-broken', 'grant-5');
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    await Promise.resolve();
    await denied.updateComplete;
    await broken.updateComplete;

    expect(lineText(denied)).toBe('authorized by u-denied');
    expect(lineText(broken)).toBe('authorized by u-broken');
    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual([
      '/api/v1/users/u-denied',
      '/api/v1/users/u-broken',
    ]);
  });

  it('shows the line on a grouped message with no header', async () => {
    apiFetchMock.mockResolvedValue(notFound);
    const el = await mountFromAgent('u-alice', '', false);

    expect(el.shadowRoot?.querySelector('.bubble-header')).toBeNull();
    expect(lineText(el)).toBe('authorized by u-alice');
    expect(el.shadowRoot?.querySelector('.authorized-by')?.hasAttribute('title')).toBe(false);
  });

  it('shows nothing and looks up no user on an ordinary message', async () => {
    const el = await mountFromAgent('', '');

    expect(el.shadowRoot?.querySelector('.sender-name')?.textContent).toContain('builder');
    expect(el.shadowRoot?.querySelector('.authorized-by')).toBeNull();
    expect(apiFetchMock).not.toHaveBeenCalled();
  });
});

// nc-delivery-unreachable: "Agent unreachable" replaces the generic "Failed"
// label when the primary agent could not receive the message at all, either
// via the machine-readable code (new sends) or the reason prefix (history
// rows sent before the code field existed).
describe('scion-chat-message delivery state', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function mountOutbound(props: Partial<ScionChatMessage>): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'hello';
    el.fromAgent = false;
    Object.assign(el, props);
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;
    return el;
  }

  function deliveryState(el: ScionChatMessage): Element | null | undefined {
    return el.shadowRoot?.querySelector('.delivery-state');
  }

  it('shows "Waking agent…" while a wake-and-send is in flight', async () => {
    const el = await mountOutbound({ dispatchState: 'waking' });
    const state = deliveryState(el);
    expect(state?.textContent).toContain('Waking agent');
    expect(state?.classList.contains('waking')).toBe(true);
  });

  it('shows "Agent unreachable" when dispatchFailureCode is agent_unreachable', async () => {
    const el = await mountOutbound({
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable (suspended)',
      dispatchFailureCode: 'agent_unreachable',
    });

    const state = deliveryState(el);
    expect(state?.textContent).toContain('Agent unreachable');
    expect(state?.classList.contains('failed')).toBe(true);
    const tooltip = el.shadowRoot?.querySelector('sl-tooltip');
    expect(tooltip?.getAttribute('content')).toBe('Agent unreachable (suspended)');
  });

  it('falls back to matching the reason prefix for history rows without a code', async () => {
    const el = await mountOutbound({
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable',
      dispatchFailureCode: '',
    });

    const state = deliveryState(el);
    expect(state?.textContent).toContain('Agent unreachable');
  });

  it('keeps the generic "Failed" label for a non-unreachable dispatch error', async () => {
    const el = await mountOutbound({
      dispatchState: 'failed',
      dispatchFailureReason: "agent 'x' not found or not running",
      dispatchFailureCode: 'dispatch_error',
    });

    const state = deliveryState(el);
    expect(state?.textContent).toContain('Failed');
    expect(state?.textContent).not.toContain('Agent unreachable');
  });

  it('keeps the generic "Failed" label when neither code nor reason indicate unreachable', async () => {
    const el = await mountOutbound({
      dispatchState: 'failed',
      dispatchFailureReason: "agent 'x' not found or not running",
      dispatchFailureCode: '',
    });

    const state = deliveryState(el);
    expect(state?.textContent).toContain('Failed');
    expect(state?.textContent).not.toContain('Agent unreachable');
  });

  // ptone/scion#4480: a dispatched message was accepted by the agent's
  // runtime, not confirmed read, so the receipt never claims "Delivered".
  it('shows "Accepted" for dispatchState=dispatched', async () => {
    const el = await mountOutbound({
      dispatchState: 'dispatched',
    });

    const state = deliveryState(el);
    expect(state).toBeTruthy();
    expect(state?.classList.contains('dispatched')).toBe(true);
    expect(state?.textContent).toContain('Accepted');
    expect(state?.textContent).not.toContain('Delivered');
  });

  // F5 (p2a-r2 review): design agent-reincarnate §3.7 — a message to a
  // migrating agent must show a receipt distinct from both "Accepted" and
  // "Failed", telling the sender it was saved for catch-up.
  it('shows a distinct "Deferred" indicator for dispatchState=deferred', async () => {
    const el = await mountOutbound({
      dispatchState: 'deferred',
    });

    const state = deliveryState(el);
    expect(state).toBeTruthy();
    expect(state?.classList.contains('deferred')).toBe(true);
    expect(state?.classList.contains('failed')).toBe(false);
    expect(state?.textContent).toContain('Deferred');
    expect(state?.textContent).toContain('reincarnating');
    const icon = state?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('pause-circle');
  });

  it('shows a "Not delivered to any agent" hint for dispatchState=no_recipient', async () => {
    const el = await mountOutbound({
      dispatchState: 'no_recipient',
    });

    const state = deliveryState(el);
    expect(state).toBeTruthy();
    expect(state?.classList.contains('no-recipient')).toBe(true);
    expect(state?.classList.contains('dispatched')).toBe(false);
    expect(state?.textContent).toContain('Not delivered to any agent');
    expect(state?.textContent).toContain('mention an agent');
    const icon = state?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('info-circle');
  });
});

declare global {
  interface Window {
    __SCION_FEATURES__?: Record<string, boolean>;
  }
}

describe('scion-chat-message gs:// linkification', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    window.__SCION_FEATURES__ = { 'web.gcs_links': true };
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  /**
   * `senderIsAgent` defaults to `fromAgent` for every existing call site,
   * where the two coincide (either both true, an agent's own message, or
   * both false, the viewer's own message). A caller wanting to test the
   * distinction directly — v2's "not me" `fromAgent` layout heuristic vs.
   * the real sender kind gs:// linkification must gate on — passes it
   * explicitly.
   */
  async function mountGcs(
    body: string,
    fromAgent: boolean,
    senderIsAgent: boolean = fromAgent
  ): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = body;
    el.fromAgent = fromAgent;
    el.senderIsAgent = senderIsAgent;
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;
    return el;
  }

  function gcsLinks(el: ScionChatMessage): HTMLElement[] {
    return Array.from(el.shadowRoot?.querySelectorAll('.md-content .gcs-link') ?? []);
  }

  it('links a gs:// URI in an agent message', async () => {
    const el = await mountGcs('see gs://bkt/dir/file.md', true);
    const links = gcsLinks(el);
    expect(links).toHaveLength(1);
    expect(links[0].dataset.gcsUri).toBe('gs://bkt/dir/file.md');
    expect(links[0].classList.contains('entity-link')).toBe(true);
  });

  it('links the cross-project-exchange URI as a single gcs link and no path link', async () => {
    const el = await mountGcs('gs://scion-xproject-exchange/workspace-volumes/dev-brief.md', true);
    expect(gcsLinks(el)).toHaveLength(1);
    expect(el.shadowRoot?.querySelectorAll('.md-content .path-link')).toHaveLength(0);
    expect(gcsLinks(el)[0].dataset.gcsUri).toBe(
      'gs://scion-xproject-exchange/workspace-volumes/dev-brief.md'
    );
  });

  it('links a gs:// URI inside inline code', async () => {
    const el = await mountGcs('run `gs://bkt/o.txt` now', true);
    expect(gcsLinks(el)).toHaveLength(1);
  });

  it('does not link inside a fenced code block', async () => {
    const el = await mountGcs('```\ngs://bkt/o.txt\n```', true);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('does not link a directory-like trailing slash', async () => {
    const el = await mountGcs('gs://bkt/dir/', true);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('does not link a scheme-prefixed non-match', async () => {
    const el = await mountGcs('xgs://bkt/o', true);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('does not link the same text in a user-sent message', async () => {
    const el = await mountGcs('gs://bkt/dir/file.md', false);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('does not link another user\'s message even when v2 renders it as "not me" (fromAgent=true)', async () => {
    // v2's fromAgent means "not the viewer" for layout — true for both an
    // agent's message and another user's message in the same topic.
    // Linkification must gate on the real sender kind, not that heuristic.
    const el = await mountGcs('gs://bkt/dir/file.md', true, false);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('links an agent message rendered the same way (fromAgent=true, senderIsAgent=true) as the positive control', async () => {
    const el = await mountGcs('gs://bkt/dir/file.md', true, true);
    expect(gcsLinks(el)).toHaveLength(1);
  });

  it('does not link when the web.gcs_links experiment is off, even for an agent message', async () => {
    window.__SCION_FEATURES__ = { 'web.gcs_links': false };
    const el = await mountGcs('gs://bkt/dir/file.md', true);
    expect(gcsLinks(el)).toHaveLength(0);
  });

  it('links a gs:// URI at the very start of the body', async () => {
    const el = await mountGcs('gs://bkt/o.txt leads the message', true);
    expect(gcsLinks(el)).toHaveLength(1);
  });

  it('produces no extra attributes or elements for a hostile name (quote/onmouseover)', async () => {
    const el = await mountGcs('gs://bkt/a"onmouseover=alert(1)', true);
    const links = gcsLinks(el);
    // The regex stops the object capture before the quote, so the link
    // that renders is for "a", not the full hostile string, and no
    // onmouseover attribute or handler is ever created.
    expect(links).toHaveLength(1);
    expect(links[0].dataset.gcsUri).toBe('gs://bkt/a');
    expect(links[0].getAttribute('onmouseover')).toBeNull();
  });

  // A hostile `<img>`/`<script>` name needs the real sanitizing renderer
  // (marked + DOMPurify) to HTML-escape the raw `<`/`>` before this pass
  // ever sees them; this file's markdown mock renders raw markdown into
  // `<p>` without that escaping, so it cannot validly exercise this case.
  // Covered instead by chat-file-links.test.ts's regex-boundary unit test
  // (the object capture stops before `<`) and by the real-Chromium spec in
  // web/e2e/chat-file-preview/.

  it('emits a composed gcs-link-click event with bucket, object, name and messageId', async () => {
    const el = await mountGcs('gs://bkt/dir/report.md', true);
    el.messageId = 'msg-123';

    let detail: { bucket: string; object: string; name: string; messageId: string } | undefined;
    document.addEventListener('gcs-link-click', (e) => {
      detail = (e as CustomEvent).detail;
    });

    const link = gcsLinks(el)[0];
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(detail).toEqual({
      bucket: 'bkt',
      object: 'dir/report.md',
      name: 'report.md',
      messageId: 'msg-123',
    });
  });

  it('gives gs:// links a "#" href and keeps the click on the page', async () => {
    const el = await mountGcs('gs://bkt/dir/report.md', true);
    const link = gcsLinks(el)[0];
    // Positive control: the link exists, so the href check is not vacuous.
    expect(link).toBeDefined();
    expect(link.getAttribute('href')).toBe('#');

    const click = new MouseEvent('click', { bubbles: true, composed: true, cancelable: true });
    link.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(true);
  });

  it('keeps a middle-click on a gs:// link from opening a tab', async () => {
    const el = await mountGcs(
      'gs://bkt/dir/report.md and [the docs](https://example.com/docs)',
      true
    );
    const link = gcsLinks(el)[0];
    expect(link).toBeDefined();
    const aux = new MouseEvent('auxclick', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 1,
    });
    link.dispatchEvent(aux);
    expect(aux.defaultPrevented).toBe(true);

    // Positive control: a middle-click on an ordinary external link is left alone.
    const external = el.shadowRoot?.querySelector<HTMLAnchorElement>(
      '.md-content a[href="https://example.com/docs"]'
    );
    expect(external).toBeTruthy();
    const externalAux = new MouseEvent('auxclick', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 1,
    });
    external?.dispatchEvent(externalAux);
    expect(externalAux.defaultPrevented).toBe(false);
  });

  it('gs:// link wins leftmost over an embedded /workspace path, with no path-link inside it', async () => {
    const el = await mountGcs('gs://bkt/workspace/x.md', true);
    expect(gcsLinks(el)).toHaveLength(1);
    expect(el.shadowRoot?.querySelectorAll('.md-content .path-link')).toHaveLength(0);
  });

  it('does not dispatch when data-gcs-uri is present but empty', async () => {
    const el = await mountGcs('gs://bkt/o.txt', true);
    const link = gcsLinks(el)[0];
    link.setAttribute('data-gcs-uri', '');

    let dispatched = false;
    document.addEventListener('gcs-link-click', () => {
      dispatched = true;
    });
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(dispatched).toBe(false);
  });

  it('does not dispatch when data-gcs-uri fails to parse', async () => {
    const el = await mountGcs('gs://bkt/o.txt', true);
    const link = gcsLinks(el)[0];
    link.setAttribute('data-gcs-uri', 'not-a-valid-uri');

    let dispatched = false;
    document.addEventListener('gcs-link-click', () => {
      dispatched = true;
    });
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(dispatched).toBe(false);
  });

  function ghRefLinksIn(el: ScionChatMessage): HTMLAnchorElement[] {
    return Array.from(
      el.shadowRoot?.querySelectorAll('.md-content .gh-ref-link') ?? []
    ) as HTMLAnchorElement[];
  }

  // Interplay with the separate GitHub shortform-ref pass (styleGithubRefs,
  // applied after styleEntityLinks in renderContent): a fragment-shaped
  // gs:// tail must not become a gcs-link AND then also feed a GitHub ref
  // match, or vice versa.
  //
  // An extracted object immediately followed by `#` is VOID — no link at
  // all, not a link truncated at the boundary — whenever a further
  // non-whitespace character follows, exactly like the existing `a#frag`
  // parity vector (chat-file-links.test.ts / pkg/hub/gcs_link_test.go, both
  // `linked: false`). `o/r` followed by `#1` is that same shape, so the
  // actual behaviour is zero `.gcs-link` elements here, not one truncated
  // at `/r` with `#1` left as plain trailing text.
  //
  // With no gcs-link produced, styleGithubRefs runs on the entirely
  // untouched raw text next. It does not match here either, independent of
  // the gcs pass: GITHUB_REF_REGEX's leading boundary group rejects a
  // preceding `/`, and both `o` (preceded by the bucket's own `/`) and `r`
  // (preceded by `/` after `o`) are only ever reachable with a `/` right
  // before them — the same rule already pinned by the "does not link the
  // tail of a longer slash-separated path" case above for `a/b/c#12`.
  //
  // One further consequence worth noting: a cross-feature collision where
  // the GitHub-ref pass reaches inside an existing gcs `<a>` and re-links
  // its `#<number>` tail cannot arise from this vector shape at all — the
  // continuation rule voids any gcs extraction at a `#` followed by a digit
  // (or any other non-whitespace character), so a *successful* gcs-link can
  // never itself display a trailing `#<number>` for GITHUB_REF_REGEX to even
  // attempt matching against. The gh pass's existing `<a>`-skip
  // (GITHUB_REF_SKIP_REGION) still exists as a general safeguard for other
  // shapes (e.g. an object that merely resembles `owner/repo` with no `#` at
  // all), but it is not what makes this particular case produce zero links
  // — the void rule alone already does.
  it('produces neither a gcs-link nor a GitHub-ref link for a gs:// URI with a fragment-continuation digit tail', async () => {
    const el = await mountGcs('see gs://bkt/o/r#1 for details', true);
    expect(gcsLinks(el)).toHaveLength(0);
    expect(ghRefLinksIn(el)).toHaveLength(0);
    // The raw, unlinked text survives verbatim — proof this is "no link
    // produced", not "linked then silently dropped by some other guard".
    expect(el.shadowRoot?.querySelector('.md-content')?.textContent).toContain(
      'see gs://bkt/o/r#1 for details'
    );
  });

  it('positive control: a GitHub ref elsewhere in the same body still becomes a link, proving the absence above is specific to the gs:// URI, not a global GitHub-ref failure', async () => {
    const el = await mountGcs('see gs://bkt/o/r#1 and also ptone/scion#2217', true);
    expect(gcsLinks(el)).toHaveLength(0);
    const links = ghRefLinksIn(el);
    expect(links).toHaveLength(1);
    expect(links[0].textContent).toBe('ptone/scion#2217');
  });
});

// Review round 2, R2-3: AC4 ("sees native chat timestamps in Tokyo time,
// with a zone label") was implemented (R1-3) but had no test pinning it.
describe('scion-chat-message zone label (AC4, review R2-3)', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('renders the preferred-zone time and a title with the full instant and zone', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'hello';
    el.fromAgent = true;
    el.timestamp = '2026-09-23T15:00:00Z'; // -> 2026-09-24T00:00 JST
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;

    const timeEl = el.shadowRoot?.querySelector('.msg-time');
    expect(timeEl?.textContent).toBe('00:00');
    expect(timeEl?.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
  });

  // Review R2-1: a message already mounted (e.g. before /auth/me resolves,
  // or before a later preference change) must not stay stuck in the
  // browser zone — DisplayZoneController re-renders it.
  it('re-renders in the new zone after a mounted message outlives a preference change', async () => {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'hello';
    el.fromAgent = true;
    el.timestamp = '2026-09-23T15:00:00Z';
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.msg-time')?.textContent).toBe('15:00'); // UTC (Auto)

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    const timeEl = el.shadowRoot?.querySelector('.msg-time');
    expect(timeEl?.textContent).toBe('00:00');
    expect(timeEl?.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
  });
});

describe('scion-chat-message wide tables', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('wraps each table in a named, keyboard-reachable sideways scroller, once', async () => {
    const el = await mount(
      '<table><tr><td>a</td></tr></table><table><caption> Agents </caption><tr><td>b</td></tr></table>'
    );
    const wrappers = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('.md-content .md-table-scroll') ?? []
    );
    expect(wrappers).toHaveLength(2);
    for (const w of wrappers) {
      expect(w.firstElementChild?.tagName).toBe('TABLE');
      expect(w.tabIndex).toBe(0);
      expect(w.getAttribute('role')).toBe('region');
    }
    expect(wrappers.map((w) => w.getAttribute('aria-label'))).toEqual(['Table', 'Agents']);

    // A re-render of the same content does not wrap twice.
    el.requestUpdate();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.md-table-scroll .md-table-scroll')).toHaveLength(0);
  });
});

const { imageThumbStyle, knownImageSize } = await import('./chat-message.js');

describe('inline image thumbnail box', () => {
  const ref = (id: string, extra: { width?: number; height?: number } = {}) => ({
    id,
    name: `${id}.png`,
    mime: 'image/png',
    size: 10,
    ...extra,
  });

  it('reserves the whole thumbnail box while the size is unknown', () => {
    expect(knownImageSize(ref('unknown'))).toBeNull();
    expect(imageThumbStyle(null)).toEqual({
      width: '320px',
      aspectRatio: '320 / 240',
    });
  });

  it('scales a large image down into the thumbnail box, keeping its ratio', () => {
    // Width-bound: 1200x500 fits at 320 wide.
    expect(imageThumbStyle({ width: 1200, height: 500 })).toEqual({
      width: '320px',
      aspectRatio: '1200 / 500',
    });
    // Height-bound: 600x900 fits at 240 tall, so 160 wide.
    expect(imageThumbStyle({ width: 600, height: 900 })).toEqual({
      width: '160px',
      aspectRatio: '600 / 900',
    });
  });

  it('never enlarges a small image', () => {
    expect(imageThumbStyle({ width: 40, height: 30 })).toEqual({
      width: '40px',
      aspectRatio: '40 / 30',
    });
  });

  it('takes a size only from the server, never from an earlier load', () => {
    expect(knownImageSize(ref('sent', { width: 800, height: 600 }))).toEqual({
      width: 800,
      height: 600,
    });
    expect(knownImageSize(ref('broken', { width: 0, height: 0 }))).toBeNull();
  });
});

describe('scion-chat-message attachment-only (ptone/scion#6146)', () => {
  async function mountWith(
    body: string,
    attachments: string[],
    deletedAt = ''
  ): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = body;
    el.attachments = attachments;
    el.deletedAt = deletedAt;
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;
    return el;
  }

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders the attachments without an empty text bubble', async () => {
    const el = await mountWith('', ['/scratch/shot.png']);
    expect(el.shadowRoot?.querySelector('.bubble-content')).toBeNull();
    const chip = el.shadowRoot?.querySelector('.attachments .attachment-chip');
    expect(chip?.textContent).toContain('shot.png');
  });

  it('keeps the text bubble for a message with text and attachments', async () => {
    const el = await mountWith('see this', ['/scratch/shot.png']);
    expect(el.shadowRoot?.querySelector('.bubble-content')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.attachments')).not.toBeNull();
  });

  it('still shows a deleted message as deleted', async () => {
    const el = await mountWith('', ['/scratch/shot.png'], '2026-10-24T00:00:00Z');
    expect(el.shadowRoot?.querySelector('.deleted-message')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.attachments')).toBeNull();
  });
});

describe('scion-chat-message More actions dots (ptone/scion#6556)', () => {
  interface CssBlock {
    prelude: string;
    body: string;
  }

  /** Split CSS text into its blocks at one nesting level: `prelude { body }`. */
  function cssBlocks(css: string): CssBlock[] {
    const blocks: CssBlock[] = [];
    const text = css.replace(/\/\*[\s\S]*?\*\//g, '');
    let start = 0;
    let i = 0;
    while (i < text.length) {
      if (text[i] !== '{') {
        i++;
        continue;
      }
      let depth = 1;
      let j = i + 1;
      while (j < text.length && depth > 0) {
        if (text[j] === '{') depth++;
        else if (text[j] === '}') depth--;
        j++;
      }
      blocks.push({
        prelude: text.slice(start, i).replace(/\s+/g, ' ').trim(),
        body: text.slice(i + 1, j - 1),
      });
      start = i = j;
    }
    return blocks;
  }

  /** The merged declarations of every rule in `blocks` whose selector list has `selector`. */
  function declarationsFor(blocks: CssBlock[], selector: string): Record<string, string> {
    const decls: Record<string, string> = {};
    for (const block of blocks) {
      if (!block.prelude.split(',').some((sel) => sel.trim() === selector)) continue;
      for (const decl of block.body.split(';')) {
        const colon = decl.indexOf(':');
        if (colon < 0) continue;
        decls[decl.slice(0, colon).trim()] = decl
          .slice(colon + 1)
          .replace(/\s+/g, ' ')
          .trim();
      }
    }
    return decls;
  }

  function componentCss(): string {
    const Msg = customElements.get('scion-chat-message') as unknown as {
      styles: { cssText: string } | { cssText: string }[];
    };
    return [Msg.styles]
      .flat()
      .map((s) => s.cssText)
      .join('\n');
  }

  const rem = (v: string | undefined): number => {
    const m = /^(-?[\d.]+)rem$/.exec(v ?? '');
    if (!m) throw new Error(`not a rem length: ${v}`);
    return Number(m[1]);
  };

  async function mountActions(props: Partial<ScionChatMessage>): Promise<ScionChatMessage> {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    Object.assign(el, { senderName: 'Alice', timestamp: '2026-10-24T09:05:00Z' }, props, {
      actionsEnabled: true,
    });
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve();
    await el.updateComplete;
    return el;
  }

  afterEach(() => {
    document.body.innerHTML = '';
  });

  for (const fromAgent of [false, true]) {
    it(`sits inside the text bubble, in its reserved corner (${fromAgent ? "someone else's" : 'own'} message)`, async () => {
      const el = await mountActions({ body: 'hello', fromAgent });
      const button = el.actionsButton!;
      expect(button).not.toBeNull();
      const content = button.parentElement!;
      expect(content.classList.contains('bubble-content')).toBe(true);
      expect(content.classList.contains('has-actions')).toBe(true);
      // The text is a sibling of the button, never inside it.
      expect(content.querySelector('.md-content')?.textContent).toContain('hello');
      expect(button.textContent?.trim()).toBe('');
    });
  }

  it('keeps the exact accessible label and the menu semantics', async () => {
    const el = await mountActions({ body: 'hello', fromAgent: true, showHeader: true });
    const button = el.actionsButton!;
    const time = el.shadowRoot!.querySelector('.msg-time')?.textContent?.trim();
    expect(time).toBeTruthy();
    expect(button.getAttribute('aria-label')).toBe(`More actions, message from Alice, ${time}`);
    expect(button.getAttribute('aria-haspopup')).toBe('menu');
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(button.querySelector('sl-icon')?.getAttribute('name')).toBe('three-dots-vertical');
  });

  it('keeps the usage hint out of the text bubble', async () => {
    const el = await mountActions({ body: 'hello', actionsTabIndex: 0 });
    const help = el.shadowRoot!.getElementById('actions-help')!;
    expect(help).not.toBeNull();
    expect(el.actionsButton!.getAttribute('aria-describedby')).toBe('actions-help');
    expect(help.closest('.bubble-content')).toBeNull();
    expect(el.shadowRoot!.querySelector('.bubble-content')?.textContent).not.toContain('arrows');
  });

  it('sits in the deleted-message bubble too', async () => {
    const el = await mountActions({ body: 'gone', deletedAt: '2026-10-24T10:00:00Z' });
    const content = el.actionsButton!.parentElement!;
    expect(content.classList.contains('bubble-content')).toBe(true);
    expect(content.querySelector('.deleted-message')).not.toBeNull();
  });

  it('reserves a corner beside the attachments of an attachment-only message', async () => {
    const el = await mountActions({ body: '', attachments: ['/scratch/shot.png'] });
    expect(el.shadowRoot!.querySelector('.bubble-content')).toBeNull();
    const button = el.actionsButton!;
    expect(button.parentElement!.classList.contains('bubble')).toBe(true);
    expect(button.parentElement!.classList.contains('actions-corner')).toBe(true);
    expect(button.closest('.attachments')).toBeNull();
  });

  it('reserves no corner while the button is off', async () => {
    const el = document.createElement('scion-chat-message') as ScionChatMessage;
    el.body = 'hello';
    el.attachments = [];
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.actionsButton).toBeNull();
    expect(el.shadowRoot!.querySelector('.has-actions, .actions-corner')).toBeNull();
  });

  it('is always visible: no hover reveal, and nothing hides it on touch (hover: none)', () => {
    const css = componentCss();
    const top = cssBlocks(css);
    const button = declarationsFor(top, '.msg-actions');
    expect(button.opacity).toBeUndefined();
    expect(button.display).not.toBe('none');
    expect(button.visibility).toBeUndefined();
    // No rule anywhere, media queries included, hides or fades the button.
    const allBlocks = top.flatMap((b) => (b.prelude.startsWith('@') ? cssBlocks(b.body) : [b]));
    for (const b of allBlocks.filter((b) => b.prelude.includes('.msg-actions'))) {
      expect(b.body, b.prelude).not.toMatch(/opacity|display\s*:\s*none|visibility/);
    }
    const hoverNone = top.filter((b) => /^@media\s*\(\s*hover\s*:\s*none\s*\)/.test(b.prelude));
    for (const b of hoverNone) expect(b.body).not.toContain('msg-actions');
  });

  it('is bare dots with a touch target of at least 24px, in theme colours', () => {
    const button = declarationsFor(cssBlocks(componentCss()), '.msg-actions');
    expect(button.border).toBe('none');
    expect(button.background).toBe('none');
    expect(rem(button.width) * 16).toBeGreaterThanOrEqual(24);
    expect(rem(button.height) * 16).toBeGreaterThanOrEqual(24);
    expect(button.color).toMatch(/^var\(--scion-/);
  });

  it('stays outside the text box: the bubble reserves the whole corner column', () => {
    const top = cssBlocks(componentCss());
    const button = declarationsFor(top, '.msg-actions');
    for (const sel of ['.bubble-content.has-actions', '.bubble.actions-corner']) {
      const reserve = declarationsFor(top, sel);
      expect(reserve.position, sel).toBe('relative');
      if (sel === '.bubble.actions-corner') expect(reserve['box-sizing'], sel).toBe('border-box');
      expect(rem(reserve['padding-right']), sel).toBeGreaterThanOrEqual(
        rem(button.right) + rem(button.width)
      );
    }
  });

  it('shows a focus ring for keyboard focus only', () => {
    const top = cssBlocks(componentCss());
    expect(declarationsFor(top, '.msg-actions:focus').outline).toBe('none');
    expect(declarationsFor(top, '.msg-actions:focus-visible').outline).toMatch(
      /^2px solid var\(--scion-primary/
    );
  });
});
