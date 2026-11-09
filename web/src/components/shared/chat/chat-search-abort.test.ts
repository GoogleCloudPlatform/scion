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
 * Chat search is bounded (ptone/scion#7586, design v3 section 14 step 6):
 * every request has its own AbortController, new input and closing the
 * panel abort the running request, a 503 or 429 shows its message instead
 * of failing silently, and an aborted request is not an error.
 */

import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';

vi.mock('../../../client/main.js', () => import('../../../client/__fixtures__/main-stub.js'));

const apiFetch = vi.fn<(path: string, init?: { signal?: AbortSignal }) => Promise<Response>>();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, init?: { signal?: AbortSignal }) => apiFetch(path, init),
  extractApiError: vi.fn(() => 'error'),
}));

/* eslint-disable @typescript-eslint/no-explicit-any */

let mod: typeof import('./chat-search.js');

beforeAll(async () => {
  mod = await import('./chat-search.js');
});

beforeEach(() => {
  apiFetch.mockReset();
});

/** A fetch that never settles until its signal aborts, then rejects like fetch. */
function pendingUntilAborted(_path: string, init?: { signal?: AbortSignal }): Promise<Response> {
  return new Promise((_resolve, reject) => {
    init?.signal?.addEventListener('abort', () =>
      reject(new DOMException('The operation was aborted.', 'AbortError'))
    );
  });
}

function makeSearch(query = 'needle'): any {
  const el = document.createElement('scion-chat-search') as any;
  el.query = query;
  return el;
}

describe('searchErrorMessage', () => {
  it('maps 503 to the timeout message and 429 to the busy message', () => {
    expect(mod.searchErrorMessage(503)).toBe(mod.SEARCH_TIMEOUT_MESSAGE);
    expect(mod.searchErrorMessage(429)).toBe(mod.SEARCH_BUSY_MESSAGE);
    expect(mod.searchErrorMessage(500)).toBe(mod.SEARCH_FAILED_MESSAGE);
  });

  it('maps the body error code first and falls back to the status', () => {
    expect(mod.searchErrorMessage(503, 'SEARCH_TIMEOUT')).toBe(mod.SEARCH_TIMEOUT_MESSAGE);
    expect(mod.searchErrorMessage(429, 'SEARCH_BUSY')).toBe(mod.SEARCH_BUSY_MESSAGE);
    // A maintenance or proxy 503 with another code is a plain failure.
    expect(mod.searchErrorMessage(503, 'MAINTENANCE')).toBe(mod.SEARCH_FAILED_MESSAGE);
    expect(mod.searchErrorMessage(400, 'VALIDATION_ERROR')).toBe(mod.SEARCH_FAILED_MESSAGE);
  });

  it('uses the exact timeout text from the design', () => {
    expect(mod.SEARCH_TIMEOUT_MESSAGE).toBe(
      'Search took too long. Try a longer phrase or search one space.'
    );
  });
});

describe('scion-chat-search request lifecycle', () => {
  it('passes an AbortSignal and aborts the previous request on a new search', async () => {
    apiFetch.mockImplementation(pendingUntilAborted);
    const el = makeSearch();
    const first = el.performSearch();
    const firstSignal = apiFetch.mock.calls[0][1]?.signal as AbortSignal;
    expect(firstSignal).toBeInstanceOf(AbortSignal);
    expect(firstSignal.aborted).toBe(false);

    const second = el.performSearch();
    expect(firstSignal.aborted).toBe(true);
    const secondSignal = apiFetch.mock.calls[1][1]?.signal as AbortSignal;
    expect(secondSignal).not.toBe(firstSignal);
    expect(secondSignal.aborted).toBe(false);

    await first;
    // The superseded request's abort is not an error, and the newer
    // request still owns the loading state.
    expect(el.errorMsg).toBe('');
    expect(el.loading).toBe(true);

    el.close();
    await second;
    expect(secondSignal.aborted).toBe(true);
  });

  it('aborts the running request when the user types again', async () => {
    apiFetch.mockImplementation(pendingUntilAborted);
    const el = makeSearch();
    const run = el.performSearch();
    const signal = apiFetch.mock.calls[0][1]?.signal as AbortSignal;

    el.handleInput({ target: { value: 'needles' } } as unknown as Event);
    expect(signal.aborted).toBe(true);
    await run;
    expect(el.errorMsg).toBe('');
    el.disconnectedCallback();
  });

  it('aborts the running request when the panel closes', async () => {
    apiFetch.mockImplementation(pendingUntilAborted);
    const el = makeSearch();
    const run = el.performSearch();
    const signal = apiFetch.mock.calls[0][1]?.signal as AbortSignal;

    el.close();
    expect(signal.aborted).toBe(true);
    await run;
    expect(el.errorMsg).toBe('');
    expect(el.loading).toBe(false);
  });

  it('aborts the running request when the element is disconnected', async () => {
    apiFetch.mockImplementation(pendingUntilAborted);
    const el = makeSearch();
    const run = el.performSearch();
    const signal = apiFetch.mock.calls[0][1]?.signal as AbortSignal;
    el.disconnectedCallback();
    expect(signal.aborted).toBe(true);
    await run;
    expect(el.errorMsg).toBe('');
  });

  it('shows a plain failure for a 503 whose body code is not SEARCH_TIMEOUT', async () => {
    apiFetch.mockResolvedValue(
      new Response(JSON.stringify({ error: { code: 'MAINTENANCE', message: 'down' } }), {
        status: 503,
      })
    );
    const el = makeSearch();
    await el.performSearch();
    expect(el.errorMsg).toBe(mod.SEARCH_FAILED_MESSAGE);
  });

  it('limits the input to the hub maximum of 256 characters', async () => {
    const el = makeSearch('') as any;
    document.body.appendChild(el);
    el.searchActive = true;
    await el.updateComplete;
    const input = el.shadowRoot?.querySelector('.search-input') as HTMLInputElement | null;
    expect(input?.getAttribute('maxlength')).toBe(String(mod.SEARCH_MAX_QUERY_CHARS));
    expect(mod.SEARCH_MAX_QUERY_CHARS).toBe(256);
    el.remove();
  });

  it('shows the timeout message on 503', async () => {
    apiFetch.mockResolvedValue(new Response('{}', { status: 503 }));
    const el = makeSearch();
    await el.performSearch();
    expect(el.errorMsg).toBe(mod.SEARCH_TIMEOUT_MESSAGE);
    expect(el.results).toEqual([]);
    expect(el.loading).toBe(false);
  });

  it('shows the busy message on 429', async () => {
    apiFetch.mockResolvedValue(new Response('{}', { status: 429 }));
    const el = makeSearch();
    await el.performSearch();
    expect(el.errorMsg).toBe(mod.SEARCH_BUSY_MESSAGE);
    expect(el.loading).toBe(false);
  });

  it('retries a 429 once after Retry-After and shows the results', async () => {
    vi.useFakeTimers();
    try {
      apiFetch
        .mockResolvedValueOnce(new Response('{}', { status: 429, headers: { 'Retry-After': '1' } }))
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ results: [], nextCursor: '' }), { status: 200 })
        );
      const el = makeSearch();
      const run = el.performSearch();
      await vi.advanceTimersByTimeAsync(1000);
      await run;
      expect(apiFetch).toHaveBeenCalledTimes(2);
      expect(apiFetch.mock.calls[1][1]?.signal).toBe(apiFetch.mock.calls[0][1]?.signal);
      expect(el.errorMsg).toBe('');
      expect(el.noResults).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows the busy message when the retry is refused too', async () => {
    vi.useFakeTimers();
    try {
      apiFetch.mockResolvedValue(new Response('{}', { status: 429 }));
      const el = makeSearch();
      const run = el.performSearch();
      await vi.advanceTimersByTimeAsync(1000);
      await run;
      expect(apiFetch).toHaveBeenCalledTimes(2);
      expect(el.errorMsg).toBe(mod.SEARCH_BUSY_MESSAGE);
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps the results on screen when "load more" fails', async () => {
    const el = makeSearch();
    el.results = [{ messageId: 'm1' }];
    el.nextCursor = 'c1';
    apiFetch.mockResolvedValueOnce(new Response('{}', { status: 503 }));
    await el.performSearch(true);
    expect(el.results).toEqual([{ messageId: 'm1' }]);
    expect(el.errorMsg).toBe('');
    expect(el.loadMoreError).toBe(mod.SEARCH_TIMEOUT_MESSAGE);
  });

  it('clears the message on a successful search', async () => {
    apiFetch.mockResolvedValueOnce(new Response('{}', { status: 503 }));
    const el = makeSearch();
    await el.performSearch();
    expect(el.errorMsg).toBe(mod.SEARCH_TIMEOUT_MESSAGE);

    apiFetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ results: [], nextCursor: '' }), { status: 200 })
    );
    await el.performSearch();
    expect(el.errorMsg).toBe('');
    expect(el.noResults).toBe(true);
  });
});
