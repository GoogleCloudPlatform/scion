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
 * Tests for durable review drafts in the review editor (ptone/scion#7116,
 * ptone/scion#8471, Phase 2): footer buttons, ~30 s autosave + status line,
 * load current-base draft on entering Review mode, Cancel / 409 / stale
 * dialogs, #7506 part 4 refused-submit preservation, sign-out teardown,
 * experiment-off parity (A6), and zero browser storage (C8).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import './artifact-detail.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts.js';
import type { ArtifactResponse, ReviewDraft } from '../../client/artifacts.js';
import { ACCOUNT_TEARDOWN_EVENT } from '../../utils/auth.js';
import {
  ARTIFACT_REVIEW_EDITOR_FLAG,
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

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

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
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

interface DraftServerState {
  draft: ReviewDraft | null;
  currentSeq: number;
  requests: Array<{ method: string; url: string; body?: string }>;
  canPublish?: boolean;
  canReview?: boolean;
  fileText?: string;
  nextPutResponse?: () => Response;
  holdPut?: (res: Response) => Promise<Response>;
  nextDeleteResponse?: () => Response;
  finalizeResponse?: () => Response;
}

function mockHub(state: DraftServerState): void {
  const meta = artifact(state.currentSeq);
  if (state.canPublish !== undefined) meta.canPublish = state.canPublish;
  if (state.canReview !== undefined) meta.canReview = state.canReview;
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
        if (state.nextPutResponse) return reply(state.nextPutResponse());
        const parsed = JSON.parse(rawBody ?? '{}') as {
          rev: number;
          base: number;
          entry: string;
          body: string;
          note: string;
          carried?: { base: number; body: string } | null;
        };
        const curRev = state.draft?.rev ?? 0;
        if (parsed.rev !== curRev) {
          return reply(
            new Response(
              JSON.stringify({
                error: {
                  code: 'draft_conflict',
                  message: 'changed elsewhere',
                  details: {
                    rev: curRev,
                    savedAt: state.draft?.savedAt ?? '2026-11-21T18:05:00Z',
                    base: state.draft?.base ?? 1,
                  },
                },
              }),
              { status: 409, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        const savedAt = '2026-11-21T18:10:00Z';
        state.draft = {
          rev: curRev + 1,
          base: parsed.base,
          entry: parsed.entry,
          body: parsed.body,
          note: parsed.note,
          carried: parsed.carried ?? null,
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
        if (state.nextDeleteResponse) return reply(state.nextDeleteResponse());
        state.draft = null;
        return reply(new Response(null, { status: 204 }));
      }
    }

    if (url.endsWith('/finalize')) {
      if (state.finalizeResponse) return reply(state.finalizeResponse());
      state.currentSeq += 1;
      meta.artifact.currentSeq = state.currentSeq;
      meta.version = { ...meta.version!, seq: state.currentSeq, kind: 'review' };
      return reply(
        new Response(JSON.stringify(meta), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
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
      return reply(new Response(state.fileText ?? BASE_TEXT, { status: 200 }));
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

async function mountPage(): Promise<ScionPageArtifactDetail> {
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = {
    path: `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
    user: { id: 'u1', email: 'rev@test', name: 'Reviewer', role: 'member' },
  } as unknown as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);
  return el;
}

describe('ScionPageArtifactDetail review drafts (Phase 2)', () => {
  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    resetServerFlagStateForTests();
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('makes no draft requests and keeps the legacy footer when the experiment is off (A6)', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: false,
    };
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    expect(state.requests.some((r) => r.url.includes('/review-draft'))).toBe(false);
    expect(findButton(el, '.edit-footer sl-button', 'Save review')).toBeDefined();
    expect(findButton(el, '.edit-footer sl-button', 'Save Draft')).toBeUndefined();
    expect(findButton(el, '.edit-footer sl-button', 'Submit Review')).toBeUndefined();
    expect(el.shadowRoot!.querySelector('.draft-status')).toBeNull();
  });

  it('allows Save Draft with 0 marks or unmarked edits while keeping Submit Review disabled (A2)', async () => {
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    const status = el.shadowRoot!.querySelector('.review-toolbar .draft-status');
    expect(status?.getAttribute('aria-live')).toBe('polite');
    expect(status?.textContent?.trim()).toBe('');

    // Edit text outside marks (0 marks, !marksOnly).
    const editor = el.shadowRoot!.querySelector('scion-code-editor.review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: 'Unmarked edit with zero marks.\n' },
      })
    );
    await el.updateComplete;

    const saveDraft = findButton(el, '.edit-footer sl-button', 'Save Draft')!;
    const submitReview = findButton(el, '.edit-footer sl-button', 'Submit Review')!;
    expect(saveDraft.hasAttribute('disabled')).toBe(false);
    expect(submitReview.hasAttribute('disabled')).toBe(true);

    saveDraft.click();
    await settle(el);

    expect(state.draft?.rev).toBe(1);
    expect(state.draft?.body).toBe('Unmarked edit with zero marks.\n');
    expect(status?.textContent).toContain('Draft saved');
  });

  it('loads an existing current-base draft on entering Review mode and deletes it on Submit Review', async () => {
    const state: DraftServerState = {
      draft: {
        rev: 3,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'Earlier note',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 1,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    const editor = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & {
      content: string;
    };
    expect(editor.content).toBe(MARKED_TEXT);
    const status = el.shadowRoot!.querySelector('.draft-status');
    expect(status?.textContent).toContain('Draft saved');

    // Submit Review deletes the draft conditionally with ?rev=3.
    const submitReview = findButton(el, '.edit-footer sl-button', 'Submit Review')!;
    expect(submitReview.hasAttribute('disabled')).toBe(false);
    submitReview.click();
    await settle(el);

    const delReq = state.requests.find(
      (r) => r.method === 'DELETE' && r.url.includes('/review-draft')
    );
    expect(delReq?.url).toContain('?rev=3');
    expect(state.draft).toBeNull();
  });

  it('offers Download and Discard for a stale-base draft on entering Review mode', async () => {
    const state: DraftServerState = {
      draft: {
        rev: 2,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: '',
        carried: null,
        savedAt: '2026-11-21T17:00:00Z',
        createdAt: '2026-11-21T16:50:00Z',
      },
      currentSeq: 2,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('sl-dialog.stale-draft-dialog');
    expect(dialog).not.toBeNull();
    const dialogText = (dialog?.textContent ?? '').replace(/\s+/g, ' ');
    expect(dialogText).toContain('version 1');
    expect(dialogText).toContain('version 2');

    // Discard deletes the stale draft and enters fresh Review mode on v2.
    dialog!.querySelector<HTMLElement>('sl-button.stale-draft-discard')!.click();
    await settle(el);

    expect(state.draft).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-dialog.stale-draft-dialog')).toBeNull();
    expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).not.toBeNull();
  });

  it('handles Cancel clean (keeps draft) and Cancel dirty (Save draft and close / Discard changes / Back)', async () => {
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    const editor = el.shadowRoot!.querySelector('scion-code-editor.review-editor')!;
    editor.dispatchEvent(new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } }));
    await el.updateComplete;

    // Dirty Cancel opens the three-way dialog.
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await el.updateComplete;
    const dialog = el.shadowRoot!.querySelector('sl-dialog.draft-cancel-dialog');
    expect(dialog).not.toBeNull();

    // Back to review keeps editing.
    dialog!.querySelector<HTMLElement>('sl-button.draft-cancel-back')!.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('sl-dialog.draft-cancel-dialog')).toBeNull();

    // Save draft and close saves to hub and leaves Review mode.
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.draft-cancel-save')!.click();
    await settle(el);
    expect(state.draft?.rev).toBe(1);
    expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).toBeNull();

    // Re-enter, make another unsaved change, and choose Discard changes:
    // reverts without sending DELETE to the hub.
    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: 'Unsaved extra text' } })
    );
    await el.updateComplete;
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.draft-cancel-discard')!.click();
    await settle(el);

    expect(state.requests.some((r) => r.method === 'DELETE')).toBe(false);
    expect(state.draft?.rev).toBe(1);
    expect(state.draft?.body).toBe(MARKED_TEXT);
  });

  it('shows the 409 conflict dialog and lets the reviewer Load or Keep without losing text', async () => {
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    // Another tab writes rev 4 while this tab is at rev 0.
    state.draft = {
      rev: 4,
      base: 1,
      entry: 'plan.md',
      body: 'We ship in {~~Q3~>Q5~~}.\n',
      note: 'From tab B',
      carried: null,
      savedAt: '2026-11-21T18:08:00Z',
      createdAt: '2026-11-21T18:00:00Z',
    };

    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } })
    );
    await el.updateComplete;
    findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('sl-dialog.draft-conflict-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog?.textContent).toContain('changed on another device or tab');

    // Keep this one overwrites rev 4 -> rev 5 with local text intact.
    dialog!.querySelector<HTMLElement>('sl-button.draft-conflict-keep')!.click();
    await settle(el);

    expect(state.draft?.rev).toBe(5);
    expect(state.draft?.body).toBe(MARKED_TEXT);
    expect(el.shadowRoot!.querySelector('sl-dialog.draft-conflict-dialog')).toBeNull();
  });

  it('refused submit (#7506 part 4) on 422 unmarked_changes or stale_review keeps draft rev, buffer, and beforeunload guard intact', async () => {
    const state: DraftServerState = {
      draft: {
        rev: 2,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'Initial saved draft',
        carried: null,
        savedAt: '2026-11-21T18:00:00Z',
        createdAt: '2026-11-21T17:55:00Z',
      },
      currentSeq: 1,
      requests: [],
      finalizeResponse: () =>
        new Response(
          JSON.stringify({
            error: {
              code: 'unmarked_changes',
              message: 'unmarked changes',
              details: {
                files: [{ path: 'plan.md', change: 'modified', hunks: [{ line: 1 }] }],
              },
            },
          }),
          { status: 422, headers: { 'Content-Type': 'application/json' } }
        ),
    };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    // Make an unacknowledged edit that still passes client-side marksOnly check
    // so Submit Review can be clicked and refused by the hub with 422.
    const editedWithComment = 'We ship in {~~Q3~>Q4~~}{>>move out a quarter<<}{>>extra note<<}.\n';
    const editor = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & {
      content: string;
    };
    editor.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: editedWithComment } })
    );
    await el.updateComplete;

    findButton(el, '.edit-footer sl-button', 'Submit Review')!.click();
    await settle(el);

    // 1. No DELETE /review-draft sent.
    expect(
      state.requests.some((r) => r.method === 'DELETE' && r.url.includes('/review-draft'))
    ).toBe(false);
    // 2. Draft rev unchanged.
    expect(state.draft?.rev).toBe(2);
    // 3. Buffer (marks, comments) stays intact.
    expect(editor.content).toBe(editedWithComment);
    // 4. beforeunload guard is still armed because the edit is unacknowledged.
    const unload422 = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unload422);
    expect(unload422.defaultPrevented).toBe(true);
    // 5. Error names the changed spans (reviewRejectedMessage).
    const alert422 = Array.from(el.shadowRoot!.querySelectorAll('sl-alert')).find((a) =>
      a.textContent?.includes('plan.md (line 1)')
    );
    expect(alert422).toBeDefined();

    // Now test stale_review refusal with the same unacknowledged buffer.
    state.finalizeResponse = () =>
      new Response(
        JSON.stringify({
          error: { code: 'stale_review', message: 'a newer version was published' },
        }),
        { status: 409, headers: { 'Content-Type': 'application/json' } }
      );

    findButton(el, '.edit-footer sl-button', 'Submit Review')!.click();
    await settle(el);

    expect(
      state.requests.some((r) => r.method === 'DELETE' && r.url.includes('/review-draft'))
    ).toBe(false);
    expect(state.draft?.rev).toBe(2);
    const internals = el as unknown as {
      reviewText: string;
      discardedReview: string | null;
      draftController?: { currentRev: number };
    };
    expect(internals.draftController?.currentRev).toBe(2);
    expect(internals.reviewText).toBe(editedWithComment);
    expect(internals.discardedReview).toBeNull();
    const liveEditor = el.shadowRoot!.querySelector('scion-code-editor.review-editor') as
      | (HTMLElement & { content: string })
      | null;
    expect(liveEditor).not.toBeNull();
    expect(liveEditor!.content).toBe(editedWithComment);
    const unloadStale = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unloadStale);
    expect(unloadStale.defaultPrevented).toBe(true);
    const alertStale = Array.from(el.shadowRoot!.querySelectorAll('sl-alert')).find((a) =>
      a.textContent?.includes('Your draft and edits are kept intact.')
    );
    expect(alertStale).toBeDefined();
  });

  it('drops the buffer and cancels draft activity on sign-out (ACCOUNT_TEARDOWN_EVENT) and writes nothing to browser storage (C8)', async () => {
    const lsSpy = vi.spyOn(Storage.prototype, 'setItem');
    const idbOpen = vi.fn();
    const cachesOpen = vi.fn();
    const origIdb = Object.getOwnPropertyDescriptor(globalThis, 'indexedDB');
    const origCaches = Object.getOwnPropertyDescriptor(globalThis, 'caches');
    Object.defineProperty(globalThis, 'indexedDB', {
      configurable: true,
      value: { open: idbOpen },
    });
    Object.defineProperty(globalThis, 'caches', {
      configurable: true,
      value: { open: cachesOpen },
    });

    try {
      const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
      mockHub(state);
      const el = await mountPage();

      findButton(el, '.header-actions sl-button', 'Review')!.click();
      await settle(el);

      el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
        new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } })
      );
      await el.updateComplete;
      findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
      await settle(el);

      // Sign-out clears Review mode and disarms beforeunload.
      window.dispatchEvent(new CustomEvent(ACCOUNT_TEARDOWN_EVENT));
      await el.updateComplete;

      expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).toBeNull();
      const unload = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
      window.dispatchEvent(unload);
      expect(unload.defaultPrevented).toBe(false);

      // C8: zero writes to localStorage, sessionStorage, IndexedDB, or Cache API.
      expect(lsSpy).not.toHaveBeenCalled();
      expect(idbOpen).not.toHaveBeenCalled();
      expect(cachesOpen).not.toHaveBeenCalled();
      expect(window.localStorage.length).toBe(0);
      expect(window.sessionStorage.length).toBe(0);
    } finally {
      if (origIdb) Object.defineProperty(globalThis, 'indexedDB', origIdb);
      else delete (globalThis as Record<string, unknown>).indexedDB;
      if (origCaches) Object.defineProperty(globalThis, 'caches', origCaches);
      else delete (globalThis as Record<string, unknown>).caches;
    }
  });

  it('settles an in-flight rev-0 draft PUT before Submit Review so DELETE ?rev=1 removes the draft (R1)', async () => {
    let releasePut!: () => void;
    const state: DraftServerState = {
      draft: null,
      currentSeq: 1,
      requests: [],
      holdPut: (res): Promise<Response> =>
        new Promise<Response>((resolve) => {
          releasePut = (): void => resolve(res);
        }),
    };
    mockHub(state);

    const el = await mountPage();
    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } })
    );
    await el.updateComplete;

    // Start rev-0 Save Draft PUT (committed on hub, HTTP response held in flight).
    findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
    await el.updateComplete;

    // Belt-and-braces: Submit Review is disabled while PUT is in flight.
    const submitBtn = findButton(el, '.edit-footer sl-button', 'Submit Review')!;
    expect(submitBtn.hasAttribute('disabled')).toBe(true);

    // Trigger saveReview() while the rev-0 PUT is in flight; quiesce() must
    // block publishFiles() until the in-flight PUT settles.
    const submitPromise = (el as unknown as { saveReview(): Promise<void> }).saveReview();
    await settle(el);
    expect(state.requests.some((r) => r.method === 'POST' && r.url.endsWith('/versions'))).toBe(
      false
    );

    releasePut();
    await submitPromise;
    await settle(el);

    const delReq = state.requests.find(
      (r) => r.method === 'DELETE' && r.url.includes('/review-draft')
    );
    expect(delReq?.url).toContain('?rev=1');
    expect(state.draft).toBeNull();
    expect(state.currentSeq).toBe(2);
  });

  it('Save draft and close awaits an in-flight PUT and closes Review mode cleanly (S2)', async () => {
    let releasePut!: () => void;
    const state: DraftServerState = {
      draft: null,
      currentSeq: 1,
      requests: [],
      holdPut: (res): Promise<Response> =>
        new Promise<Response>((resolve) => {
          releasePut = (): void => resolve(res);
        }),
    };
    mockHub(state);

    const el = await mountPage();
    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } })
    );
    await el.updateComplete;

    findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
    await el.updateComplete;

    // Click Cancel while PUT is in flight -> opens Cancel dialog.
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await el.updateComplete;
    const saveCloseBtn = el.shadowRoot!.querySelector<HTMLElement>('sl-button.draft-cancel-save')!;
    saveCloseBtn.click();
    releasePut();
    await settle(el);

    expect(state.draft?.rev).toBe(1);
    expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).toBeNull();
  });

  it('renders the polite draft status line in the graphical .g-toolbar layout too', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACT_REVIEW_EDITOR_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    const state: DraftServerState = {
      draft: {
        rev: 1,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: '',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 1,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    const status = el.shadowRoot!.querySelector('.g-toolbar .draft-status');
    expect(status?.getAttribute('aria-live')).toBe('polite');
    expect(status?.textContent).toContain('Draft saved');
    expect(findButton(el, '.edit-footer sl-button', 'Save Draft')).toBeDefined();
    expect(findButton(el, '.edit-footer sl-button', 'Submit Review')).toBeDefined();
  });
});

describe('ScionPageArtifactDetail review drafts (Phase 3)', () => {
  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    resetServerFlagStateForTests();
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('shows private Draft badge in History, version menu, and next to Resume draft without auto-entering Review mode (O1, A5)', async () => {
    const state: DraftServerState = {
      draft: {
        rev: 2,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'Draft note',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 2,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    // Parallel GET /review-draft happened on page load.
    expect(
      state.requests.filter((r) => r.method === 'GET' && r.url.includes('/review-draft'))
    ).toHaveLength(1);

    // O1: page load does NOT auto-enter Review mode.
    expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).toBeNull();

    // Review button reads "Resume draft" with a Draft badge next to it.
    expect(findButton(el, '.header-actions sl-button', 'Resume draft')).toBeDefined();
    const headerBadge = el.shadowRoot!.querySelector('.header-actions .review-draft-badge');
    expect(headerBadge?.textContent?.trim()).toBe('Draft');

    // History table shows the Draft chip only on the draft's base row (v1), not v2.
    const rows = Array.from(
      el.shadowRoot!.querySelectorAll('sl-tab-panel[name="history"] tbody tr')
    );
    expect(rows).toHaveLength(2);
    const v2Row = rows.find((r) => r.querySelector('strong')?.textContent === 'v2')!;
    const v1Row = rows.find((r) => r.querySelector('strong')?.textContent === 'v1')!;
    expect(v2Row.querySelector('.draft-chip-badge')).toBeNull();
    expect(v1Row.querySelector('.draft-chip-badge')?.textContent?.trim()).toBe('Draft');
    const chipTooltip = v1Row.querySelector('.draft-chip-trigger sl-tooltip');
    expect(chipTooltip?.getAttribute('content')).toMatch(
      /Your saved review draft, saved \d{2}:\d{2}/
    );
    expect(v1Row.querySelector('.draft-chip-resume')?.textContent).toContain('Resume');
    expect(v1Row.querySelector('.draft-chip-discard')?.textContent).toContain('Discard draft');

    // Header version menu shows Draft badge on v1 and Resume/Discard menu items.
    const menuItems = Array.from(el.shadowRoot!.querySelectorAll('.version-menu sl-menu-item'));
    const v1MenuItem = menuItems.find((m) => m.getAttribute('value') === '1')!;
    const v2MenuItem = menuItems.find((m) => m.getAttribute('value') === '2')!;
    expect(v1MenuItem.querySelector('.draft-chip-badge')?.textContent?.trim()).toBe('Draft');
    expect(v2MenuItem.querySelector('.draft-chip-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.version-menu-draft-resume')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.version-menu-draft-discard')).not.toBeNull();
  });

  it('hides all Draft badges when GET /review-draft returns null (non-author) or caller has no review access', async () => {
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    expect(findButton(el, '.header-actions sl-button', 'Review')).toBeDefined();
    expect(findButton(el, '.header-actions sl-button', 'Resume draft')).toBeUndefined();
    expect(el.shadowRoot!.querySelector('.review-draft-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.draft-chip-badge')).toBeNull();

    // Caller without review access makes no GET /review-draft request at all.
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    const noAccess: DraftServerState = {
      draft: null,
      currentSeq: 1,
      requests: [],
      canPublish: false,
      canReview: false,
    };
    mockHub(noAccess);
    await mountPage();
    expect(noAccess.requests.some((r) => r.url.includes('/review-draft'))).toBe(false);
  });

  it('shows no Draft badge and makes no GET /review-draft when the experiment is off (A6)', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: false,
    };
    const state: DraftServerState = {
      draft: {
        rev: 1,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: '',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 1,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    expect(state.requests.some((r) => r.url.includes('/review-draft'))).toBe(false);
    expect(findButton(el, '.header-actions sl-button', 'Review')).toBeDefined();
    expect(findButton(el, '.header-actions sl-button', 'Resume draft')).toBeUndefined();
    expect(el.shadowRoot!.querySelector('.review-draft-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.draft-chip-badge')).toBeNull();
  });

  it('resumes byte-exact from the History chip menu with no replace prompt and shows fail-closed notice in graphical view', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACT_REVIEW_EDITOR_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    const state: DraftServerState = {
      draft: {
        rev: 4,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'Byte-exact note',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 1,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    // Select "Resume" on the History row draft chip menu.
    const chipMenu = el.shadowRoot!.querySelector('.draft-chip-menu sl-menu')!;
    chipMenu.dispatchEvent(new CustomEvent('sl-select', { detail: { item: { value: 'resume' } } }));
    await settle(el);

    // No replace confirmation dialog; editor opens with byte-exact body and note.
    expect(el.shadowRoot!.querySelector('sl-dialog')).toBeNull();
    const internals = el as unknown as {
      reviewing: boolean;
      reviewText: string;
      reviewNote: string;
      draftController?: { currentRev: number };
    };
    expect(internals.reviewing).toBe(true);
    expect(internals.reviewText).toBe(MARKED_TEXT);
    expect(internals.reviewNote).toBe('Byte-exact note');
    expect(internals.draftController?.currentRev).toBe(4);

    // Fail-closed marks notice in graphical view (v3 §7.1).
    const hint = el.shadowRoot!.querySelector('.hint-strip');
    expect(hint?.textContent).toContain('read-only in the graphical view');
  });

  it('discards a draft from the History chip with confirmation and DELETE ?rev=', async () => {
    const state: DraftServerState = {
      draft: {
        rev: 5,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: '',
        carried: null,
        savedAt: '2026-11-21T18:05:00Z',
        createdAt: '2026-11-21T18:00:00Z',
      },
      currentSeq: 1,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    const chipMenu = el.shadowRoot!.querySelector('.draft-chip-menu sl-menu')!;
    chipMenu.dispatchEvent(
      new CustomEvent('sl-select', { detail: { item: { value: 'discard' } } })
    );
    await el.updateComplete;

    const dialog = el.shadowRoot!.querySelector('sl-dialog.draft-discard-dialog');
    expect(dialog).not.toBeNull();
    expect((dialog?.textContent ?? '').replace(/\s+/g, ' ')).toContain("can't be undone");

    // Cancel keeps the draft.
    dialog!.querySelector<HTMLElement>('sl-button.draft-discard-cancel')!.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('sl-dialog.draft-discard-dialog')).toBeNull();
    expect(state.draft?.rev).toBe(5);

    // Re-open and confirm discard.
    chipMenu.dispatchEvent(
      new CustomEvent('sl-select', { detail: { item: { value: 'discard' } } })
    );
    await el.updateComplete;
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.draft-discard-confirm')!.click();
    await settle(el);

    const delReq = state.requests.find(
      (r) => r.method === 'DELETE' && r.url.includes('/review-draft')
    );
    expect(delReq?.url).toContain('?rev=5');
    expect(state.draft).toBeNull();
    expect(el.shadowRoot!.querySelector('.draft-chip-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.review-draft-badge')).toBeNull();
    expect(findButton(el, '.header-actions sl-button', 'Review')).toBeDefined();
  });

  it('moves a stale-base draft to the current version with read-only carried text, preserving v1 until edited (v3 §7.4)', async () => {
    const V2_TEXT = 'We ship in Q3 with updated milestones.\n';
    const state: DraftServerState = {
      draft: {
        rev: 2,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'v1 note',
        carried: null,
        savedAt: '2026-11-21T17:00:00Z',
        createdAt: '2026-11-21T16:50:00Z',
      },
      currentSeq: 2,
      fileText: V2_TEXT,
      requests: [],
    };
    mockHub(state);
    const el = await mountPage();

    // Open stale-draft dialog via Resume draft.
    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('sl-dialog.stale-draft-dialog');
    expect(dialog).not.toBeNull();
    const moveBtn = dialog!.querySelector<HTMLElement>('sl-button.stale-draft-move')!;
    expect(moveBtn.textContent).toContain('Move my review to v2');

    // Click Move my review to v2.
    moveBtn.click();
    await settle(el);

    // Editor starts on v2 text, carried panel shows v1 draft read-only.
    const editor = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & { content: string };
    expect(editor.content).toBe(V2_TEXT);
    const carriedPanel = el.shadowRoot!.querySelector('sl-details.carried-review');
    expect(carriedPanel).not.toBeNull();
    expect(carriedPanel?.getAttribute('summary')).toContain('v1');
    expect(carriedPanel?.querySelector('pre')?.textContent).toBe(MARKED_TEXT);

    // Before typing: NO PUT sent; abandoning leaves the v1 draft untouched on the hub.
    expect(state.requests.some((r) => r.method === 'PUT' && r.url.includes('/review-draft'))).toBe(
      false
    );
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    expect(state.draft?.base).toBe(1);
    expect(state.draft?.body).toBe(MARKED_TEXT);

    // Move again, now type a mark on v2 and Save Draft.
    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.stale-draft-move')!.click();
    await settle(el);

    const v2Marked = 'We ship in {~~Q3~>Q4~~} with updated milestones.\n';
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: v2Marked } })
    );
    await el.updateComplete;
    findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
    await settle(el);

    expect(state.draft?.rev).toBe(3);
    expect(state.draft?.base).toBe(2);
    expect(state.draft?.body).toBe(v2Marked);
    expect(state.draft?.carried).toEqual({ base: 1, body: MARKED_TEXT });

    // Close and resume the moved draft: carried text is still shown read-only!
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    const resumedCarried = el.shadowRoot!.querySelector('sl-details.carried-review');
    expect(resumedCarried).not.toBeNull();
    expect(resumedCarried?.querySelector('pre')?.textContent).toBe(MARKED_TEXT);

    // Dismiss carried text and save: clears carried on the hub.
    resumedCarried!.querySelector<HTMLElement>('sl-button.carried-dismiss-btn')!.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('sl-details.carried-review')).toBeNull();
    findButton(el, '.edit-footer sl-button', 'Save Draft')!.click();
    await settle(el);
    expect(state.draft?.rev).toBe(4);
    expect(state.draft?.carried).toBeNull();
  });

  it('persists pending edits before moving after a stale_review refusal, and aborts the move if saving fails (R1)', async () => {
    const V2_TEXT = 'We ship in Q3 with updated milestones.\n';
    const unsavedEdit = 'We ship in {~~Q3~>Q4~~}{>>move out a quarter<<}{>>unsaved edit<<}.\n';
    const state: DraftServerState = {
      draft: {
        rev: 1,
        base: 1,
        entry: 'plan.md',
        body: MARKED_TEXT,
        note: 'Initial note',
        carried: null,
        savedAt: '2026-11-21T18:00:00Z',
        createdAt: '2026-11-21T17:55:00Z',
      },
      currentSeq: 1,
      requests: [],
      finalizeResponse: () =>
        new Response(
          JSON.stringify({
            error: { code: 'stale_review', message: 'a newer version was published' },
          }),
          { status: 409, headers: { 'Content-Type': 'application/json' } }
        ),
    };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Resume draft')!.click();
    await settle(el);

    // Make an unsaved edit, then attempt Submit Review which gets refused with stale_review.
    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: unsavedEdit } })
    );
    await el.updateComplete;

    findButton(el, '.edit-footer sl-button', 'Submit Review')!.click();
    await settle(el);

    const moveBtn = el.shadowRoot!.querySelector<HTMLElement>('sl-button.move-review-btn')!;
    expect(moveBtn).not.toBeNull();

    // Case 1: saveNow() fails (500) -> abort move, stay in v1 session with edits & beforeunload armed.
    state.nextPutResponse = () =>
      new Response(JSON.stringify({ error: { code: 'internal', message: 'db down' } }), {
        status: 500,
        headers: { 'Content-Type': 'application/json' },
      });
    moveBtn.click();
    await settle(el);

    expect(state.draft?.rev).toBe(1);
    expect(state.draft?.body).toBe(MARKED_TEXT);
    const editorAfterFail = el.shadowRoot!.querySelector(
      'scion-code-editor.review-editor'
    ) as HTMLElement & { content: string };
    expect(editorAfterFail.content).toBe(unsavedEdit);
    expect(el.shadowRoot!.querySelector('sl-details.carried-review')).toBeNull();
    const unload = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);

    // Case 2: hub is healthy and v2 is published -> Move saves pending edits on v1 first,
    // then loads v2 with the saved v1 text in carried-review; Cancel after move keeps v1 intact.
    delete state.nextPutResponse;
    state.currentSeq = 2;
    state.fileText = V2_TEXT;
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.move-review-btn')!.click();
    await settle(el);

    // Pending edits reached the hub on base 1 (rev 1 -> 2).
    expect(state.draft?.rev).toBe(2);
    expect(state.draft?.base).toBe(1);
    expect(state.draft?.body).toBe(unsavedEdit);

    // Editor now shows v2 text; carried panel shows the saved v1 text including the pending edit.
    const carried = el.shadowRoot!.querySelector('sl-details.carried-review');
    expect(carried).not.toBeNull();
    expect(carried?.querySelector('pre')?.textContent).toBe(unsavedEdit);

    // Cancelling the unwritten v2 move leaves the persisted v1 draft (with unsavedEdit) intact.
    findButton(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    expect(state.draft?.rev).toBe(2);
    expect(state.draft?.base).toBe(1);
    expect(state.draft?.body).toBe(unsavedEdit);
  });

  it('ignores an in-flight GET /review-draft that resolves after ACCOUNT_TEARDOWN_EVENT (O1)', async () => {
    let releaseGet!: (res: Response) => void;
    const base = artifact(1);
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes('/review-draft')) {
        return new Promise<Response>((resolve) => {
          releaseGet = resolve;
        });
      }
      if (url.includes('/files/plan.md')) {
        return Promise.resolve(new Response(BASE_TEXT, { status: 200 }));
      }
      if (url.includes('/versions')) {
        return Promise.resolve(
          new Response(JSON.stringify({ versions: [base.version], discarded: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify(base), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });

    const el = await mountPage();
    // Sign-out happens while GET /review-draft is still in flight.
    window.dispatchEvent(new CustomEvent(ACCOUNT_TEARDOWN_EVENT));
    await el.updateComplete;

    releaseGet(
      new Response(
        JSON.stringify({
          draft: {
            rev: 1,
            base: 1,
            entry: 'plan.md',
            body: MARKED_TEXT,
            note: '',
            carried: null,
            savedAt: '2026-11-21T18:05:00Z',
            createdAt: '2026-11-21T18:00:00Z',
          },
          currentSeq: 1,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await settle(el);

    expect(el.shadowRoot!.querySelector('.review-draft-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.draft-chip-badge')).toBeNull();
    expect(findButton(el, '.header-actions sl-button', 'Resume draft')).toBeUndefined();
  });
});
