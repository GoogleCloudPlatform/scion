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
vi.mock('../artifact-review/review-editor-loader.js', () => ({
  loadReviewEditor: loader.load,
  preloadReviewEditor: loader.preload,
}));

const ID = '5f1c2d3e-0000-4000-8000-000000000002';
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

function meta(): ArtifactResponse {
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
      totalBytes: TEXT.length,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path: 'plan.md', size: TEXT.length, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    canPublish: true,
  } as ArtifactResponse;
}

const writes: string[] = [];

function mockFetch(): void {
  const m = meta();
  writes.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (init?.method && init.method !== 'GET') {
        writes.push(`${init.method} ${url}`);
        return Promise.resolve(new Response('{}', { status: 500 }));
      }
      if (url.endsWith('/grants')) {
        return Promise.resolve(new Response('{"grants":[]}', { status: 200 }));
      }
      if (/\/versions(\?|$)/.test(url)) {
        const body = { versions: [{ ...m.version!, files: [] }], discarded: [] };
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
      }
      if (url.includes('/files/')) return Promise.resolve(new Response(TEXT, { status: 200 }));
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

describe('artifact page: graphical review editor experiment', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-detail.js');
  }, 30_000);

  beforeEach(() => {
    loader.load.mockReset();
    loader.load.mockImplementation(() => Promise.resolve());
    loader.preload.mockReset();
    window.localStorage.clear();
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
      "Lists and tables can't take suggestions here yet."
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
});
