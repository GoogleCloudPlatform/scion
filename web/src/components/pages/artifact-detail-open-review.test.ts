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
 * The artifact page side of the open-review check (ptone/scion#6849): the
 * review heartbeat while Review mode is open, and the Wait / Publish anyway
 * dialog when an edit publish is refused with 409 review_open.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import { requestBodyText, requestUrl } from '../../client/__fixtures__/request-url.js';
import { showToast } from '../../utils/toast.js';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

const ID = '4c1f0e8e-1c87-4d8a-9a2b-0f5f7f0b8b11';
const FLAG = 'hub.artifacts_open_review_check';

function meta(): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'user',
      ownerRef: 'u-1',
      title: 'Design',
      currentSeq: 1,
      createdAt: '2026-10-05T12:00:00Z',
      updatedAt: '2026-10-05T12:00:00Z',
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: 'design.md',
      totalBytes: 5,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path: 'design.md', size: 5, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    canPublish: true,
  };
}

interface Req {
  method: string;
  url: string;
  body: string;
}

/** Mocks the hub: reads answer the artifact; writes go to write(). */
function mockHub(write: (r: Req) => Response): Req[] {
  const reqs: Req[] = [];
  const m = meta();
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => Promise.resolve(answer(input, init)))
  );
  function answer(input: RequestInfo | URL, init?: RequestInit): Response {
    const url = requestUrl(input);
    const method = init?.method ?? 'GET';
    // A file upload sends a Blob, which requestBodyText rejects; its bytes are not checked here.
    const body = init?.body instanceof Blob ? '' : requestBodyText(init?.body);
    const r: Req = { method, url, body };
    reqs.push(r);
    if (method !== 'GET') return write(r);
    if (url.endsWith('/grants')) {
      return new Response(JSON.stringify({ grants: [], crossProjectSharing: false }));
    }
    if (url.startsWith('/api/v1/agents/') || url.startsWith('/api/v1/users')) {
      return new Response('{"error":{"code":"not_found","message":"nf"}}', { status: 404 });
    }
    if (/\/versions(\?|$)/.test(url)) {
      return new Response(
        JSON.stringify({ versions: [{ ...m.version, files: [] }], discarded: [] })
      );
    }
    if (url.includes('/files/')) return new Response('# Doc\n');
    return new Response(JSON.stringify(m), { headers: { 'Content-Type': 'application/json' } });
  }
  return reqs;
}

async function settle(el: ScionPageArtifactDetail, n = 30): Promise<void> {
  for (let i = 0; i < n; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(flags: Record<string, boolean>): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': true, ...flags };
  window.history.replaceState({}, '', `/projects/p-1/artifacts/${ID}`);
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = { path: `/projects/p-1/artifacts/${ID}` } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el, 20);
  return el;
}

function button(el: ScionPageArtifactDetail, sel: string, text: string): HTMLElement | undefined {
  return Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>(sel)).find((b) =>
    b.textContent!.includes(text)
  );
}

const sessionReqs = (reqs: Req[]): Req[] => reqs.filter((r) => r.url.includes('/review-sessions/'));

describe('artifact page: open reviews', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-detail.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.mocked(showToast).mockClear();
    delete window.__SCION_FEATURES__;
    resetPrincipalNames();
  });

  it('beats while Review mode is open and ends the session on Cancel', async () => {
    const reqs = mockHub((r) =>
      r.method === 'PUT' && r.url.includes('/review-sessions/')
        ? new Response(JSON.stringify({ currentSeq: 1 }))
        : new Response(null, { status: 204 })
    );
    const el = await mount({ [FLAG]: true });
    expect(sessionReqs(reqs)).toHaveLength(0);
    button(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);
    const beats = sessionReqs(reqs);
    expect(beats).toHaveLength(1);
    expect(beats[0].method).toBe('PUT');
    expect(beats[0].url).toMatch(
      new RegExp(`^/api/v1/artifacts/${ID}/review-sessions/[A-Za-z0-9_-]{22}$`)
    );
    expect(JSON.parse(beats[0].body)).toEqual({ base: 1, edited: false, dirty: false });
    button(el, '.edit-footer sl-button', 'Cancel')!.click();
    await settle(el);
    const all = sessionReqs(reqs);
    expect(all.map((r) => r.method)).toEqual(['PUT', 'DELETE']);
    expect(all[1].url).toBe(beats[0].url);
  });

  it('sends no heartbeat with the experiment off', async () => {
    const reqs = mockHub(() => new Response(null, { status: 204 }));
    const el = await mount({});
    button(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);
    expect(sessionReqs(reqs)).toHaveLength(0);
  });

  it('ends the session when the page goes away', async () => {
    const reqs = mockHub((r) =>
      r.method === 'PUT'
        ? new Response(JSON.stringify({ currentSeq: 1 }))
        : new Response(null, { status: 204 })
    );
    const el = await mount({ [FLAG]: true });
    button(el, '.header-actions sl-button', 'Review')!.click();
    await settle(el);
    el.remove();
    await new Promise((r) => setTimeout(r, 0));
    expect(sessionReqs(reqs).map((r) => r.method)).toEqual(['PUT', 'DELETE']);
  });

  const refusal = JSON.stringify({
    error: {
      code: 'review_open',
      message: '1 review of this artifact is open; publish anyway with force, or wait for it',
      details: {
        reviews: [
          { reviewer: 'Una User', lastActivityAt: new Date().toISOString(), unsavedChanges: true },
        ],
        truncated: false,
        base: 1,
      },
    },
  });

  async function editAndPublish(el: ScionPageArtifactDetail): Promise<void> {
    button(el, '.header-actions sl-button', 'Edit')!.click();
    await el.updateComplete;
    el.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', { detail: { content: '# Mine' } })
    );
    await el.updateComplete;
    button(el, '.edit-footer sl-button', 'Publish new version')!.click();
    await settle(el);
  }

  it('asks Wait or Publish anyway on review_open; Wait keeps the edit and publishes nothing', async () => {
    const reqs = mockHub(() => new Response(refusal, { status: 409 }));
    const el = await mount({ [FLAG]: true });
    await editAndPublish(el);
    const dialog = el.shadowRoot!.querySelector('sl-dialog.review-open-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog!.textContent).toContain('Una User is reviewing this version.');
    expect(dialog!.textContent).toContain('unsaved changes');
    expect(el.shadowRoot!.querySelector('sl-alert[variant="danger"]')).toBeNull();
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.review-open-wait')!.click();
    await settle(el);
    expect(el.shadowRoot!.querySelector('sl-dialog.review-open-dialog')).toBeNull();
    // Still editing, with the publisher's text.
    const editor = el.shadowRoot!.querySelector('scion-code-editor') as HTMLElement & {
      content: string;
    };
    expect(editor.content).toBe('# Mine');
    const writes = reqs.filter((r) => r.method !== 'GET');
    expect(writes.map((r) => `${r.method} ${r.url}`)).toEqual([
      `POST /api/v1/artifacts/${ID}/versions`,
    ]);
    expect(JSON.parse(writes[0].body)).not.toHaveProperty('force');
  });

  it('Publish anyway publishes with force and shows the warning', async () => {
    let forced = false;
    const reqs = mockHub((r) => {
      if (r.method === 'POST' && r.url.endsWith('/versions')) {
        if (!JSON.parse(r.body).force) return new Response(refusal, { status: 409 });
        forced = true;
        return new Response(
          JSON.stringify({
            artifact: meta().artifact,
            version: { seq: 2 },
            upload: { required: ['design.md'] },
            warnings: ['published while 1 review was open (Una User)'],
          }),
          { status: 201 }
        );
      }
      if (r.method === 'PUT') return new Response(null, { status: 204 });
      return new Response(JSON.stringify(meta()), { status: 200 });
    });
    const el = await mount({ [FLAG]: true });
    await editAndPublish(el);
    el.shadowRoot!.querySelector<HTMLElement>('sl-button.review-open-force')!.click();
    // The forced publish hashes, uploads and finalizes; wait for the finalize
    // itself rather than a fixed number of ticks, which a loaded host can outrun.
    await vi.waitFor(
      () => expect(reqs.some((r) => r.url.endsWith('/versions/2/finalize'))).toBe(true),
      { timeout: 10000 }
    );
    await settle(el);
    expect(forced).toBe(true);
    expect(el.shadowRoot!.querySelector('sl-dialog.review-open-dialog')).toBeNull();
    expect(vi.mocked(showToast)).toHaveBeenCalledWith(
      'published while 1 review was open (Una User)',
      'warning'
    );
    // Above vitest's default 5000 ms per test, so the 10000 ms wait can finish.
  }, 20000);

  it('names several reviewers', async () => {
    const many = JSON.stringify({
      error: {
        code: 'review_open',
        details: {
          reviews: [
            { reviewer: 'Una User', lastActivityAt: '', unsavedChanges: false },
            { reviewer: 'a reviewer', lastActivityAt: '', unsavedChanges: true },
          ],
          truncated: true,
        },
      },
    });
    mockHub(() => new Response(many, { status: 409 }));
    const el = await mount({ [FLAG]: true });
    await editAndPublish(el);
    const text = el.shadowRoot!.querySelector('sl-dialog.review-open-dialog')!.textContent!;
    expect(text).toContain('Una User and others are reviewing this version.');
    expect(text).toContain('and others');
  });
});
