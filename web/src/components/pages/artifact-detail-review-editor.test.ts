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
 * The artifact page with the graphical review editor experiment
 * (web.artifact_review_editor) off and on. The editor itself is replaced
 * by a stand-in element: these tests cover the page's wiring, not the
 * editor (see components/artifact-review).
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const loader = vi.hoisted(() => ({
  load: vi.fn(() => Promise.resolve()),
  preload: vi.fn(),
}));
const confirm = vi.hoisted(() => ({
  asked: [] as string[],
  answer: true,
  /** When set, the answer waits until release is called. */
  defer: false,
  release: (_: boolean): void => {},
}));
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: (message: string): Promise<boolean> => {
    confirm.asked.push(message);
    if (confirm.defer) return new Promise<boolean>((r) => (confirm.release = r));
    return Promise.resolve(confirm.answer);
  },
}));
/**
 * The block count: real, or held until released (to act while a size is
 * pending). A text in `counts` counts as the number given there, so a test
 * can be over the block ceiling without lexing 10,000 blocks (the real
 * count at the limits is pinned in artifact-detail-review-size.test.ts).
 */
const blocks = vi.hoisted(() => ({
  defer: false,
  pending: [] as (() => void)[],
  counts: new Map<string, number>(),
}));
vi.mock('../../utils/markdown-blocks.js', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../utils/markdown-blocks.js')>();
  const count = (md: string): Promise<number> => {
    const n = blocks.counts.get(md);
    return n === undefined ? real.countTopLevelBlocks(md) : Promise.resolve(n);
  };
  return {
    countTopLevelBlocks: (md: string): Promise<number> =>
      blocks.defer
        ? // Counted now, handed over when released.
          count(md).then((n) => new Promise<number>((r) => blocks.pending.push(() => r(n))))
        : count(md),
  };
});
vi.mock('../artifact-review/review-editor-loader.js', () => ({
  loadReviewEditor: loader.load,
  preloadReviewEditor: loader.preload,
}));

const ID = '5f1c2d3e-0000-4000-8000-000000000002';
/** The page's large-document limits, read once the page module loads. */
let LARGE_REVIEW_BYTES = 0;
let LARGE_REVIEW_BLOCKS = 0;
let MAX_GRAPHICAL_BYTES = 0;
let MAX_GRAPHICAL_BLOCKS = 0;
const TEXT = 'We ship in Q3.\n';

/** Stand-in for <scion-review-editor>: records calls, returns a set text. */
class FakeReviewEditor extends HTMLElement {
  content = '';
  next: string | null = null;
  /** flush() reports that the document cannot be written. */
  unwritable = false;
  comments = 0;
  /** hasOwnMarks() answer. */
  own = false;
  hasOwnMarks(): boolean {
    return this.own;
  }
  /** isComposing() answer. */
  composing = false;
  isComposing(): boolean {
    return this.composing;
  }
  flush(): string | null {
    if (this.unwritable) return null;
    return this.next ?? this.content;
  }
  comment(): Promise<void> {
    this.comments++;
    return Promise.resolve();
  }
}
if (!customElements.get('scion-review-editor')) {
  customElements.define('scion-review-editor', FakeReviewEditor);
}

function meta(text = TEXT): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'user',
      ownerRef: 'u-1',
      title: 'Plan',
      currentSeq: 1,
      createdAt: '2026-10-05T12:00:00Z',
      updatedAt: '2026-10-05T12:00:00Z',
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: 'plan.md',
      totalBytes: text.length,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path: 'plan.md', size: text.length, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    canPublish: true,
  } as ArtifactResponse;
}

const writes: string[] = [];

function mockFetch(
  text = TEXT,
  opts: {
    body?: () => string;
    /** The entry file's response (held until it resolves, when it is a promise). */
    files?: () => Response | Promise<Response> | null;
    write?: (method: string, url: string, m: ArtifactResponse) => Response;
  } = {}
): void {
  const m = meta(text);
  writes.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (init?.method && init.method !== 'GET') {
        writes.push(`${init.method} ${url}`);
        if (opts.write) return Promise.resolve(opts.write(init.method, url, m));
        return Promise.resolve(new Response('{}', { status: 500 }));
      }
      if (url.endsWith('/grants')) {
        return Promise.resolve(new Response('{"grants":[]}', { status: 200 }));
      }
      if (/\/versions(\?|$)/.test(url)) {
        const body = { versions: [{ ...m.version!, files: [] }], discarded: [] };
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
      }
      if (url.includes('/files/')) {
        const held = opts.files?.();
        if (held) return Promise.resolve(held);
        return Promise.resolve(new Response(opts.body ? opts.body() : text, { status: 200 }));
      }
      return Promise.resolve(new Response(JSON.stringify(m), { status: 200 }));
    })
  );
}

async function settle(el: ScionPageArtifactDetail, n = 20): Promise<void> {
  for (let i = 0; i < n; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(editorFlag: boolean): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': true, 'web.artifact_review_editor': editorFlag };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = { path: `/projects/p-1/artifacts/${ID}` } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);
  return el;
}

const $ = <T extends Element = HTMLElement>(el: ScionPageArtifactDetail, sel: string): T | null =>
  el.shadowRoot!.querySelector<T>(sel);
const all = (el: ScionPageArtifactDetail, sel: string): HTMLElement[] =>
  Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>(sel));
const byText = (el: ScionPageArtifactDetail, sel: string, text: string): HTMLElement | undefined =>
  all(el, sel).find((b) => b.textContent!.includes(text));

async function startReview(el: ScionPageArtifactDetail): Promise<void> {
  byText(el, '.header-actions sl-button', 'Review')!.click();
  await settle(el);
}

async function chooseView(
  el: ScionPageArtifactDetail,
  view: 'graphical' | 'source'
): Promise<void> {
  const toggle = $<HTMLElement & { value: string }>(el, 'sl-radio-group.view-toggle')!;
  toggle.value = view;
  toggle.dispatchEvent(new Event('sl-change'));
  await settle(el);
}

/** A short text that counts as one block over the graphical view's ceiling. */
function overBlockCeiling(): string {
  const md = 'p\n\n'.repeat(3);
  blocks.counts.set(md, MAX_GRAPHICAL_BLOCKS + 1);
  return md;
}

describe('artifact page: graphical review editor experiment', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    ({ LARGE_REVIEW_BYTES, LARGE_REVIEW_BLOCKS, MAX_GRAPHICAL_BYTES, MAX_GRAPHICAL_BLOCKS } =
      await import('./artifact-detail.js'));
  }, 30_000);

  beforeEach(() => {
    loader.load.mockReset();
    loader.load.mockImplementation(() => Promise.resolve());
    loader.preload.mockReset();
    window.localStorage.clear();
    confirm.asked.length = 0;
    confirm.answer = true;
    confirm.defer = false;
    blocks.defer = false;
    blocks.pending.length = 0;
    blocks.counts.clear();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetPrincipalNames();
  });

  it('with the experiment off, Review is the side-by-side page and the editor never loads', async () => {
    mockFetch();
    const el = await mount(false);
    await startReview(el);
    expect($(el, '.review-panes scion-artifact-markdown-frame')).not.toBeNull();
    expect($(el, '.review-panes scion-code-editor.review-editor')).not.toBeNull();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.g-toolbar')).toBeNull();
    expect($(el, 'sl-badge.reviewing')).toBeNull();
    expect(loader.load).not.toHaveBeenCalled();
    expect(loader.preload).not.toHaveBeenCalled();
  });

  it('with the experiment on, reading keeps the isolated preview frame', async () => {
    mockFetch();
    const el = await mount(true);
    expect($(el, 'scion-artifact-markdown-frame')).not.toBeNull();
    expect($(el, 'scion-review-editor')).toBeNull();
    // The editor chunk is fetched ahead of time, but nothing renders it.
    expect(loader.preload).toHaveBeenCalledTimes(1);
    expect(loader.load).not.toHaveBeenCalled();
  });

  it('opens Review in the graphical view: one wide editor, no side preview', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    expect(loader.load).toHaveBeenCalledTimes(1);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    expect(editor).not.toBeNull();
    expect(editor.content).toBe(TEXT);
    expect($(el, 'scion-artifact-markdown-frame')).toBeNull();
    expect($(el, 'sl-radio-group.critic-toggle')).toBeNull();
    expect($(el, 'sl-badge.reviewing')?.textContent).toContain('Reviewing');
    const track = $(el, 'sl-switch.track')!;
    expect(track.hasAttribute('checked')).toBe(true);
    expect(track.hasAttribute('disabled')).toBe(true);
    // The focusable wrapper names the locked switch for keyboard users.
    const wrap = $(el, '.track-wrap')!;
    expect(wrap.getAttribute('tabindex')).toBe('0');
    expect(wrap.getAttribute('role')).toBe('switch');
    expect(wrap.getAttribute('aria-checked')).toBe('true');
    expect(wrap.getAttribute('aria-disabled')).toBe('true');
    expect(wrap.getAttribute('aria-label')).toContain('Track changes: on.');
    expect($(el, '.edit-bar')!.textContent).toContain('Your edits are recorded as suggestions.');
    // Phase 1 shows no tray, accept, reject or mark navigation controls.
    for (const label of ['Reject', 'Accept', 'Withdraw']) {
      expect(byText(el, '.g-toolbar sl-button', label)).toBeUndefined();
    }
    byText(el, '.g-toolbar sl-button', 'Comment')!.click();
    expect(editor.comments).toBe(1);
  });

  it('shows the skeleton while the editor loads', async () => {
    mockFetch();
    let release: () => void = () => {};
    loader.load.mockImplementation(() => new Promise<void>((r) => (release = r)));
    const el = await mount(true);
    await startReview(el);
    expect($(el, '.g-work .skeleton')).not.toBeNull();
    expect($(el, 'scion-review-editor')).toBeNull();
    release();
    await settle(el);
    expect($(el, '.g-work .skeleton')).toBeNull();
    expect($(el, 'scion-review-editor')).not.toBeNull();
  });

  it('falls back to the source view with a notice when the editor cannot load', async () => {
    mockFetch();
    loader.load.mockImplementation(() => Promise.reject(new Error('chunk gone')));
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const el = await mount(true);
    await startReview(el);
    spy.mockRestore();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    expect($(el, '.hint-strip')!.textContent).toContain('The graphical view could not be loaded.');
  });

  it('opens a large document in the source view with a notice; Graphical asks first', async () => {
    // One byte over the soft limit: the page counts bytes, and é is two.
    const large = 'é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a';
    expect(new TextEncoder().encode(large).length).toBe(LARGE_REVIEW_BYTES + 1);
    mockFetch(large);
    const el = await mount(true);
    await startReview(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    const strip = $(el, '.hint-strip')!;
    expect(strip.textContent).toContain('This document is large, so it opened in the Source view.');
    expect(strip.textContent).not.toContain('Open Source view');
    expect($(el, '.hint-region')!.getAttribute('role')).toBe('status');
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(false);
    // The reviewer's own choice is not changed.
    expect(window.localStorage.getItem('scion:artifact-review-view')).toBeNull();
    // Cancel keeps the Source view.
    confirm.answer = false;
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([
      'Opening the graphical view may take a few seconds for a document this size. Continue?',
    ]);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    // Continue opens it, and switching back and forth does not ask again.
    confirm.answer = true;
    await chooseView(el, 'graphical');
    expect(loader.load).toHaveBeenCalledTimes(1);
    expect($<FakeReviewEditor>(el, 'scion-review-editor')!.content).toBe(large);
    await chooseView(el, 'source');
    await chooseView(el, 'graphical');
    expect(confirm.asked.length).toBe(2);
    expect($(el, 'scion-review-editor')).not.toBeNull();
  });

  it('a document too large for the graphical view cannot switch to it', async () => {
    const huge = 'a'.repeat(MAX_GRAPHICAL_BYTES) + '\n';
    mockFetch(huge);
    const el = await mount(true);
    await startReview(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    expect($(el, '.hint-strip')!.textContent).toContain(
      'This document is too large for the graphical view.'
    );
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(true);
    expect($(el, '.g-toolbar sl-tooltip:has(sl-radio-group)')!.getAttribute('content')).toBe(
      'Too large for the graphical view.'
    );
    // Even a change event from the toggle does not open it.
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([]);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
  });

  it('counts top-level blocks too: over the soft limit opens the source view', async () => {
    mockFetch('p\n\n'.repeat(LARGE_REVIEW_BLOCKS + 1));
    const el = await mount(true);
    await startReview(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.hint-strip')!.textContent).toContain('This document is large');
  }, 30_000);

  it('opens a document at the limits in the graphical view', async () => {
    const edge = 'a'.repeat(LARGE_REVIEW_BYTES - 1) + '\n';
    expect(new TextEncoder().encode(edge).length).toBe(LARGE_REVIEW_BYTES);
    mockFetch(edge);
    const el = await mount(true);
    await startReview(el);
    expect(loader.load).toHaveBeenCalledTimes(1);
    expect($<FakeReviewEditor>(el, 'scion-review-editor')!.content).toBe(edge);
    expect($(el, '.hint-strip')).toBeNull();
  });

  it('a large document opens in the source view without the notice when the reviewer chose it', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch('a'.repeat(LARGE_REVIEW_BYTES + 1));
    const el = await mount(true);
    await startReview(el);
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    expect($(el, '.hint-strip')).toBeNull();
  });

  /** Types text into the Source view's editor (its content-changed event). */
  const typeSource = async (el: ScionPageArtifactDetail, content: string): Promise<void> => {
    $(el, '.g-work scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content } })
    );
    await settle(el);
  };
  /** Waits out the Source view's preview pause. */
  const previewPause = (): Promise<void> => new Promise((r) => setTimeout(r, 300));

  it('text that grows past the soft limit in the Source view asks before the graphical view opens', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    const large = TEXT + '{++' + 'x'.repeat(LARGE_REVIEW_BYTES) + '++}';
    await typeSource(el, large);
    confirm.answer = false;
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([
      'Opening the graphical view may take a few seconds for a document this size. Continue?',
    ]);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    confirm.answer = true;
    await chooseView(el, 'graphical');
    expect(loader.load).toHaveBeenCalledTimes(1);
    expect($<FakeReviewEditor>(el, 'scion-review-editor')!.content).toBe(large);
  });

  it('text that grows past the ceiling in the Source view cannot open the graphical view', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(false);
    await typeSource(el, TEXT + '{++' + 'x'.repeat(MAX_GRAPHICAL_BYTES) + '++}');
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([]);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.hint-strip')!.textContent).toContain(
      'This document is too large for the graphical view.'
    );
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(true);
  });

  it('the toggle follows the text as it is edited in the Source view', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const graphical = (): HTMLElement => $(el, 'sl-radio-button[value="graphical"]')!;
    expect(graphical().hasAttribute('disabled')).toBe(false);
    await typeSource(el, TEXT + '{++' + 'x'.repeat(MAX_GRAPHICAL_BYTES) + '++}');
    await previewPause();
    await settle(el);
    expect(graphical().hasAttribute('disabled')).toBe(true);
    await typeSource(el, TEXT);
    await previewPause();
    await settle(el);
    expect(graphical().hasAttribute('disabled')).toBe(false);
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([]);
    expect(loader.load).toHaveBeenCalledTimes(1);
  });

  it('a confirmation answered after the review was left and started again does nothing', async () => {
    mockFetch('é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a');
    const el = await mount(true);
    await startReview(el);
    confirm.defer = true;
    await chooseView(el, 'graphical');
    expect(confirm.asked.length).toBe(1);
    // Leave Review mode and start it again, then answer the old question.
    byText(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    await startReview(el);
    confirm.release(true);
    await settle(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
  });

  /** Lets every held block count finish, one at a time. */
  const releaseBlocks = async (el: ScionPageArtifactDetail): Promise<void> => {
    // A held count joins the queue once it is computed: wait for it.
    for (let i = 0; i < 50 && !blocks.pending.length; i++) await settle(el, 1);
    blocks.defer = false;
    while (blocks.pending.length) {
      blocks.pending.shift()!();
      await settle(el);
    }
  };

  it('two quick choices of the graphical view ask once', async () => {
    mockFetch('é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a');
    const el = await mount(true);
    await startReview(el);
    blocks.defer = true;
    await chooseView(el, 'graphical');
    await chooseView(el, 'graphical');
    // Both counts finish together (one would be the second choice's).
    blocks.defer = false;
    for (const f of blocks.pending.splice(0)) f();
    await settle(el);
    expect(confirm.asked.length).toBe(1);
    expect(loader.load).toHaveBeenCalledTimes(1);
  });

  it('text edited while it is sized is sized again', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    blocks.defer = true;
    await chooseView(el, 'graphical');
    // While the small text is sized, it grows past the soft limit.
    await typeSource(el, TEXT + '{++' + 'x'.repeat(LARGE_REVIEW_BYTES) + '++}');
    confirm.answer = false;
    await releaseBlocks(el);
    expect(confirm.asked.length).toBe(1);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
  });

  it('over the block ceiling with few bytes, the graphical view does not open when chosen', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const many = overBlockCeiling();
    expect(new TextEncoder().encode(many).length).toBeLessThan(LARGE_REVIEW_BYTES);
    await typeSource(el, many);
    // Typing pauses look at bytes only: the toggle stays enabled.
    await previewPause();
    await settle(el);
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(false);
    // Choosing counts the blocks.
    await chooseView(el, 'graphical');
    expect(confirm.asked).toEqual([]);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, '.hint-strip')!.textContent).toContain(
      'This document is too large for the graphical view.'
    );
    expect($(el, 'sl-radio-button[value="graphical"]')!.hasAttribute('disabled')).toBe(true);
  });

  it('text changed while the confirmation is open is sized again before anything opens', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    await typeSource(el, TEXT + '{++' + 'x'.repeat(LARGE_REVIEW_BYTES) + '++}');
    confirm.defer = true;
    await chooseView(el, 'graphical');
    expect(confirm.asked.length).toBe(1);
    // While the question is open the text grows past the ceiling.
    await typeSource(el, TEXT + '{++' + 'x'.repeat(MAX_GRAPHICAL_BYTES) + '++}');
    confirm.release(true);
    await settle(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    expect($(el, '.hint-strip')!.textContent).toContain(
      'This document is too large for the graphical view.'
    );
  });

  it('a choice still being sized when a stale review restarts opens nothing', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    let body = TEXT;
    let reloading = false;
    const large = 'é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a';
    mockFetch(TEXT, {
      // While the reload fetches the new text, the pending choice's block
      // count finishes: it must not open the graphical view on the text
      // being discarded.
      body: () => {
        if (reloading) {
          reloading = false;
          blocks.defer = false;
          for (const f of blocks.pending.splice(0)) f();
        }
        return body;
      },
      write: (method, url, m) => {
        if (url.endsWith('/finalize')) {
          body = large;
          reloading = true;
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            { status: 409 }
          );
        }
        if (method === 'PUT') return new Response(null, { status: 204 });
        return new Response(
          JSON.stringify({
            artifact: m.artifact,
            version: { seq: 2 },
            upload: { required: ['plan.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const el = await mount(true);
    await startReview(el);
    await typeSource(el, 'We {++really ++}ship in Q3.\n');
    blocks.defer = true;
    await chooseView(el, 'graphical');
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el, 40);
    expect(writes.some((w) => w.endsWith('/finalize'))).toBe(true);
    await releaseBlocks(el);
    // The pending choice was for the discarded text: nothing opened; the
    // reloaded text is sized on its own (large: the Source view).
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    const code = $<HTMLElement & { content: string }>(
      el,
      '.g-work scion-code-editor.review-editor'
    )!;
    expect(code.content).toBe(large);
  });

  /** A review save that finalizes as stale: the current version then reads large. */
  const staleSave =
    (onFinalize: () => void) =>
    (method: string, url: string, m: ArtifactResponse): Response => {
      if (url.endsWith('/finalize')) {
        onFinalize();
        return new Response(JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }), {
          status: 409,
        });
      }
      if (method === 'PUT') return new Response(null, { status: 204 });
      return new Response(
        JSON.stringify({
          artifact: m.artifact,
          version: { seq: 2 },
          upload: { required: ['plan.md'] },
        }),
        { status: 201 }
      );
    };
  /** The view toggle's Graphical button and its tooltip. */
  const graphicalButton = (el: ScionPageArtifactDetail): HTMLElement =>
    $(el, 'sl-radio-button[value="graphical"]')!;
  const toggleTip = (el: ScionPageArtifactDetail): string | null =>
    $(el, '.g-toolbar sl-tooltip:has(sl-radio-group)')!.getAttribute('content');

  it('shows a pending toggle while the size is checked, at the start and when choosing', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    blocks.defer = true;
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(true);
    expect(toggleTip(el)).toBe('Checking the document size...');
    await releaseBlocks(el);
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(false);
    expect(toggleTip(el)).toBe('Undo history is kept per view and resets when you switch');
    blocks.defer = true;
    await chooseView(el, 'graphical');
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(true);
    expect(toggleTip(el)).toBe('Checking the document size...');
    await releaseBlocks(el);
    expect(loader.load).toHaveBeenCalledTimes(1);
  });

  it('a choice still pending does not hold back the next review', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    blocks.defer = true;
    await chooseView(el, 'graphical');
    byText(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    blocks.defer = false;
    await startReview(el);
    // The earlier choice's count is still held; the new review can choose.
    expect(blocks.pending.length).toBe(1);
    await chooseView(el, 'graphical');
    expect(loader.load).toHaveBeenCalledTimes(1);
    expect($(el, 'scion-review-editor')).not.toBeNull();
    await releaseBlocks(el);
  });

  it('a too-large size from the block count stays until the blocks are counted again', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const many = overBlockCeiling();
    await typeSource(el, many);
    await chooseView(el, 'graphical');
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(true);
    // Still over the block ceiling (few bytes): the pause keeps it.
    blocks.counts.set(many + 'q\n', MAX_GRAPHICAL_BLOCKS + 2);
    await typeSource(el, many + 'q\n');
    await previewPause();
    await settle(el);
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(true);
    // Back under it: the pause counts the blocks again and enables it.
    await typeSource(el, TEXT);
    await previewPause();
    await settle(el);
    expect(graphicalButton(el).hasAttribute('disabled')).toBe(false);
  }, 30_000);

  it('a choice made while a stale review reloads does nothing', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    let body = TEXT;
    let release: () => void = () => {};
    let hold = false;
    const large = 'é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a';
    mockFetch(TEXT, {
      // The reload's fetch of the new text is held until released below.
      files: () =>
        hold
          ? new Promise<Response>((r) => {
              hold = false;
              release = (): void => r(new Response(body, { status: 200 }));
            })
          : null,
      body: () => body,
      write: staleSave(() => {
        body = large;
        hold = true;
      }),
    });
    const el = await mount(true);
    await startReview(el);
    await typeSource(el, 'We {++really ++}ship in Q3.\n');
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el, 40);
    // The reload waits for its text. A choice of the graphical view arrives
    // now (the toggle itself may not be on the page), and its block count
    // finishes before the reload does.
    blocks.defer = true;
    (el as unknown as { chooseReviewView: (v: string) => void }).chooseReviewView('graphical');
    await releaseBlocks(el);
    expect(confirm.asked).toEqual([]);
    expect(loader.load).not.toHaveBeenCalled();
    release();
    await settle(el, 40);
    expect(loader.load).not.toHaveBeenCalled();
    const code = $<HTMLElement & { content: string }>(
      el,
      '.g-work scion-code-editor.review-editor'
    )!;
    expect(code.content).toBe(large);
  });

  it('a pending choice is dropped by a stale review restart, whatever finishes first', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    let body = TEXT;
    let release: () => void = () => {};
    let hold = false;
    const large = 'é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a';
    mockFetch(TEXT, {
      // The reload's fetch of the new text is held until released below.
      files: () =>
        hold
          ? new Promise<Response>((r) => {
              hold = false;
              release = (): void => r(new Response(body, { status: 200 }));
            })
          : null,
      body: () => body,
      write: staleSave(() => {
        body = large;
        hold = true;
      }),
    });
    const el = await mount(true);
    await startReview(el);
    await typeSource(el, 'We {++really ++}ship in Q3.\n');
    blocks.defer = true;
    await chooseView(el, 'graphical');
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el, 40);
    // The reload is waiting for its text; the pending choice's count finishes now.
    await releaseBlocks(el);
    expect(loader.load).not.toHaveBeenCalled();
    release();
    await settle(el, 40);
    await releaseBlocks(el);
    expect(loader.load).not.toHaveBeenCalled();
    expect($(el, 'scion-review-editor')).toBeNull();
    const code = $<HTMLElement & { content: string }>(
      el,
      '.g-work scion-code-editor.review-editor'
    )!;
    expect(code.content).toBe(large);
  });

  it('still large after an edit while the confirmation is open, it asks again', async () => {
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const large = TEXT + '{++' + 'x'.repeat(LARGE_REVIEW_BYTES) + '++}';
    await typeSource(el, large);
    confirm.defer = true;
    await chooseView(el, 'graphical');
    expect(confirm.asked.length).toBe(1);
    await typeSource(el, large + 'more');
    confirm.release(true);
    await settle(el);
    expect(confirm.asked.length).toBe(2);
    expect(loader.load).not.toHaveBeenCalled();
    confirm.release(true);
    await settle(el);
    expect(loader.load).toHaveBeenCalledTimes(1);
    expect($<FakeReviewEditor>(el, 'scion-review-editor')!.content).toBe(large + 'more');
  });

  it('a review restarted on a larger current version is sized again', async () => {
    let body = TEXT;
    const large = 'é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a';
    mockFetch(TEXT, {
      body: () => body,
      // A review save: create, upload, then finalize, which finds a newer
      // version (with large text) and refuses the review as stale.
      write: (method, url, m) => {
        if (url.endsWith('/finalize')) {
          body = large;
          return new Response(
            JSON.stringify({ error: { code: 'stale_review', message: 'stale' } }),
            { status: 409 }
          );
        }
        if (method === 'PUT') return new Response(null, { status: 204 });
        return new Response(
          JSON.stringify({
            artifact: m.artifact,
            version: { seq: 2 },
            upload: { required: ['plan.md'] },
          }),
          { status: 201 }
        );
      },
    });
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.next = 'We {++really ++}ship in Q3.\n';
    editor.dispatchEvent(
      new CustomEvent('review-change', {
        detail: { text: editor.next },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el, 40);
    expect(writes.some((w) => w.endsWith('/finalize'))).toBe(true);
    // The reloaded text is large: the Source view, with the notice.
    expect($(el, 'scion-review-editor')).toBeNull();
    const code = $<HTMLElement & { content: string }>(
      el,
      '.g-work scion-code-editor.review-editor'
    )!;
    expect(code.content).toBe(large);
    expect($(el, '.hint-strip')!.textContent).toContain('This document is large');
  });

  it('toggles to the source view with the editor text, and remembers the choice', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.next = 'We {++really ++}ship in Q3.\n';
    await chooseView(el, 'source');
    expect($(el, 'scion-review-editor')).toBeNull();
    const code = $<HTMLElement & { content: string }>(
      el,
      '.g-work scion-code-editor.review-editor'
    )!;
    expect(code.content).toBe('We {++really ++}ship in Q3.\n');
    for (const label of ['Comment', 'Suggest', 'Insert', 'Delete']) {
      expect(byText(el, '.g-toolbar sl-button', label), label).toBeDefined();
    }
    expect($(el, 'sl-switch.track')).toBeNull();
    expect(window.localStorage.getItem('scion:artifact-review-view')).toBe('source');
    // Leaving and starting again opens the remembered view.
    byText(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    await startReview(el);
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
    await chooseView(el, 'graphical');
    expect($(el, 'scion-review-editor')).not.toBeNull();
    expect(window.localStorage.getItem('scion:artifact-review-view')).toBe('graphical');
  });

  it('shows refusal hints with Open Source view, and takes edits from review-change', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('review-hint', { detail: { kind: 'format' }, bubbles: true, composed: true })
    );
    await settle(el);
    expect($(el, '.hint-strip')!.textContent).toContain("Formatting can't be suggested here.");
    const save = (): HTMLElement => byText(el, '.edit-footer sl-button', 'Save review')!;
    expect(save().hasAttribute('disabled')).toBe(true);
    editor.dispatchEvent(
      new CustomEvent('review-change', {
        detail: { text: 'We {++really ++}ship in Q3.\n' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);
    // The next edit closes the hint; marks only, so Save is enabled.
    expect($(el, '.hint-strip')).toBeNull();
    expect(save().hasAttribute('disabled')).toBe(false);
    editor.dispatchEvent(
      new CustomEvent('review-hint', {
        detail: { kind: 'list-table' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);
    expect($(el, '.hint-strip')!.textContent).toContain(
      "List and table structure can't be changed in a suggestion."
    );
    byText(el, '.hint-strip sl-button', 'Open Source view')!.click();
    await settle(el);
    expect($(el, '.g-work scion-code-editor.review-editor')).not.toBeNull();
  });

  it('keeps one live region mounted, and the earlier-review hint has no Source pointer', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const region = $(el, '.hint-region')!;
    expect(region.getAttribute('role')).toBe('status');
    expect(region.getAttribute('aria-live')).toBe('polite');
    expect($(el, '.hint-strip')).toBeNull();
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('review-hint', { detail: { kind: 'earlier' }, bubbles: true, composed: true })
    );
    await settle(el);
    // The same element announces the hint.
    expect($(el, '.hint-region')).toBe(region);
    const strip = region.querySelector('.hint-strip')!;
    expect(strip.textContent).toContain('This suggestion is from an earlier review.');
    expect(strip.textContent).toContain('You can comment on it.');
    expect(byText(el, '.hint-strip sl-button', 'Open Source view')).toBeUndefined();
  });

  it('shows the moved-comment hint with Open Source view', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('review-hint', { detail: { kind: 'moved' }, bubbles: true, composed: true })
    );
    await settle(el);
    const strip = $(el, '.hint-strip')!;
    expect(strip.textContent).toContain('This edit would move an earlier comment.');
    expect(strip.textContent).toContain('Edit around it, or use the Source view.');
    expect(byText(el, '.hint-strip sl-button', 'Open Source view')).toBeDefined();
  });

  it('switching to Source with own suggestions says they become read-only in the graphical view', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    // Without own suggestions, no notice.
    await chooseView(el, 'source');
    expect($(el, '.hint-strip')).toBeNull();
    await chooseView(el, 'graphical');
    const again = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    // Returning from Source opens the text in a new editor: ownership starts over.
    expect(again).not.toBe(editor);
    again.own = true;
    await chooseView(el, 'source');
    const strip = $(el, '.hint-strip')!;
    expect(strip.textContent).toContain(
      "Suggestions you've made so far become read-only in the graphical view after you switch to Source."
    );
    expect(strip.textContent).toContain('You can still change them there.');
    expect(byText(el, '.hint-strip sl-button', 'Open Source view')).toBeUndefined();
  });

  it('Ctrl+Alt+M comments in the source view', async () => {
    mockFetch();
    window.localStorage.setItem('scion:artifact-review-view', 'source');
    const el = await mount(true);
    await startReview(el);
    const apply = vi.spyOn(el as unknown as { applyTool: (t: string) => void }, 'applyTool');
    $(el, '.graphical-review')!.dispatchEvent(
      new KeyboardEvent('keydown', { code: 'KeyM', ctrlKey: true, altKey: true, bubbles: true })
    );
    expect(apply).toHaveBeenCalledWith('comment');
  });

  it('keeps the graphical review, and Save takes its text, if the experiment turns off mid-review', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.next = 'We {++really ++}ship in Q3.\n';
    window.__SCION_FEATURES__ = { 'hub.artifacts': true, 'web.artifact_review_editor': false };
    el.requestUpdate();
    await settle(el);
    // The layout of the review that is open does not change.
    expect($(el, 'scion-review-editor')).toBe(editor);
    expect($(el, 'sl-badge.reviewing')).not.toBeNull();
    await (el as unknown as { saveReview(): Promise<void> }).saveReview();
    await settle(el);
    expect((el as unknown as { reviewText: string }).reviewText).toBe(
      'We {++really ++}ship in Q3.\n'
    );
    expect(writes.length).toBeGreaterThan(0);
  });

  it('stops Save and the switch to Source when the editor cannot write its text', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('review-change', {
        detail: { text: 'We {++really ++}ship in Q3.\n' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);
    editor.unwritable = true;
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    expect(writes).toEqual([]);
    expect($(el, '.hint-strip')!.textContent).toContain(
      "That change can't be recorded as a suggestion."
    );
    await chooseView(el, 'source');
    expect($(el, 'scion-review-editor')).not.toBeNull();
    expect($(el, '.g-work scion-code-editor.review-editor')).toBeNull();
    expect(window.localStorage.getItem('scion:artifact-review-view')).not.toBe('source');
  });

  it('asks to finish typing when Save comes while an input method composition runs', async () => {
    mockFetch();
    const el = await mount(true);
    await startReview(el);
    const editor = $<FakeReviewEditor>(el, 'scion-review-editor')!;
    editor.unwritable = true;
    editor.composing = true;
    byText(el, '.edit-footer sl-button', 'Save review')!.click();
    await settle(el);
    expect(writes).toEqual([]);
    expect($(el, '.hint-strip')!.textContent).toContain("Finish the text you're typing first.");
  });
});
