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
 * Tests for redundant Cancel, Save Draft, and Submit Review actions in the
 * sticky review header (.g-sticky in artifact-detail.ts, ptone/scion#9488):
 * parity of handlers, confirmations, and disabled/busy/loading states with
 * the bottom .edit-footer buttons, plus compact icon-only mobile layout rules.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse, ReviewDraft } from '../../client/artifacts.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts.js';
import {
  ARTIFACT_REVIEW_EDITOR_FLAG,
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';

const loader = vi.hoisted(() => ({
  load: vi.fn(() => Promise.resolve()),
  preload: vi.fn(),
}));
vi.mock('../artifact-review/review-editor-loader.js', () => ({
  loadReviewEditor: loader.load,
  preloadReviewEditor: loader.preload,
}));

class FakeReviewEditor extends HTMLElement {
  content = '';
  scrollInset = 0;
  hasOwnMarks(): boolean {
    return false;
  }
  isComposing(): boolean {
    return false;
  }
  flush(): string | null {
    return this.content;
  }
  comment(): Promise<void> {
    return Promise.resolve();
  }
}
if (!customElements.get('scion-review-editor')) {
  customElements.define('scion-review-editor', FakeReviewEditor);
}

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const ARTIFACT_ID = '22222222-2222-2222-2222-222222222222';
const BASE_TEXT = 'We ship in Q3.\n';
const MARKED_TEXT = 'We ship in {~~Q3~>Q4~~}{>>move out a quarter<<}.\n';

function artifact(seq = 1): ArtifactResponse {
  return {
    artifact: {
      id: ARTIFACT_ID,
      ref: `art:${ARTIFACT_ID}`,
      scopeKind: 'project',
      scopeRef: PROJECT_ID,
      ownerKind: 'user',
      ownerRef: 'u1',
      title: 'Plan',
      currentSeq: seq,
      createdAt: '2026-11-20T00:00:00Z',
      updatedAt: '2026-11-20T00:00:00Z',
    },
    version: {
      seq,
      ref: `art:${ARTIFACT_ID}@${seq}`,
      kind: 'publish',
      entryPath: 'plan.md',
      totalBytes: BASE_TEXT.length,
      fileCount: 1,
      createdByKind: 'user',
      createdByRef: 'u1',
      createdAt: '2026-11-20T00:00:00Z',
      state: 'ready',
      files: [
        {
          path: 'plan.md',
          size: BASE_TEXT.length,
          sha256: 'abc',
          mediaType: 'text/markdown',
        },
      ],
    },
    canPublish: true,
    canReview: true,
  };
}

interface MockServerState {
  draft: ReviewDraft | null;
  currentSeq: number;
  requests: Array<{ method: string; url: string; body?: string }>;
  holdPut?: (res: Response) => Promise<Response>;
  holdFinalize?: (res: Response) => Promise<Response>;
}

function mockHub(state: MockServerState): void {
  const meta = artifact(state.currentSeq);
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url =
      typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
    const method = (init?.method ?? 'GET').toUpperCase();
    const rawBody = typeof init?.body === 'string' ? init.body : undefined;
    state.requests.push({ method, url, body: rawBody });

    const reply = (r: Response): Promise<Response> => Promise.resolve(r);

    if (url.includes('/review-draft')) {
      if (method === 'GET') {
        return reply(
          new Response(JSON.stringify({ draft: state.draft, currentSeq: state.currentSeq }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (method === 'PUT') {
        const parsed = JSON.parse(rawBody ?? '{}') as {
          rev: number;
          base: number;
          entry: string;
          body: string;
          note: string;
        };
        const savedAt = '2026-11-21T18:10:00Z';
        state.draft = {
          rev: (state.draft?.rev ?? 0) + 1,
          base: parsed.base,
          entry: parsed.entry,
          body: parsed.body,
          note: parsed.note,
          carried: null,
          savedAt,
          createdAt: state.draft?.createdAt ?? savedAt,
        };
        const res = new Response(
          JSON.stringify({ rev: state.draft.rev, savedAt, currentSeq: state.currentSeq }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        );
        return state.holdPut ? state.holdPut(res) : reply(res);
      }
      if (method === 'DELETE') {
        state.draft = null;
        return reply(new Response(null, { status: 204 }));
      }
    }

    if (url.endsWith('/finalize')) {
      state.currentSeq += 1;
      meta.artifact.currentSeq = state.currentSeq;
      meta.version = { ...meta.version!, seq: state.currentSeq, kind: 'review' };
      const res = new Response(JSON.stringify(meta), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
      return state.holdFinalize ? state.holdFinalize(res) : reply(res);
    }
    if (method === 'PUT' && url.includes('/files/')) {
      return reply(new Response(null, { status: 204 }));
    }
    if (method === 'POST' && url.endsWith('/versions')) {
      return reply(
        new Response(
          JSON.stringify({
            artifact: meta.artifact,
            version: { seq: state.currentSeq + 1 },
            upload: { required: ['plan.md'] },
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }
    if (url.includes('/files/plan.md')) {
      return reply(new Response(BASE_TEXT, { status: 200 }));
    }
    if (url.includes('/versions')) {
      const versions = Array.from({ length: state.currentSeq }, (_, i) => ({
        ...meta.version!,
        seq: state.currentSeq - i,
        ref: `art:${ARTIFACT_ID}@${state.currentSeq - i}`,
      }));
      return reply(
        new Response(JSON.stringify({ versions, discarded: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    return reply(
      new Response(JSON.stringify(meta), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  });
}

async function settle(el: ScionPageArtifactDetail, ticks = 10): Promise<void> {
  for (let i = 0; i < ticks; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

function extractMediaBlocks(cssText: string, query: string): string[] {
  const blocks: string[] = [];
  const header = `@media ${query}`;
  let idx = 0;
  while ((idx = cssText.indexOf(header, idx)) !== -1) {
    const open = cssText.indexOf('{', idx + header.length);
    if (open === -1) break;
    let depth = 0;
    let end = open;
    for (; end < cssText.length; end++) {
      if (cssText[end] === '{') depth++;
      else if (cssText[end] === '}') {
        depth--;
        if (depth === 0) {
          blocks.push(cssText.slice(open + 1, end));
          break;
        }
      }
    }
    idx = end + 1;
  }
  return blocks;
}

function findButton(
  el: ScionPageArtifactDetail,
  selector: string,
  label: string
): HTMLElement | undefined {
  return Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>(selector)).find((b) =>
    b.textContent?.includes(label)
  );
}

async function mountAndStartReview(draftsEnabled: boolean): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = {
    [ARTIFACTS_FLAG]: true,
    [ARTIFACT_REVIEW_EDITOR_FLAG]: true,
    [ARTIFACTS_REVIEW_DRAFTS_FLAG]: draftsEnabled,
  };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = {
    path: `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
    user: { id: 'u1', email: 'rev@test', name: 'Reviewer', role: 'member' },
  } as unknown as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);

  findButton(el, '.header-actions sl-button', 'Review')!.click();
  await settle(el);
  return el;
}

function emitGraphicalEdit(el: ScionPageArtifactDetail, text: string): void {
  const editor = el.shadowRoot!.querySelector<FakeReviewEditor>('scion-review-editor')!;
  editor.content = text;
  editor.dispatchEvent(new CustomEvent('review-change', { detail: { text } }));
}

describe('ScionPageArtifactDetail sticky review header actions', () => {
  beforeAll(async () => {
    await import('./artifact-detail.js');
  });

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('mirrors disabled and loading states across sticky header and bottom footer buttons', async () => {
    let releasePut: (() => void) | null = null;
    let releaseFinalize: (() => void) | null = null;
    const state: MockServerState = {
      draft: null,
      currentSeq: 1,
      requests: [],
      holdPut: (res): Promise<Response> =>
        new Promise<Response>((resolve) => {
          releasePut = (): void => resolve(res);
        }),
      holdFinalize: (res): Promise<Response> =>
        new Promise<Response>((resolve) => {
          releaseFinalize = (): void => resolve(res);
        }),
    };
    mockHub(state);
    const el = await mountAndStartReview(true);

    const stickyCancel = el.shadowRoot!.querySelector<HTMLElement>(
      '.g-sticky .g-review-actions sl-button.cancel-review-btn'
    )!;
    const stickySaveDraft = el.shadowRoot!.querySelector<HTMLElement>(
      '.g-sticky .g-review-actions sl-button.save-draft-btn'
    )!;
    const stickySubmit = el.shadowRoot!.querySelector<HTMLElement>(
      '.g-sticky .g-review-actions sl-button.submit-review-btn'
    )!;

    const footerCancel = el.shadowRoot!.querySelector<HTMLElement>(
      '.edit-footer sl-button.cancel-review-btn'
    )!;
    const footerSaveDraft = el.shadowRoot!.querySelector<HTMLElement>(
      '.edit-footer sl-button.save-draft-btn'
    )!;
    const footerSubmit = el.shadowRoot!.querySelector<HTMLElement>(
      '.edit-footer sl-button.submit-review-btn'
    )!;

    // Both sets are rendered.
    expect(stickyCancel).not.toBeNull();
    expect(stickySaveDraft).not.toBeNull();
    expect(stickySubmit).not.toBeNull();
    expect(footerCancel).not.toBeNull();
    expect(footerSaveDraft).not.toBeNull();
    expect(footerSubmit).not.toBeNull();

    // Initial state: Cancel enabled; Save Draft and Submit Review disabled.
    expect(stickyCancel.hasAttribute('disabled')).toBe(false);
    expect(footerCancel.hasAttribute('disabled')).toBe(false);
    expect(stickySaveDraft.hasAttribute('disabled')).toBe(true);
    expect(footerSaveDraft.hasAttribute('disabled')).toBe(true);
    expect(stickySubmit.hasAttribute('disabled')).toBe(true);
    expect(footerSubmit.hasAttribute('disabled')).toBe(true);

    // Unmarked edit: Save Draft enabled, Submit Review still disabled on both sets.
    emitGraphicalEdit(el, 'Unmarked change.\n');
    await el.updateComplete;
    expect(stickySaveDraft.hasAttribute('disabled')).toBe(false);
    expect(footerSaveDraft.hasAttribute('disabled')).toBe(false);
    expect(stickySubmit.hasAttribute('disabled')).toBe(true);
    expect(footerSubmit.hasAttribute('disabled')).toBe(true);

    // Valid marked edit: both Save Draft and Submit Review enabled on both sets.
    emitGraphicalEdit(el, MARKED_TEXT);
    await el.updateComplete;
    expect(stickySaveDraft.hasAttribute('disabled')).toBe(false);
    expect(footerSaveDraft.hasAttribute('disabled')).toBe(false);
    expect(stickySubmit.hasAttribute('disabled')).toBe(false);
    expect(footerSubmit.hasAttribute('disabled')).toBe(false);

    // While Save Draft is in-flight: both Save Draft buttons show loading and both Submit buttons are disabled.
    stickySaveDraft.click();
    await settle(el, 3);
    expect(stickySaveDraft.hasAttribute('loading')).toBe(true);
    expect(footerSaveDraft.hasAttribute('loading')).toBe(true);
    expect(stickySubmit.hasAttribute('disabled')).toBe(true);
    expect(footerSubmit.hasAttribute('disabled')).toBe(true);

    releasePut!();
    await settle(el);
    expect(state.draft?.rev).toBe(1);
    expect(stickySaveDraft.hasAttribute('loading')).toBe(false);
    expect(footerSaveDraft.hasAttribute('loading')).toBe(false);
    expect(stickySubmit.hasAttribute('disabled')).toBe(false);
    expect(footerSubmit.hasAttribute('disabled')).toBe(false);

    // While Submit Review is in-flight: both Submit buttons show loading and both Cancel + Save Draft buttons are disabled.
    stickySubmit.click();
    await settle(el, 4);
    expect(stickySubmit.hasAttribute('loading')).toBe(true);
    expect(footerSubmit.hasAttribute('loading')).toBe(true);
    expect(stickyCancel.hasAttribute('disabled')).toBe(true);
    expect(footerCancel.hasAttribute('disabled')).toBe(true);
    expect(stickySaveDraft.hasAttribute('disabled')).toBe(true);
    expect(footerSaveDraft.hasAttribute('disabled')).toBe(true);

    releaseFinalize!();
    await settle(el);
    expect(state.currentSeq).toBe(2);
    expect(state.draft).toBeNull();
    expect(el.shadowRoot!.querySelector('.g-sticky')).toBeNull();
  });

  it('triggers the same Cancel confirmation dialog and clean close from the sticky header', async () => {
    const state: MockServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountAndStartReview(true);

    // Clean cancel from sticky header closes review mode immediately.
    el.shadowRoot!.querySelector<HTMLElement>('.g-sticky .cancel-review-btn')!.click();
    await settle(el);
    expect(el.shadowRoot!.querySelector('.g-sticky')).toBeNull();

    // Re-enter review, make an unsaved edit, and click sticky Cancel -> opens confirmation dialog.
    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);
    emitGraphicalEdit(el, MARKED_TEXT);
    await el.updateComplete;

    el.shadowRoot!.querySelector<HTMLElement>('.g-sticky .cancel-review-btn')!.click();
    await el.updateComplete;
    const dialog = el.shadowRoot!.querySelector('sl-dialog.draft-cancel-dialog');
    expect(dialog).not.toBeNull();

    // Save draft and close via the confirmation dialog.
    dialog!.querySelector<HTMLElement>('sl-button.draft-cancel-save')!.click();
    await settle(el);
    expect(state.draft?.body).toBe(MARKED_TEXT);
    expect(el.shadowRoot!.querySelector('.g-sticky')).toBeNull();
  });

  it('keeps legacy Save review parity in sticky header when review drafts experiment is off', async () => {
    const state: MockServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountAndStartReview(false);

    const stickyCancel = el.shadowRoot!.querySelector<HTMLElement>(
      '.g-sticky .g-review-actions sl-button.cancel-review-btn'
    )!;
    const stickySave = el.shadowRoot!.querySelector<HTMLElement>(
      '.g-sticky .g-review-actions sl-button.submit-review-btn'
    )!;
    const footerSave = el.shadowRoot!.querySelector<HTMLElement>(
      '.edit-footer sl-button.submit-review-btn'
    )!;

    expect(stickyCancel).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.g-sticky .save-draft-btn')).toBeNull();
    expect(stickySave.textContent).toContain('Save review');
    expect(stickySave.hasAttribute('disabled')).toBe(true);
    expect(footerSave.hasAttribute('disabled')).toBe(true);

    emitGraphicalEdit(el, MARKED_TEXT);
    await el.updateComplete;
    expect(stickySave.hasAttribute('disabled')).toBe(false);
    expect(footerSave.hasAttribute('disabled')).toBe(false);

    stickySave.click();
    await settle(el);
    expect(state.currentSeq).toBe(2);
    expect(el.shadowRoot!.querySelector('.g-sticky')).toBeNull();
  });

  it('renders accessible labels, tooltips, icons, and compact non-wrapping mobile CSS rules', async () => {
    const state: MockServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountAndStartReview(true);

    const buttons = Array.from(
      el.shadowRoot!.querySelectorAll<HTMLElement>('.g-sticky .g-review-actions sl-button')
    );
    expect(buttons).toHaveLength(3);

    const expected = [
      { label: 'Cancel', icon: 'x-lg' },
      { label: 'Save Draft', icon: 'floppy' },
      { label: 'Submit Review', icon: 'send' },
    ];
    for (const [i, item] of expected.entries()) {
      const btn = buttons[i]!;
      const tooltip = btn.closest('sl-tooltip');
      expect(tooltip?.getAttribute('content')).toBe(item.label);
      expect(btn.getAttribute('aria-label')).toBe(item.label);
      expect(btn.querySelector('sl-icon[slot="prefix"]')?.getAttribute('name')).toBe(item.icon);
      expect(btn.querySelector('.lbl')?.textContent?.trim()).toBe(item.label);
    }

    // Verify component stylesheet enforces icon-only compact styling on narrow/mobile widths
    // and non-wrapping, non-overflowing sticky toolbar layout at <=640px.
    const ctor = customElements.get('scion-page-artifact-detail') as unknown as {
      styles: Array<{ cssText: string }>;
    };
    const cssText = ctor.styles.map((s) => s.cssText).join('\n');

    const narrowCss = extractMediaBlocks(cssText, '(max-width: 900px)').join('\n');
    expect(narrowCss).toContain('.g-review-actions .lbl');
    expect(narrowCss).toContain('.g-review-actions sl-button::part(label)');
    expect(narrowCss).toContain('padding: 0;');

    const mobileCss = extractMediaBlocks(cssText, '(max-width: 640px)').join('\n');
    expect(mobileCss).toContain('flex-wrap: nowrap;');
    expect(mobileCss).toContain('.g-toolbar .lbl');
    expect(mobileCss).toContain('.g-toolbar .draft-status-wrap');
    expect(mobileCss).toContain('overflow: hidden;');
    expect(mobileCss).toContain('text-overflow: ellipsis;');
  });
});
