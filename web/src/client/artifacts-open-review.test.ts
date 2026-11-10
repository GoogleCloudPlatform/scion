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
 * The client side of the open-review check (ptone/scion#6849): a publish
 * refused with 409 review_open, force, create-time warnings, and the
 * review-session calls.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  endReviewSession,
  newReviewSessionId,
  publishFiles,
  putReviewSession,
  ReviewOpenError,
} from './artifacts.js';
import { requestUrl } from './__fixtures__/request-url.js';

interface Call {
  url: string;
  method: string;
  body: BodyInit | null | undefined;
  keepalive: boolean | undefined;
}

function recordFetch(handler: (c: Call) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const c: Call = {
        url: requestUrl(input),
        method: init?.method ?? 'GET',
        body: init?.body,
        keepalive: init?.keepalive,
      };
      calls.push(c);
      return Promise.resolve(handler(c));
    })
  );
  return calls;
}

const json = (v: unknown, status = 200): Response =>
  new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } });

const refusal = {
  error: {
    code: 'review_open',
    message: '1 review of this artifact is open; publish anyway with force, or wait for it',
    details: {
      reviews: [
        { reviewer: 'Una User', lastActivityAt: '2026-11-04T10:01:00Z', unsavedChanges: true },
      ],
      truncated: false,
      base: 7,
    },
  },
};

const files = (): { path: string; data: Blob }[] => [{ path: 'doc.md', data: new Blob(['# v8']) }];

describe('publishFiles and open reviews', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('throws ReviewOpenError on a 409 review_open and sends nothing more', async () => {
    const calls = recordFetch(() => json(refusal, 409));
    const err = await publishFiles({ artifactId: 'a-1', entry: 'doc.md', files: files() }).catch(
      (e: unknown) => e
    );
    expect(err).toBeInstanceOf(ReviewOpenError);
    const r = err as ReviewOpenError;
    expect(r.code).toBe('review_open');
    expect(r.pending).toBeNull();
    expect(r.reviews).toEqual(refusal.error.details.reviews);
    expect(r.truncated).toBe(false);
    expect(r.base).toBe(7);
    expect(calls).toHaveLength(1);
    expect(JSON.parse(calls[0].body as string)).not.toHaveProperty('force');
  });

  it('names a reviewer without a name "a reviewer"', async () => {
    recordFetch(() =>
      json(
        {
          error: {
            code: 'review_open',
            details: { reviews: [{ lastActivityAt: 'x' }, 'junk'], truncated: true },
          },
        },
        409
      )
    );
    const err = (await publishFiles({ artifactId: 'a-1', entry: 'doc.md', files: files() }).catch(
      (e: unknown) => e
    )) as ReviewOpenError;
    expect(err.reviews).toEqual([
      { reviewer: 'a reviewer', lastActivityAt: 'x', unsavedChanges: false },
    ]);
    expect(err.truncated).toBe(true);
  });

  it('keeps another 409 a plain error', async () => {
    recordFetch(() => json({ error: { code: 'too_many_pending', message: 'busy' } }, 409));
    const err = await publishFiles({ artifactId: 'a-1', entry: 'doc.md', files: files() }).catch(
      (e: unknown) => e
    );
    expect(err).not.toBeInstanceOf(ReviewOpenError);
    expect((err as Error).message).toBe('busy');
  });

  it('sends force and returns the create warnings before the finalize ones', async () => {
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && c.url === '/api/v1/artifacts/a-1/versions') {
        return json(
          {
            artifact: { id: 'a-1' },
            version: { seq: 8 },
            upload: { required: ['doc.md'] },
            warnings: ['published while 1 review was open (Una User)'],
          },
          201
        );
      }
      if (c.method === 'PUT') return new Response(null, { status: 204 });
      return json({ artifact: { id: 'a-1' }, version: { seq: 8 }, warnings: ['later'] });
    });
    const res = await publishFiles({
      artifactId: 'a-1',
      entry: 'doc.md',
      files: files(),
      force: true,
    });
    expect(JSON.parse(calls[0].body as string).force).toBe(true);
    expect(res.warnings).toEqual(['published while 1 review was open (Una User)', 'later']);
  });

  it('resumes a pending version whether or not force is set', async () => {
    let failUpload = true;
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && c.url === '/api/v1/artifacts/a-1/versions') {
        return json(
          { artifact: { id: 'a-1' }, version: { seq: 8 }, upload: { required: ['doc.md'] } },
          201
        );
      }
      if (c.method === 'PUT') {
        return failUpload
          ? json({ error: { code: 'internal', message: 'boom' } }, 500)
          : new Response(null, { status: 204 });
      }
      return json({ artifact: { id: 'a-1' }, version: { seq: 8 } });
    });
    const first = (await publishFiles({ artifactId: 'a-1', entry: 'doc.md', files: files() }).catch(
      (e: unknown) => e
    )) as { pending: unknown };
    expect(first.pending).not.toBeNull();
    failUpload = false;
    calls.length = 0;
    await publishFiles({
      artifactId: 'a-1',
      entry: 'doc.md',
      files: files(),
      force: true,
      resume: first.pending as never,
    });
    // force is not part of what is published: the pending version is
    // resumed, with no second create.
    expect(calls.map((c) => c.method)).toEqual(['PUT', 'POST']);
    expect(calls[1].url).toBe('/api/v1/artifacts/a-1/versions/8/finalize');
  });
});

describe('review sessions', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('makes 128-bit base64url session ids', () => {
    const a = newReviewSessionId();
    expect(a).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(newReviewSessionId()).not.toBe(a);
  });

  it('PUTs a beat and returns the current version', async () => {
    const calls = recordFetch(() => json({ currentSeq: 9 }));
    const res = await putReviewSession('a-1', 'S'.repeat(22), {
      base: 7,
      edited: true,
      dirty: false,
    });
    expect(res.currentSeq).toBe(9);
    expect(calls[0].method).toBe('PUT');
    expect(calls[0].url).toBe(`/api/v1/artifacts/a-1/review-sessions/${'S'.repeat(22)}`);
    expect(JSON.parse(calls[0].body as string)).toEqual({ base: 7, edited: true, dirty: false });
  });

  it('DELETEs the session, with keepalive on request, and ignores failures', async () => {
    const calls = recordFetch(() => new Response(null, { status: 204 }));
    await endReviewSession('a-1', 'S'.repeat(22), { keepalive: true });
    expect(calls[0].method).toBe('DELETE');
    expect(calls[0].keepalive).toBe(true);
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('offline')))
    );
    await expect(endReviewSession('a-1', 'S'.repeat(22))).resolves.toBeUndefined();
  });
});
