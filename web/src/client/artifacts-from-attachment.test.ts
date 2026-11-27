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

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ATTACHMENT_ARTIFACTS_FLAG,
  formatAttachmentRef,
  publishFromAttachment,
  ReviewOpenError,
} from './artifacts.js';
import { requestUrl } from './__fixtures__/request-url.js';

const ATT_ID = '5f1c2d3e-0000-4000-8000-000000000001';
const ART_ID = '5f1c2d3e-0000-4000-8000-000000000002';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('attachment artifact client helpers (#4599)', () => {
  it('exports ATTACHMENT_ARTIFACTS_FLAG and formats attachment refs', () => {
    expect(ATTACHMENT_ARTIFACTS_FLAG).toBe('hub.attachment_artifacts');
    expect(formatAttachmentRef(ATT_ID)).toBe(`scion://attachment/${ATT_ID}`);
  });

  it('publishes a new artifact from an attachment by reference', async () => {
    const sent: Array<{ url: string; method: string; body: Record<string, unknown> }> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        sent.push({
          url: requestUrl(input),
          method: init?.method ?? 'GET',
          body: JSON.parse(typeof init?.body === 'string' ? init.body : '{}') as Record<
            string,
            unknown
          >,
        });
        return Promise.resolve(
          new Response(
            JSON.stringify({
              artifact: {
                id: ART_ID,
                ref: `scion://artifact/${ART_ID}`,
                scopeKind: 'project',
                scopeRef: 'proj-1',
                ownerKind: 'user',
                ownerRef: 'u1',
                title: 'Report',
                currentSeq: 1,
                createdAt: '2026-11-20T00:00:00Z',
                updatedAt: '2026-11-20T00:00:00Z',
              },
              version: {
                seq: 1,
                ref: `scion://artifact/${ART_ID}@1`,
                kind: 'publish',
                entryPath: 'report.md',
                totalBytes: 42,
                fileCount: 1,
                createdAt: '2026-11-20T00:00:00Z',
                state: 'ready',
                files: [],
                sourceAttachmentId: ATT_ID,
                sourceMessageId: 'msg-99',
              },
            }),
            { status: 201, headers: { 'Content-Type': 'application/json' } }
          )
        );
      })
    );

    const res = await publishFromAttachment({
      fromAttachment: ATT_ID,
      scopeRef: 'proj-1',
      title: '  Report  ',
      key: 'weekly-report',
      note: 'initial cut',
    });

    expect(sent).toEqual([
      {
        url: '/api/v1/artifacts',
        method: 'POST',
        body: {
          fromAttachment: ATT_ID,
          scopeRef: 'proj-1',
          title: 'Report',
          key: 'weekly-report',
          note: 'initial cut',
        },
      },
    ]);
    expect(res.artifact.id).toBe(ART_ID);
    expect(res.version?.sourceAttachmentId).toBe(ATT_ID);
    expect(res.version?.sourceMessageId).toBe('msg-99');
  });

  it('omits empty optional fields and surfaces ReviewOpenError on 409 review_open', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              error: {
                code: 'review_open',
                message: 'An open review exists.',
                details: {
                  base: 1,
                  truncated: false,
                  reviews: [
                    {
                      reviewer: 'Alice',
                      lastActivityAt: '2026-11-20T00:00:00Z',
                      unsavedChanges: true,
                    },
                  ],
                },
              },
            }),
            { status: 409, headers: { 'Content-Type': 'application/json' } }
          )
        )
      )
    );

    await expect(
      publishFromAttachment({
        fromAttachment: ATT_ID,
        scopeRef: 'proj-1',
        key: 'weekly-report',
      })
    ).rejects.toBeInstanceOf(ReviewOpenError);
  });
});
