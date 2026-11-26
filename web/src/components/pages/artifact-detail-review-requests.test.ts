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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import './artifact-detail.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts.js';
import type { ArtifactResponse, ReviewRequestInfo } from '../../client/artifacts.js';
import {
  ARTIFACTS_REVIEW_REQUESTS_FLAG,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const ARTIFACT_ID = '22222222-2222-2222-2222-222222222222';
const ENTRY_TEXT = '# Launch Plan\n\nShip in Q3.\n';

const OPEN_REQUEST: ReviewRequestInfo = {
  id: 'rr-1',
  reviewer: { ref: 'u-bob', displayName: 'Bob' },
  requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
  requestedSeq: 2,
  note: 'Please check section 2',
  access: 'existing',
  state: 'open',
  createdAt: '2026-11-25T10:00:00Z',
};

function artifactResp(overrides: Partial<ArtifactResponse> = {}): ArtifactResponse {
  return {
    artifact: {
      id: ARTIFACT_ID,
      ref: `scion://artifact/${ARTIFACT_ID}`,
      scopeKind: 'project',
      scopeRef: PROJECT_ID,
      ownerKind: 'user',
      ownerRef: 'u-alice',
      title: 'Launch Plan',
      currentSeq: 2,
      createdAt: '2026-11-25T00:00:00Z',
      updatedAt: '2026-11-25T00:00:00Z',
    },
    version: {
      seq: 2,
      ref: `scion://artifact/${ARTIFACT_ID}@2`,
      kind: 'publish',
      entryPath: 'plan.md',
      totalBytes: ENTRY_TEXT.length,
      fileCount: 1,
      createdByKind: 'user',
      createdByRef: 'u-alice',
      createdAt: '2026-11-25T00:00:00Z',
      state: 'ready',
      files: [
        {
          path: 'plan.md',
          size: ENTRY_TEXT.length,
          sha256: 'abc',
          mediaType: 'text/markdown',
        },
      ],
    },
    canPublish: false,
    canReview: true,
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('artifact-detail: review requests (P1)', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
    window.history.replaceState(null, '', `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  function setupFetch(
    resp: ArtifactResponse,
    dismissHandler?: (init?: RequestInit) => Response
  ): void {
    fetchMock.mockImplementation((input: string | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}`) {
        return Promise.resolve(jsonResponse(resp));
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/versions`) {
        return Promise.resolve(jsonResponse({ versions: resp.version ? [resp.version] : [] }));
      }
      if (url.includes('/files/plan.md')) {
        return Promise.resolve(new Response(ENTRY_TEXT, { status: 200 }));
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/review-request/dismiss`) {
        if (dismissHandler) return Promise.resolve(dismissHandler(init));
        return Promise.resolve(
          jsonResponse({
            request: {
              ...OPEN_REQUEST,
              state: 'dismissed',
              responseNote: 'Out of scope',
              resolvedAt: '2026-11-25T11:00:00Z',
            },
            authorNotified: true,
          })
        );
      }
      return Promise.resolve(jsonResponse({}, 404));
    });
  }

  async function mount(
    resp: ArtifactResponse,
    opts: {
      reviewRequestsOn: boolean;
      userId?: string;
      dismissHandler?: (init?: RequestInit) => Response;
    }
  ): Promise<ScionPageArtifactDetail> {
    localStorage.setItem(`scion:feature:${ARTIFACTS_FLAG}`, 'true');
    if (opts.reviewRequestsOn) {
      localStorage.setItem(`scion:feature:${ARTIFACTS_REVIEW_REQUESTS_FLAG}`, 'true');
    }
    setupFetch(resp, opts.dismissHandler);
    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      path: `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
      title: 'Launch Plan',
      user: { id: opts.userId ?? 'u-bob', email: 'bob@example.com', name: 'Bob' },
    } as ScionPageArtifactDetail['pageData'];
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  it('hides Request review button, header badge, and reviewer banner when gate is off', async () => {
    const el = await mount(
      artifactResp({
        canPublish: true,
        reviewState: { requestedOfMe: OPEN_REQUEST },
      }),
      { reviewRequestsOn: false }
    );
    expect(el.shadowRoot!.querySelector('.request-review-btn')).toBeNull();
    expect(el.shadowRoot!.querySelector('.review-requested-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).toBeNull();
  });

  it('shows Request review button only for publishers when gate is on', async () => {
    const publisher = await mount(artifactResp({ canPublish: true, canReview: false }), {
      reviewRequestsOn: true,
      userId: 'u-alice',
    });
    const btn = publisher.shadowRoot!.querySelector('.request-review-btn') as HTMLElement;
    expect(btn).not.toBeNull();
    expect(btn.textContent?.trim()).toBe('Request review');
    btn.click();
    await settle(publisher);
    const dialog = publisher.shadowRoot!.querySelector(
      'scion-artifact-request-review-dialog'
    ) as HTMLElement & { open: boolean };
    expect(dialog).not.toBeNull();
    expect(dialog.open).toBe(true);
    publisher.remove();

    const reviewerOnly = await mount(artifactResp({ canPublish: false, canReview: true }), {
      reviewRequestsOn: true,
      userId: 'u-bob',
    });
    expect(reviewerOnly.shadowRoot!.querySelector('.request-review-btn')).toBeNull();
  });

  it('renders reviewer banner and header badge, dismisses with note, and clears both without reload', async () => {
    const el = await mount(
      artifactResp({
        canPublish: false,
        canReview: true,
        reviewState: { requestedOfMe: OPEN_REQUEST },
      }),
      { reviewRequestsOn: true, userId: 'u-bob' }
    );

    const badge = el.shadowRoot!.querySelector('.review-requested-badge');
    expect(badge).not.toBeNull();
    expect(badge?.getAttribute('variant')).toBe('primary');
    expect(badge?.textContent?.trim()).toBe('Review requested');

    const banner = el.shadowRoot!.querySelector('.reviewer-request-banner');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('Alice asked you to review this (v2).');
    expect(banner?.textContent).toContain('Please check section 2');
    expect(banner?.querySelector('.banner-start-review-btn')?.textContent?.trim()).toBe(
      'Start review'
    );
    const dismissBtn = banner?.querySelector('.banner-dismiss-request-btn') as HTMLElement;
    expect(dismissBtn?.textContent?.trim()).toBe('Dismiss…');

    // Open Dismiss dialog.
    dismissBtn.click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('.dismiss-request-dialog')!;
    expect(dialog).not.toBeNull();
    expect(dialog.textContent).toContain('The author will see your note.');
    expect(dialog.textContent).toContain('0 / 1000');

    const textarea = dialog.querySelector('.dismiss-request-note') as HTMLElement & {
      value: string;
    };
    const confirmBtn = dialog.querySelector('.dismiss-request-confirm') as HTMLElement;

    // Over 1000 runes disables confirm.
    textarea.value = 'x'.repeat(1001);
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el);
    expect(dialog.textContent).toContain('1001 / 1000');
    expect(confirmBtn.hasAttribute('disabled')).toBe(true);

    // Valid note submits dismiss and clears banner + badge without reload.
    textarea.value = 'Out of scope this week';
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el);
    expect(confirmBtn.hasAttribute('disabled')).toBe(false);

    const callUrl = (input: string | URL | Request): string =>
      typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;

    const getCallsBefore = fetchMock.mock.calls.filter(
      (c) => callUrl(c[0]) === `/api/v1/artifacts/${ARTIFACT_ID}`
    ).length;

    confirmBtn.click();
    await settle(el);

    const dismissCall = fetchMock.mock.calls.find(
      (c) => callUrl(c[0]) === `/api/v1/artifacts/${ARTIFACT_ID}/review-request/dismiss`
    );
    expect(dismissCall).toBeDefined();
    expect(JSON.parse((dismissCall![1] as RequestInit).body as string)).toEqual({
      note: 'Out of scope this week',
    });

    // Banner, header badge, and dialog are all cleared without refetching the artifact.
    expect(el.shadowRoot!.querySelector('.dismiss-request-dialog')).toBeNull();
    expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).toBeNull();
    expect(el.shadowRoot!.querySelector('.review-requested-badge')).toBeNull();
    const getCallsAfter = fetchMock.mock.calls.filter(
      (c) => callUrl(c[0]) === `/api/v1/artifacts/${ARTIFACT_ID}`
    ).length;
    expect(getCallsAfter).toBe(getCallsBefore);
  });

  it('clears banner and badge on 404 or 409 request_closed and informs the user', async () => {
    for (const [status, body] of [
      [404, { error: { code: 'not_found', message: 'no open review request' } }],
      [409, { error: { code: 'request_closed', message: 'review request already closed' } }],
    ] as const) {
      const el = await mount(
        artifactResp({
          canPublish: false,
          canReview: true,
          reviewState: { requestedOfMe: OPEN_REQUEST },
        }),
        {
          reviewRequestsOn: true,
          userId: 'u-bob',
          dismissHandler: () => jsonResponse(body, status),
        }
      );

      expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).not.toBeNull();
      expect(el.shadowRoot!.querySelector('.review-requested-badge')).not.toBeNull();

      (el.shadowRoot!.querySelector('.banner-dismiss-request-btn') as HTMLElement).click();
      await settle(el);
      (el.shadowRoot!.querySelector('.dismiss-request-confirm') as HTMLElement).click();
      await settle(el);

      expect(el.shadowRoot!.querySelector('.dismiss-request-dialog')).toBeNull();
      expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).toBeNull();
      expect(el.shadowRoot!.querySelector('.review-requested-badge')).toBeNull();
      const notice = el.shadowRoot!.querySelector('.review-notice');
      expect(notice).not.toBeNull();
      expect(notice?.textContent).toContain('This review request was already closed.');
      el.remove();
    }
  });

  it('keeps dismiss dialog open with an error alert on server errors', async () => {
    const el = await mount(
      artifactResp({
        canPublish: false,
        canReview: true,
        reviewState: { requestedOfMe: OPEN_REQUEST },
      }),
      {
        reviewRequestsOn: true,
        userId: 'u-bob',
        dismissHandler: () =>
          jsonResponse({ error: { code: 'unavailable', message: 'try again later' } }, 503),
      }
    );

    (el.shadowRoot!.querySelector('.banner-dismiss-request-btn') as HTMLElement).click();
    await settle(el);
    (el.shadowRoot!.querySelector('.dismiss-request-confirm') as HTMLElement).click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('.dismiss-request-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog?.textContent).toContain('try again later');
    expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.review-requested-badge')).not.toBeNull();
  });
});

describe('artifact-detail: review requests panel, withdraw, carry-forward & submit (P2a)', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  const callUrl = (input: string | URL | Request): string =>
    typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
    window.history.replaceState(null, '', `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  async function mountP2a(opts: {
    resp: ArtifactResponse;
    reviewRequestsOn?: boolean;
    userId?: string;
    listRequests?: () => ReviewRequestInfo[];
    withdrawHandler?: (rid: string) => Response;
    dismissHandler?: () => Response;
    onGetArtifact?: () => ArtifactResponse;
    onFinalize?: () => ArtifactResponse;
  }): Promise<ScionPageArtifactDetail> {
    localStorage.setItem(`scion:feature:${ARTIFACTS_FLAG}`, 'true');
    if (opts.reviewRequestsOn !== false) {
      localStorage.setItem(`scion:feature:${ARTIFACTS_REVIEW_REQUESTS_FLAG}`, 'true');
    }
    fetchMock.mockImplementation((input: string | URL | Request, init?: RequestInit) => {
      const url = callUrl(input);
      const method = (init?.method ?? 'GET').toUpperCase();
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}` && method === 'GET') {
        const current = opts.onGetArtifact ? opts.onGetArtifact() : opts.resp;
        return Promise.resolve(jsonResponse(current));
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/versions` && method === 'GET') {
        const current = opts.onGetArtifact ? opts.onGetArtifact() : opts.resp;
        return Promise.resolve(
          jsonResponse({ versions: current.version ? [current.version] : [] })
        );
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/versions` && method === 'POST') {
        return Promise.resolve(
          jsonResponse({
            artifact: opts.resp.artifact,
            version: {
              ...opts.resp.version!,
              seq: opts.resp.artifact.currentSeq + 1,
              kind: 'review',
              state: 'pending',
            },
            upload: { required: [] },
          })
        );
      }
      if (url.endsWith('/finalize') && method === 'POST') {
        const fin = opts.onFinalize
          ? opts.onFinalize()
          : {
              ...opts.resp,
              artifact: { ...opts.resp.artifact, currentSeq: opts.resp.artifact.currentSeq + 1 },
              version: {
                ...opts.resp.version!,
                seq: opts.resp.artifact.currentSeq + 1,
                kind: 'review',
              },
            };
        return Promise.resolve(jsonResponse(fin));
      }
      if (url.includes('/files/plan.md') && method === 'GET') {
        return Promise.resolve(new Response(ENTRY_TEXT, { status: 200 }));
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/review-requests` && method === 'GET') {
        return Promise.resolve(
          jsonResponse({ requests: opts.listRequests ? opts.listRequests() : [] })
        );
      }
      const withdrawMatch = url.match(
        new RegExp(`^/api/v1/artifacts/${ARTIFACT_ID}/review-requests/([^/]+)/withdraw$`)
      );
      if (withdrawMatch && method === 'POST') {
        if (opts.withdrawHandler) {
          return Promise.resolve(opts.withdrawHandler(decodeURIComponent(withdrawMatch[1])));
        }
        return Promise.resolve(
          jsonResponse({
            request: {
              ...OPEN_REQUEST,
              id: decodeURIComponent(withdrawMatch[1]),
              state: 'withdrawn',
              resolvedAt: '2026-11-26T12:00:00Z',
            },
          })
        );
      }
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}/review-request/dismiss` && method === 'POST') {
        if (opts.dismissHandler) return Promise.resolve(opts.dismissHandler());
        return Promise.resolve(
          jsonResponse({
            request: {
              ...OPEN_REQUEST,
              state: 'dismissed',
              responseNote: 'Busy',
              resolvedAt: '2026-11-26T12:00:00Z',
            },
            authorNotified: true,
          })
        );
      }
      return Promise.resolve(jsonResponse({}, 404));
    });

    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      path: `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
      title: 'Launch Plan',
      user: { id: opts.userId ?? 'u-alice', email: 'alice@example.com', name: 'Alice' },
    } as ScionPageArtifactDetail['pageData'];
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  it('hides the Review requests panel when gate is off or viewer cannot publish', async () => {
    const gatedOff = await mountP2a({
      resp: artifactResp({ canPublish: true, canManage: true }),
      reviewRequestsOn: false,
      listRequests: () => [OPEN_REQUEST],
    });
    expect(gatedOff.shadowRoot!.querySelector('.review-requests-panel')).toBeNull();
    gatedOff.remove();

    const nonPublisher = await mountP2a({
      resp: artifactResp({ canPublish: false, canReview: true }),
      reviewRequestsOn: true,
      userId: 'u-bob',
      listRequests: () => [OPEN_REQUEST],
    });
    expect(nonPublisher.shadowRoot!.querySelector('.review-requests-panel')).toBeNull();
  });

  it('renders panel states: in draft, no access, carry-forward, dismiss note, fulfilled, and Withdraw', async () => {
    const v4Resp = artifactResp({
      canPublish: true,
      canManage: true,
      artifact: {
        ...artifactResp().artifact,
        currentSeq: 4,
      },
      version: {
        ...artifactResp().version!,
        seq: 4,
        ref: `scion://artifact/${ARTIFACT_ID}@4`,
      },
    });
    const requests: ReviewRequestInfo[] = [
      {
        id: 'rr-draft',
        reviewer: { ref: 'u-bob', displayName: 'Bob' },
        requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
        requestedSeq: 2,
        access: 'existing',
        state: 'open',
        createdAt: '2026-11-25T10:00:00Z',
        reviewerCanReview: true,
        inDraft: true,
      },
      {
        id: 'rr-noaccess',
        reviewer: { ref: 'u-carol', displayName: 'Carol' },
        requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
        requestedSeq: 4,
        access: 'existing',
        state: 'open',
        createdAt: '2026-11-25T11:00:00Z',
        reviewerCanReview: false,
        inDraft: false,
      },
      {
        id: 'rr-dismissed',
        reviewer: { ref: 'u-dave', displayName: 'Dave' },
        requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
        requestedSeq: 2,
        access: 'existing',
        state: 'dismissed',
        responseNote: 'Out on PTO this week',
        createdAt: '2026-11-25T09:00:00Z',
        resolvedAt: '2026-11-25T12:00:00Z',
      },
      {
        id: 'rr-fulfilled',
        reviewer: { ref: 'u-eve', displayName: 'Eve' },
        requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
        requestedSeq: 2,
        resolvedSeq: 3,
        access: 'raised',
        state: 'fulfilled',
        createdAt: '2026-11-25T08:00:00Z',
        resolvedAt: '2026-11-25T13:00:00Z',
      },
    ];

    const el = await mountP2a({
      resp: v4Resp,
      userId: 'u-alice',
      listRequests: () => requests,
    });

    const panel = el.shadowRoot!.querySelector('.review-requests-panel');
    expect(panel).not.toBeNull();
    expect(panel!.querySelector('.rr-open-count')?.textContent?.trim()).toBe('2 open');

    const rows = [...panel!.querySelectorAll('.review-request-row')];
    expect(rows).toHaveLength(4);

    // Row 0: Bob, open, in draft, carry-forward v2 -> v4, Withdraw button.
    expect(rows[0].textContent).toContain('Bob');
    expect(rows[0].querySelector('.rr-state-badge')?.textContent?.trim()).toBe('Open');
    expect(rows[0].querySelector('.rr-in-draft')?.textContent?.trim()).toBe('in draft');
    expect(rows[0].querySelector('.rr-no-access')).toBeNull();
    expect(rows[0].querySelector('.rr-version')?.textContent?.trim()).toBe(
      'requested on v2 · latest v4'
    );
    expect(rows[0].querySelector('.rr-withdraw-btn')).not.toBeNull();

    // Row 1: Carol, open, no access, not in draft, on latest v4 (no carry-forward suffix).
    expect(rows[1].textContent).toContain('Carol');
    expect(rows[1].querySelector('.rr-in-draft')).toBeNull();
    expect(rows[1].querySelector('.rr-no-access')?.textContent?.trim()).toBe('no access');
    expect(rows[1].querySelector('.rr-version')?.textContent?.trim()).toBe('requested on v4');
    expect(rows[1].querySelector('.rr-withdraw-btn')).not.toBeNull();

    // Row 2: Dave, dismissed, dismiss note shown, no Withdraw button.
    expect(rows[2].textContent).toContain('Dave');
    expect(rows[2].querySelector('.rr-state-badge')?.textContent?.trim()).toBe('Dismissed');
    expect(rows[2].querySelector('.rr-dismiss-note')?.textContent).toContain(
      'Out on PTO this week'
    );
    expect(rows[2].querySelector('.rr-withdraw-btn')).toBeNull();

    // Row 3: Eve, fulfilled on v3, no Withdraw button.
    expect(rows[3].textContent).toContain('Eve');
    expect(rows[3].querySelector('.rr-state-badge')?.textContent?.trim()).toBe('Fulfilled');
    expect(rows[3].querySelector('.rr-version')?.textContent).toContain('reviewed in v3');
    expect(rows[3].querySelector('.rr-withdraw-btn')).toBeNull();
  });

  it('shows carry-forward in the reviewer banner when latest version is newer than requestedSeq', async () => {
    const v4Resp = artifactResp({
      canPublish: false,
      canReview: true,
      artifact: { ...artifactResp().artifact, currentSeq: 4 },
      version: { ...artifactResp().version!, seq: 4, ref: `scion://artifact/${ARTIFACT_ID}@4` },
      reviewState: { requestedOfMe: { ...OPEN_REQUEST, requestedSeq: 2 } },
    });
    const el = await mountP2a({ resp: v4Resp, userId: 'u-bob' });
    const banner = el.shadowRoot!.querySelector('.reviewer-request-banner');
    expect(banner).not.toBeNull();
    expect(banner!.textContent).toContain('requested on v2 · latest v4');
  });

  it('withdraws an open request and refreshes the panel, and refreshes after create and dismiss', async () => {
    let items: ReviewRequestInfo[] = [
      { ...OPEN_REQUEST, id: 'rr-1', state: 'open', reviewerCanReview: true },
    ];
    const el = await mountP2a({
      resp: artifactResp({
        canPublish: true,
        canManage: true,
        reviewState: { requestedOfMe: { ...OPEN_REQUEST, id: 'rr-self' } },
      }),
      userId: 'u-alice',
      listRequests: () => items,
      withdrawHandler: (rid) => {
        items = items.map((r) => (r.id === rid ? { ...r, state: 'withdrawn' } : r));
        return jsonResponse({ request: items[0] });
      },
      dismissHandler: () => {
        items = [
          ...items,
          {
            ...OPEN_REQUEST,
            id: 'rr-self',
            state: 'dismissed',
            responseNote: 'Cannot review',
          },
        ];
        return jsonResponse({
          request: items[items.length - 1],
          authorNotified: true,
        });
      },
    });

    // 1. Withdraw refreshes panel.
    const withdrawBtn = el.shadowRoot!.querySelector('.rr-withdraw-btn') as HTMLElement;
    expect(withdrawBtn).not.toBeNull();
    withdrawBtn.click();
    await settle(el);

    const withdrawCall = fetchMock.mock.calls.find(
      (c) =>
        callUrl(c[0]) === `/api/v1/artifacts/${ARTIFACT_ID}/review-requests/rr-1/withdraw` &&
        (c[1]?.method ?? 'GET').toUpperCase() === 'POST'
    );
    expect(withdrawCall).toBeDefined();
    expect(el.shadowRoot!.querySelector('.rr-withdraw-btn')).toBeNull();
    expect(el.shadowRoot!.querySelector('.rr-state-badge')?.textContent?.trim()).toBe('Withdrawn');

    // 2. Creating a request (dialog event) refreshes panel.
    items = [
      {
        ...OPEN_REQUEST,
        id: 'rr-new',
        reviewer: { ref: 'u-carol', displayName: 'Carol' },
        state: 'open',
      },
      ...items,
    ];
    const dialog = el.shadowRoot!.querySelector('scion-artifact-request-review-dialog')!;
    dialog.dispatchEvent(
      new CustomEvent('artifact-review-requested', {
        detail: { requests: [] },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);
    expect(el.shadowRoot!.querySelector('.review-requests-panel')?.textContent).toContain('Carol');

    // 3. Dismissing a request as a publisher also refreshes the panel.
    (el.shadowRoot!.querySelector('.banner-dismiss-request-btn') as HTMLElement).click();
    await settle(el);
    (el.shadowRoot!.querySelector('.dismiss-request-confirm') as HTMLElement).click();
    await settle(el);
    expect(el.shadowRoot!.querySelector('.review-requests-panel')?.textContent).toContain(
      'Cannot review'
    );
  });

  it('clears the reviewer banner and header badge after submitting a review without a reload', async () => {
    let submitted = false;
    const baseResp = artifactResp({
      canPublish: false,
      canReview: true,
      reviewState: { requestedOfMe: OPEN_REQUEST },
    });
    const el = await mountP2a({
      resp: baseResp,
      userId: 'u-bob',
      onGetArtifact: () =>
        submitted
          ? artifactResp({
              canPublish: false,
              canReview: true,
              artifact: { ...baseResp.artifact, currentSeq: 3 },
              version: { ...baseResp.version!, seq: 3, kind: 'review' },
              reviewState: { requestedOfMe: null },
            })
          : baseResp,
      onFinalize: () => {
        submitted = true;
        // Real hub handleFinalize omits reviewState; clearing relies on the
        // local clear + showCurrentVersion() refetch path.
        return artifactResp({
          canPublish: false,
          canReview: true,
          artifact: { ...baseResp.artifact, currentSeq: 3 },
          version: { ...baseResp.version!, seq: 3, kind: 'review' },
        });
      },
    });

    // Banner and badge are visible before submitting.
    expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.review-requested-badge')).not.toBeNull();

    // Start review from the banner button.
    (el.shadowRoot!.querySelector('.banner-start-review-btn') as HTMLElement).click();
    await settle(el);

    // Add a CriticMarkup comment in the source review editor and submit.
    const editor = el.shadowRoot!.querySelector('scion-code-editor.review-editor')!;
    editor.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: `${ENTRY_TEXT}{>>Looks good!<<}` },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el);

    const saveBtn = [...el.shadowRoot!.querySelectorAll('.edit-footer sl-button')].find((b) =>
      b.textContent?.includes('Save review')
    ) as HTMLElement;
    expect(saveBtn).toBeDefined();
    saveBtn.click();
    await settle(el);

    // Fulfilment cleared the banner and badge without a page reload.
    expect(submitted).toBe(true);
    expect(el.shadowRoot!.querySelector('.reviewer-request-banner')).toBeNull();
    expect(el.shadowRoot!.querySelector('.review-requested-badge')).toBeNull();
  });
});
