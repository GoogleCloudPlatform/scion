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

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { apiFetch } from './api.js';
import { paginateAll, PaginationError, PaginationStoppedError } from './paginate-all.js';

vi.mock('./api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

interface Item {
  id: string;
}

function parsePage(body: unknown): { items: Item[]; nextCursor?: string } {
  const data = body as { items?: Item[]; nextCursor?: string };
  return { items: data.items ?? [], ...(data.nextCursor ? { nextCursor: data.nextCursor } : {}) };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
});

describe('paginateAll', () => {
  it('returns every item from a single page with no nextCursor', async () => {
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }, { id: 'b' }] }));

    const items = await paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage });

    expect(items).toEqual([{ id: 'a' }, { id: 'b' }]);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(vi.mocked(apiFetch).mock.calls[0]?.[0]).toBe('/api/v1/things?limit=100');
  });

  it('walks multiple pages until nextCursor is empty, carrying the cursor on each request', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }], nextCursor: 'c2' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'c' }] }));

    const items = await paginateAll({ path: '/api/v1/things', pageSize: 2, parsePage });

    expect(items).toEqual([{ id: 'a' }, { id: 'b' }, { id: 'c' }]);
    expect(apiFetch).toHaveBeenCalledTimes(3);
    const urls = vi.mocked(apiFetch).mock.calls.map((c) => c[0]);
    expect(urls).toEqual([
      '/api/v1/things?limit=2',
      '/api/v1/things?limit=2&cursor=c1',
      '/api/v1/things?limit=2&cursor=c2',
    ]);
  });

  it('keeps walking through a page with zero items but a nonempty cursor, and returns empty for a wholly empty list', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }] }));

    const items = await paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage });

    expect(items).toEqual([{ id: 'a' }]);
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

  it('returns an empty array for a single empty page', async () => {
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse({ items: [] }));

    const items = await paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage });

    expect(items).toEqual([]);
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it('throws PaginationError and stops walking when a page mid-walk fails', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(new Response('', { status: 500 }));

    await expect(paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage })).rejects.toThrow(
      PaginationError
    );
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

  it('throws PaginationError on a non-object response body', async () => {
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(['not', 'an', 'object']));

    await expect(paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage })).rejects.toThrow(
      PaginationError
    );
  });

  it('throws PaginationError on a repeated cursor rather than looping forever', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }], nextCursor: 'c1' }));

    await expect(paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage })).rejects.toThrow(
      'repeated pagination cursor'
    );
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

  it('stops before fetching another page once shouldContinue returns false, rejecting with the partial list attached', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }], nextCursor: 'c2' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'c' }] }));

    let pagesAllowed = 1;
    const promise = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      shouldContinue: () => pagesAllowed-- > 0,
    });

    await expect(promise).rejects.toBeInstanceOf(PaginationStoppedError);
    await promise.catch((err: PaginationStoppedError<Item>) => {
      expect(err.items).toEqual([{ id: 'a' }]);
    });
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it('rejects with an empty partial list when shouldContinue is already false before the first page', async () => {
    const promise = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      shouldContinue: () => false,
    });

    await expect(promise).rejects.toBeInstanceOf(PaginationStoppedError);
    await promise.catch((err: PaginationStoppedError<Item>) => {
      expect(err.items).toEqual([]);
    });
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('throws PaginationError once the page safety bound is reached', async () => {
    let n = 0;
    vi.mocked(apiFetch).mockImplementation(() => {
      n++;
      return Promise.resolve(jsonResponse({ items: [{ id: `x${n}` }], nextCursor: `c${n}` }));
    });

    await expect(
      paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage, maxPages: 3 })
    ).rejects.toThrow('page safety bound');
    expect(apiFetch).toHaveBeenCalledTimes(3);
  });
});
