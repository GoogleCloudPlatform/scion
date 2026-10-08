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
 * apiFetchAllPages never hands a caller a partial list: a failed or
 * malformed later page rejects, and a walk cut off by the page cap
 * rejects (ptone/scion#3465).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { apiFetchAllPages } from './api.js';

type PageReply = { items: string[]; nextCursor?: string } | 'fail' | 'bad-json';

/** Answers page N (0-based, by cursor) with `pages[N]`. */
function stubPages(pages: (i: number) => PageReply): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      urls.push(url);
      const cursor = new URL(url, 'http://x').searchParams.get('cursor');
      const reply = pages(cursor ? Number(cursor) : 0);
      if (reply === 'fail') {
        return Promise.resolve(new Response('{}', { status: 500, statusText: 'Server Error' }));
      }
      if (reply === 'bad-json') {
        return Promise.resolve(new Response('not json', { status: 200 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ things: reply.items, nextCursor: reply.nextCursor }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
  return urls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('apiFetchAllPages', () => {
  it('concatenates every page until the cursor runs out', async () => {
    const urls = stubPages((i) =>
      i < 2 ? { items: [`a${i}`], nextCursor: String(i + 1) } : { items: ['a2'] }
    );
    await expect(apiFetchAllPages<string>('/api/v1/things?limit=1', 'things')).resolves.toEqual([
      'a0',
      'a1',
      'a2',
    ]);
    expect(urls).toHaveLength(3);
  });

  it('rejects when a later page fails instead of returning the earlier pages', async () => {
    stubPages((i) => (i === 0 ? { items: ['a0'], nextCursor: '1' } : 'fail'));
    await expect(apiFetchAllPages<string>('/api/v1/things', 'things')).rejects.toThrow(
      /page 2: 500/
    );
  });

  it('rejects when a later page body cannot be parsed', async () => {
    stubPages((i) => (i === 0 ? { items: ['a0'], nextCursor: '1' } : 'bad-json'));
    await expect(apiFetchAllPages<string>('/api/v1/things', 'things')).rejects.toThrow(
      /parse page 2/
    );
  });

  it('rejects when the page cap is reached with pages remaining', async () => {
    const urls = stubPages((i) => ({ items: [`a${i}`], nextCursor: String(i + 1) }));
    await expect(apiFetchAllPages<string>('/api/v1/things', 'things')).rejects.toThrow(
      /Stopped after 50 pages/
    );
    expect(urls).toHaveLength(50);
  });

  it('still rejects when the first page fails', async () => {
    stubPages(() => 'fail');
    await expect(apiFetchAllPages<string>('/api/v1/things', 'things')).rejects.toThrow(
      /Failed to fetch: 500/
    );
  });
});
