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
 * Tests for attachment provenance visibility on <scion-page-artifact-detail>
 * (ptone/scion#4598, #4599): "Published from an attachment in a conversation"
 * appears when sourceAttachmentId is set, with the conversation link shown
 * only when sourceMessageId is present (already viewer-scoped by backend).
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { ARTIFACTS_FLAG, type ArtifactResponse } from '../../client/artifacts.js';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../utils/feature-flags.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const ARTIFACT_ID = '22222222-2222-2222-2222-222222222222';
const ATT_ID = '33333333-3333-3333-3333-333333333333';

function makeResponse(provenance: {
  sourceAttachmentId?: string;
  sourceMessageId?: string;
  sourceConversationKey?: string;
  sourceProjectId?: string;
}): ArtifactResponse {
  return {
    artifact: {
      id: ARTIFACT_ID,
      ref: `scion://artifact/${ARTIFACT_ID}`,
      scopeKind: 'project',
      scopeRef: PROJECT_ID,
      ownerKind: 'user',
      ownerRef: 'u1',
      title: 'Notes',
      currentSeq: 1,
      createdAt: '2026-11-20T00:00:00Z',
      updatedAt: '2026-11-20T00:00:00Z',
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ARTIFACT_ID}@1`,
      kind: 'publish',
      entryPath: 'notes.txt',
      totalBytes: 5,
      fileCount: 1,
      createdByKind: 'user',
      createdByRef: 'u1',
      createdAt: '2026-11-20T00:00:00Z',
      state: 'ready',
      files: [{ path: 'notes.txt', size: 5, sha256: 'abc', mediaType: 'text/plain' }],
      ...(provenance.sourceAttachmentId
        ? { sourceAttachmentId: provenance.sourceAttachmentId }
        : {}),
      ...(provenance.sourceMessageId ? { sourceMessageId: provenance.sourceMessageId } : {}),
      ...(provenance.sourceConversationKey
        ? { sourceConversationKey: provenance.sourceConversationKey }
        : {}),
      ...(provenance.sourceProjectId ? { sourceProjectId: provenance.sourceProjectId } : {}),
    },
  };
}

function stubArtifactApi(resp: ArtifactResponse): void {
  const json = (body: unknown) =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = requestUrl(input);
      if (url === `/api/v1/artifacts/${ARTIFACT_ID}`) return Promise.resolve(json(resp));
      if (url.startsWith(`/api/v1/artifacts/${ARTIFACT_ID}/versions`)) {
        return Promise.resolve(json({ versions: resp.version ? [resp.version] : [] }));
      }
      if (url.includes('/files/')) {
        return Promise.resolve(new Response('hello', { status: 200 }));
      }
      return Promise.resolve(new Response('{}', { status: 404 }));
    })
  );
}

async function mountDetail(): Promise<ScionPageArtifactDetail> {
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.projectId = PROJECT_ID;
  el.artifactId = ARTIFACT_ID;
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(el.shadowRoot?.querySelector('scion-detail-header')).not.toBeNull();
  });
  return el;
}

describe('scion-page-artifact-detail attachment provenance (#4598, #4599)', () => {
  beforeAll(async () => {
    await import('./artifact-detail.js');
  });

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    setFeatureFlag(ARTIFACTS_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('omits the provenance line for ordinary publishes without sourceAttachmentId', async () => {
    stubArtifactApi(makeResponse({}));
    const el = await mountDetail();
    expect(el.shadowRoot?.querySelector('.attachment-provenance')).toBeNull();
  });

  it('shows the provenance line without a link when sourceAttachmentId is set but sourceMessageId is absent', async () => {
    stubArtifactApi(makeResponse({ sourceAttachmentId: ATT_ID }));
    const el = await mountDetail();
    const prov = el.shadowRoot?.querySelector('.attachment-provenance');
    expect(prov).not.toBeNull();
    expect(prov?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Published from an attachment in a conversation'
    );
    expect(prov?.querySelector('a')).toBeNull();
  });

  it('shows the provenance line without a link when sourceMessageId has no routable conversation', async () => {
    stubArtifactApi(
      makeResponse({
        sourceAttachmentId: ATT_ID,
        sourceMessageId: 'msg-42',
        sourceConversationKey: 'group-no-project',
      })
    );
    const el = await mountDetail();
    const prov = el.shadowRoot?.querySelector('.attachment-provenance');
    expect(prov).not.toBeNull();
    expect(prov?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Published from an attachment in a conversation'
    );
    expect(prov?.querySelector('a')).toBeNull();
  });

  it('links to the project thread conversation and message when source route is a project thread', async () => {
    stubArtifactApi(
      makeResponse({
        sourceAttachmentId: ATT_ID,
        sourceMessageId: 'msg-42',
        sourceConversationKey: 'topic-7',
        sourceProjectId: PROJECT_ID,
      })
    );
    const el = await mountDetail();
    const prov = el.shadowRoot?.querySelector('.attachment-provenance');
    expect(prov).not.toBeNull();
    expect(prov?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Published from an attachment in a conversation'
    );
    const link = prov?.querySelector<HTMLAnchorElement>('a.provenance-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('href')).toBe(`/chat/space/${PROJECT_ID}/thread/topic-7#msg-msg-42`);
  });

  it('links to the DM conversation and message when source route is a DM', async () => {
    stubArtifactApi(
      makeResponse({
        sourceAttachmentId: ATT_ID,
        sourceMessageId: 'msg-99',
        sourceConversationKey: 'dm:user:u1:user:u2',
      })
    );
    const el = await mountDetail();
    const prov = el.shadowRoot?.querySelector('.attachment-provenance');
    expect(prov).not.toBeNull();
    const link = prov?.querySelector<HTMLAnchorElement>('a.provenance-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('href')).toBe('/chat/dm/dm%3Auser%3Au1%3Auser%3Au2#msg-msg-99');
  });
});
