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
  nextPutResponse?: () => Response;
  nextDeleteResponse?: () => Response;
  finalizeResponse?: () => Response;
}

function mockHub(state: DraftServerState): void {
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
        if (state.nextPutResponse) return reply(state.nextPutResponse());
        const parsed = JSON.parse(rawBody ?? '{}') as {
          rev: number;
          base: number;
          entry: string;
          body: string;
          note: string;
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
          carried: null,
          savedAt,
          createdAt: state.draft?.createdAt ?? savedAt,
        };
        return reply(
          new Response(
            JSON.stringify({ rev: state.draft.rev, savedAt, currentSeq: state.currentSeq }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
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
      return reply(new Response(BASE_TEXT, { status: 200 }));
    }
    if (url.includes('/versions')) {
      return reply(
        new Response(JSON.stringify({ versions: [meta.version], discarded: [] }), {
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

    findButton(el, '.header-actions sl-button', 'Review')!.click();
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

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('sl-dialog.stale-draft-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog?.textContent).toContain('version 1');
    expect(dialog?.textContent).toContain('version 2');

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
    findButton(el, '.header-actions sl-button', 'Review')!.click();
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

    findButton(el, '.header-actions sl-button', 'Review')!.click();
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
    expect(editor.content).toBe(editedWithComment);
    const unloadStale = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unloadStale);
    expect(unloadStale.defaultPrevented).toBe(true);
    const alertStale = Array.from(el.shadowRoot!.querySelectorAll('sl-alert')).find((a) =>
      a.textContent?.includes('newer version was published')
    );
    expect(alertStale).toBeDefined();
  });

  it('drops the buffer and cancels draft activity on sign-out (ACCOUNT_TEARDOWN_EVENT) and writes nothing to browser storage (C8)', async () => {
    const lsSpy = vi.spyOn(Storage.prototype, 'setItem');
    const state: DraftServerState = { draft: null, currentSeq: 1, requests: [] };
    mockHub(state);
    const el = await mountPage();

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    el.shadowRoot!.querySelector('scion-code-editor.review-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } })
    );
    await el.updateComplete;

    // Sign-out clears Review mode and disarms beforeunload.
    window.dispatchEvent(new CustomEvent(ACCOUNT_TEARDOWN_EVENT));
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('scion-code-editor.review-editor')).toBeNull();
    const unload = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(false);

    // C8: zero browser storage writes.
    expect(lsSpy).not.toHaveBeenCalled();
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
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

    findButton(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);

    const status = el.shadowRoot!.querySelector('.g-toolbar .draft-status');
    expect(status?.getAttribute('aria-live')).toBe('polite');
    expect(status?.textContent).toContain('Draft saved');
    expect(findButton(el, '.edit-footer sl-button', 'Save Draft')).toBeDefined();
    expect(findButton(el, '.edit-footer sl-button', 'Submit Review')).toBeDefined();
  });
});
