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
    opts: { reviewRequestsOn: boolean; userId?: string }
  ): Promise<ScionPageArtifactDetail> {
    localStorage.setItem(`scion:feature:${ARTIFACTS_FLAG}`, 'true');
    if (opts.reviewRequestsOn) {
      localStorage.setItem(`scion:feature:${ARTIFACTS_REVIEW_REQUESTS_FLAG}`, 'true');
    }
    setupFetch(resp);
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
});
