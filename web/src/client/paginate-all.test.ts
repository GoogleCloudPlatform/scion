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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { MockInstance } from 'vitest';
import { apiFetch } from './api.js';
import {
  paginateAll,
  PaginationError,
  PaginationStoppedError,
  PaginationTruncatedError,
} from './paginate-all.js';

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

afterEach(() => {
  vi.useRealTimers();
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

  it('carries the items accumulated up to the page safety bound on a PaginationTruncatedError', async () => {
    let n = 0;
    vi.mocked(apiFetch).mockImplementation(() => {
      n++;
      return Promise.resolve(jsonResponse({ items: [{ id: `x${n}` }], nextCursor: `c${n}` }));
    });

    const err = await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      maxPages: 2,
    }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(PaginationTruncatedError);
    expect(err).toBeInstanceOf(PaginationError);
    expect((err as PaginationTruncatedError<Item>).items).toEqual([{ id: 'x1' }, { id: 'x2' }]);
  });

  it('reports each page to onPage with that page and the running total, before the next request', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }, { id: 'c' }] }));
    const seen: Array<{ page: string[]; all: string[]; requestsSoFar: number }> = [];

    await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      onPage: (page, all) =>
        seen.push({
          page: page.map((i) => i.id),
          all: all.map((i) => i.id),
          requestsSoFar: vi.mocked(apiFetch).mock.calls.length,
        }),
    });

    expect(seen).toEqual([
      { page: ['a'], all: ['a'], requestsSoFar: 1 },
      { page: ['b', 'c'], all: ['a', 'b', 'c'], requestsSoFar: 2 },
    ]);
  });

  it('issues page requests through the fetch option instead of apiFetch when given', async () => {
    const fetchPage = vi.fn(() => Promise.resolve(jsonResponse({ items: [{ id: 'a' }] })));

    const items = await paginateAll({
      path: '/api/v1/things',
      pageSize: 10,
      parsePage,
      fetch: fetchPage,
    });

    expect(items).toEqual([{ id: 'a' }]);
    expect(fetchPage).toHaveBeenCalledWith('/api/v1/things?limit=10', expect.any(Object));
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

describe('paginateAll walk signal', () => {
  it('aborts the page in flight and rejects with an AbortError when the walk signal aborts', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockImplementationOnce((_url, init) => settleOnlyOnAbort<Response>(init?.signal));
    const walk = new AbortController();

    const settled = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      signal: walk.signal,
    }).catch((e: unknown) => e);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(2));
    const pageSignal = vi.mocked(apiFetch).mock.calls[1]?.[1]?.signal;
    walk.abort();

    const err = await settled;
    expect(err).toBeInstanceOf(DOMException);
    expect((err as DOMException).name).toBe('AbortError');
    expect(err).not.toBeInstanceOf(PaginationError);
    expect(pageSignal?.aborted).toBe(true);
  });

  it('requests no further page once the walk signal has aborted between pages', async () => {
    const walk = new AbortController();
    vi.mocked(apiFetch).mockImplementationOnce(() => {
      walk.abort();
      return Promise.resolve(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }));
    });

    const err = await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      signal: walk.signal,
    }).catch((e: unknown) => e);

    expect((err as DOMException).name).toBe('AbortError');
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it('rejects without any request when the walk signal is already aborted', async () => {
    const walk = new AbortController();
    walk.abort();

    const err = await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      signal: walk.signal,
    }).catch((e: unknown) => e);

    expect((err as DOMException).name).toBe('AbortError');
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

/**
 * A promise that, like a fetch request or body read, never settles on its
 * own and rejects with an AbortError once `signal` is aborted.
 */
function settleOnlyOnAbort<T>(signal: AbortSignal | null | undefined): Promise<T> {
  return new Promise<T>((_, reject) => {
    signal?.addEventListener('abort', () => {
      reject(new DOMException('The operation was aborted.', 'AbortError'));
    });
  });
}

describe('paginateAll page timeout', () => {
  it('rejects with PaginationError after pageTimeoutMs when a page request never settles, aborting its signal', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockImplementationOnce((_url, init) => settleOnlyOnAbort<Response>(init?.signal));

    const settled = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      label: 'things list',
      pageTimeoutMs: 5000,
    }).then(
      () => 'resolved',
      (err: unknown) => err
    );

    await vi.advanceTimersByTimeAsync(4999);
    expect(apiFetch).toHaveBeenCalledTimes(2);
    const signal = vi.mocked(apiFetch).mock.calls[1]?.[1]?.signal;
    expect(signal).toBeInstanceOf(AbortSignal);
    expect(signal?.aborted).toBe(false);

    await vi.advanceTimersByTimeAsync(1);
    const err = await settled;
    expect(err).toBeInstanceOf(PaginationError);
    expect(err).not.toBeInstanceOf(PaginationStoppedError);
    expect((err as Error).message).toBe('things list page request timed out after 5000ms');
    expect(signal?.aborted).toBe(true);
    expect(apiFetch).toHaveBeenCalledTimes(2);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('rejects with PaginationError after pageTimeoutMs when a page body read never settles', async () => {
    vi.useFakeTimers();
    const stalledBody = new Response('{}', { status: 200 });
    let stalledJson: MockInstance | undefined;
    vi.mocked(apiFetch).mockImplementationOnce((_url, init) => {
      stalledJson = vi.spyOn(stalledBody, 'json').mockReturnValue(settleOnlyOnAbort(init?.signal));
      return Promise.resolve(stalledBody);
    });

    const settled = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      label: 'things list',
      pageTimeoutMs: 5000,
    }).then(
      () => 'resolved',
      (err: unknown) => err
    );

    await vi.advanceTimersByTimeAsync(4999);
    expect(stalledJson).toHaveBeenCalledTimes(1);
    expect(vi.mocked(apiFetch).mock.calls[0]?.[1]?.signal?.aborted).toBe(false);

    await vi.advanceTimersByTimeAsync(1);
    const err = await settled;
    expect(err).toBeInstanceOf(PaginationError);
    expect((err as Error).message).toBe('things list page request timed out after 5000ms');
    expect(vi.mocked(apiFetch).mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('defaults the page timeout to 60000ms', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch).mockImplementationOnce((_url, init) =>
      settleOnlyOnAbort<Response>(init?.signal)
    );

    const settled = paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage }).then(
      () => 'resolved',
      (err: unknown) => err
    );

    await vi.advanceTimersByTimeAsync(59_999);
    expect(vi.mocked(apiFetch).mock.calls[0]?.[1]?.signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    const err = await settled;
    expect(err).toBeInstanceOf(PaginationError);
    expect((err as Error).message).toBe('/api/v1/things page request timed out after 60000ms');
    expect(vi.mocked(apiFetch).mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('rejects a walk stopped by shouldContinue before page 2 and leaves no pending timer', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }] }));

    let pagesAllowed = 1;
    const err = await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      pageTimeoutMs: 5000,
      shouldContinue: () => pagesAllowed-- > 0,
    }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(PaginationStoppedError);
    expect((err as PaginationStoppedError<Item>).items).toEqual([{ id: 'a' }]);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('leaves no pending timer after a page body that is not an object', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(['not', 'an', 'object']));

    await expect(
      paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage, label: 'things list' })
    ).rejects.toThrow('things list response body was not an object');
    expect(vi.getTimerCount()).toBe(0);
  });

  it('completes a normal multi-page walk and leaves no pending timer', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'a' }], nextCursor: 'c1' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'b' }], nextCursor: 'c2' }))
      .mockResolvedValueOnce(jsonResponse({ items: [{ id: 'c' }] }));

    const items = await paginateAll({
      path: '/api/v1/things',
      pageSize: 2,
      parsePage,
      pageTimeoutMs: 5000,
    });

    expect(items).toEqual([{ id: 'a' }, { id: 'b' }, { id: 'c' }]);
    expect(apiFetch).toHaveBeenCalledTimes(3);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('keeps non-timeout failures unchanged and leaves no pending timer', async () => {
    vi.useFakeTimers();
    vi.mocked(apiFetch).mockResolvedValueOnce(new Response('', { status: 500 }));
    await expect(
      paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage, label: 'things list' })
    ).rejects.toThrow('things list request failed: 500');
    expect(vi.getTimerCount()).toBe(0);

    vi.mocked(apiFetch).mockResolvedValueOnce(new Response('not json', { status: 200 }));
    await expect(
      paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage, label: 'things list' })
    ).rejects.toThrow('things list response was not valid JSON');
    expect(vi.getTimerCount()).toBe(0);

    const networkError = new TypeError('Failed to fetch');
    vi.mocked(apiFetch).mockRejectedValueOnce(networkError);
    await expect(
      paginateAll({ path: '/api/v1/things', pageSize: 100, parsePage, label: 'things list' })
    ).rejects.toBe(networkError);
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('paginateAll failed-response body (ptone/scion#2949)', () => {
  async function failWith(res: Response): Promise<PaginationError> {
    vi.mocked(apiFetch).mockResolvedValueOnce(res);
    const err = await paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      label: 'Things',
    }).then(
      () => undefined,
      (e: unknown) => e
    );
    expect(err).toBeInstanceOf(PaginationError);
    return err as PaginationError;
  }

  it('carries the status, the parsed body and the hub message ({error: {message}})', async () => {
    const body = { error: { code: 'forbidden', message: 'You cannot list skills here' } };
    const err = await failWith(jsonResponse(body, 403));
    expect(err.message).toBe('Things request failed: 403');
    expect(err.status).toBe(403);
    expect(err.body).toEqual(body);
    expect(err.hubMessage).toBe('You cannot list skills here');
  });

  it('reads {message} and {error: "..."} bodies', async () => {
    expect((await failWith(jsonResponse({ message: 'quota exceeded' }, 429))).hubMessage).toBe(
      'quota exceeded'
    );
    expect((await failWith(jsonResponse({ error: 'bad cursor' }, 400))).hubMessage).toBe(
      'bad cursor'
    );
  });

  it('keeps a non-JSON body as capped text but takes no hub message from it', async () => {
    const err = await failWith(new Response('upstream unavailable', { status: 502 }));
    expect(err.status).toBe(502);
    expect(err.body).toBe('upstream unavailable');
    expect(err.hubMessage).toBeUndefined();
  });

  it('does not show a proxy HTML error page, and caps the text it keeps', async () => {
    const html = `<html><body><h1>502 Bad Gateway</h1>${'x'.repeat(2000)}</body></html>`;
    const err = await failWith(
      new Response(html, { status: 502, headers: { 'Content-Type': 'text/html' } })
    );
    expect(err.hubMessage).toBeUndefined();
    expect(typeof err.body).toBe('string');
    expect((err.body as string).length).toBe(501);
    expect((err.body as string).endsWith('…')).toBe(true);
  });

  it('caps a long hub message and appends error.details.guidance', async () => {
    const long = await failWith(jsonResponse({ error: { message: 'm'.repeat(800) } }, 400));
    expect(long.hubMessage).toBe(`${'m'.repeat(500)}…`);

    const guided = await failWith(
      jsonResponse(
        { error: { message: 'clone failed', details: { guidance: 'check the URL' } } },
        400
      )
    );
    expect(guided.hubMessage).toBe('clone failed — check the URL');
  });

  it('has no body or hub message for an empty body or a body without a message', async () => {
    const empty = await failWith(new Response('', { status: 500 }));
    expect(empty.status).toBe(500);
    expect(empty.body).toBeUndefined();
    expect(empty.hubMessage).toBeUndefined();

    const noMessage = await failWith(jsonResponse({ error: { code: 'internal' } }, 500));
    expect(noMessage.body).toEqual({ error: { code: 'internal' } });
    expect(noMessage.hubMessage).toBeUndefined();
  });

  it('reports the status alone when the error body cannot be read', async () => {
    const res = new Response('ignored', { status: 503 });
    vi.spyOn(res, 'text').mockRejectedValue(new TypeError('body stream failed'));
    const err = await failWith(res);
    expect(err.message).toBe('Things request failed: 503');
    expect(err.status).toBe(503);
    expect(err.body).toBeUndefined();
    expect(err.hubMessage).toBeUndefined();
  });

  it('bounds the error body read by the page timeout and leaves no pending timer', async () => {
    vi.useFakeTimers();
    const res = new Response('ignored', { status: 500 });
    vi.mocked(apiFetch).mockImplementationOnce((_url, init) => {
      vi.spyOn(res, 'text').mockReturnValue(settleOnlyOnAbort(init?.signal));
      return Promise.resolve(res);
    });
    const settled = paginateAll({
      path: '/api/v1/things',
      pageSize: 100,
      parsePage,
      label: 'Things',
      pageTimeoutMs: 5000,
    }).then(
      () => undefined,
      (e: unknown) => e
    );
    await vi.advanceTimersByTimeAsync(5000);
    const err = (await settled) as PaginationError;
    expect(err).toBeInstanceOf(PaginationError);
    expect(err.status).toBe(500);
    expect(err.body).toBeUndefined();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('keeps PaginationError constructible with a message only', () => {
    const err = new PaginationError('x');
    expect(err.status).toBeUndefined();
    expect(err.body).toBeUndefined();
    expect(err.hubMessage).toBeUndefined();
  });
});
