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
import type { ArtifactResponse } from '../../client/artifacts.js';
import {
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const ARTIFACT_ID = '22222222-2222-2222-2222-222222222222';
const ENTRY_TEXT = '# Launch Plan\n\nShip in Q3.\n';

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
      createdAt: '2026-11-26T00:00:00Z',
      updatedAt: '2026-11-26T00:00:00Z',
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
      createdAt: '2026-11-26T00:00:00Z',
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
  for (let i = 0; i < 8; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('artifact-detail: review drafts state (P2c)', () => {
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
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  function mockDetail(resp: ArtifactResponse): void {
    fetchMock.mockImplementation((input: string | URL) => {
      const urlStr = String(input);
      if (urlStr === `/api/v1/artifacts/${ARTIFACT_ID}`) {
        return Promise.resolve(jsonResponse(resp));
      }
      if (urlStr.includes('/versions?')) {
        return Promise.resolve(jsonResponse({ versions: [resp.version] }));
      }
      if (urlStr.includes('/files/plan.md')) {
        return Promise.resolve(new Response(ENTRY_TEXT, { status: 200 }));
      }
      if (urlStr.endsWith('/review-draft')) {
        return Promise.resolve(jsonResponse({ draft: null, currentSeq: 2 }));
      }
      return Promise.resolve(jsonResponse({}));
    });
  }

  it('shows neutral Review in draft header badge when reviewState.myDraft is set and gate is on', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    mockDetail(
      artifactResp({
        reviewState: {
          requestedOfMe: null,
          myDraft: { base: 2, savedAt: '2026-11-26T10:00:00Z' },
        },
      })
    );

    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      user: { id: 'u-bob', email: 'bob@test', name: 'Bob', role: 'member' },
    } as never;
    document.body.appendChild(el);
    await settle(el);

    const badge = el.shadowRoot!.querySelector('sl-badge.review-in-draft-badge');
    expect(badge).not.toBeNull();
    expect(badge!.getAttribute('variant')).toBe('neutral');
    expect(badge!.textContent).toContain('Review in draft');
  });

  it('hides Review in draft and publisher draft count when drafts gate is off', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: false,
    };
    mockDetail(
      artifactResp({
        canPublish: true,
        reviewState: {
          requestedOfMe: null,
          myDraft: { base: 2, savedAt: '2026-11-26T10:00:00Z' },
          draftsInProgress: 2,
        },
      })
    );

    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      user: { id: 'u-alice', email: 'alice@test', name: 'Alice', role: 'member' },
    } as never;
    document.body.appendChild(el);
    await settle(el);

    expect(el.shadowRoot!.querySelector('sl-badge.review-in-draft-badge')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-badge.drafts-in-progress-badge')).toBeNull();
  });

  it('shows publisher-side draftsInProgress count for publishers only', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    mockDetail(
      artifactResp({
        canPublish: true,
        canReview: false,
        reviewState: {
          requestedOfMe: null,
          draftsInProgress: 2,
        },
      })
    );

    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      user: { id: 'u-alice', email: 'alice@test', name: 'Alice', role: 'member' },
    } as never;
    document.body.appendChild(el);
    await settle(el);

    const pubBadge = el.shadowRoot!.querySelector('sl-badge.drafts-in-progress-badge');
    expect(pubBadge).not.toBeNull();
    expect(pubBadge!.getAttribute('variant')).toBe('neutral');
    expect(pubBadge!.textContent).toContain('2 reviews in draft');
    expect(el.shadowRoot!.querySelector('sl-badge.review-in-draft-badge')).toBeNull();
  });

  it('hides draftsInProgress count for non-publishers', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    mockDetail(
      artifactResp({
        canPublish: false,
        canReview: true,
        reviewState: {
          requestedOfMe: null,
          draftsInProgress: 2,
        },
      })
    );

    const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
    el.pageData = {
      user: { id: 'u-bob', email: 'bob@test', name: 'Bob', role: 'member' },
    } as never;
    document.body.appendChild(el);
    await settle(el);

    expect(el.shadowRoot!.querySelector('sl-badge.drafts-in-progress-badge')).toBeNull();
  });
});
