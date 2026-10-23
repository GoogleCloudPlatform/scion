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

import { describe, it, expect, vi, afterEach } from 'vitest';

import {
  ChatStarsController,
  STAR_STATE_BATCH,
  chatStarsLocalChanges,
  conversationSupportsStars,
  fetchStarDetail,
  fetchStarPage,
  fetchStarState,
  putStar,
  type StarsApi,
} from './chat-stars.js';

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (v: T) => void;
  reject: (e: unknown) => void;
} {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** A UUID-shaped message ID (the controller ignores other IDs). */
function mid(n: number | string): string {
  const hex = Number(n).toString(16).padStart(12, '0');
  return `00000000-0000-4000-8000-${hex}`;
}

function fakeApi(overrides: Partial<StarsApi> = {}): StarsApi {
  return {
    putStar: vi.fn(() => Promise.resolve('2026-10-22T10:00:00Z')),
    deleteStar: vi.fn(() => Promise.resolve()),
    fetchStarState: vi.fn(() => Promise.resolve([])),
    ...overrides,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('conversationSupportsStars', () => {
  it('serves project threads only in phase 1', () => {
    expect(conversationSupportsStars('topic-1')).toBe(true);
    expect(conversationSupportsStars('dm:user:a:agent:b')).toBe(false);
    expect(conversationSupportsStars('')).toBe(false);
  });
});

describe('ChatStarsController', () => {
  const M1 = mid(1);
  const M2 = mid(2);

  it('loads unknown IDs in batches of 200 and never refetches known ones', async () => {
    const starredSet = new Set([mid(7)]);
    const fetchState = vi.fn((ids: readonly string[]) =>
      Promise.resolve(ids.filter((id) => starredSet.has(id)))
    );
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const ids = Array.from({ length: 450 }, (_, i) => mid(i));
    await c.load([...ids, 'local-pending-row']);
    expect(fetchState).toHaveBeenCalledTimes(3);
    expect(fetchState.mock.calls.map((call) => call[0].length)).toEqual([
      STAR_STATE_BATCH,
      STAR_STATE_BATCH,
      50,
    ]);
    expect(fetchState.mock.calls.flatMap((call) => call[0])).not.toContain('local-pending-row');
    expect(c.state(mid(7))).toBe('starred');
    expect(c.state(mid(8))).toBe('unstarred');
    expect(c.state(mid(9999))).toBe('unknown');
    await c.load([mid(7), mid(8)]);
    expect(fetchState).toHaveBeenCalledTimes(3);
  });

  it('leaves state unknown when the state request fails', async () => {
    const c = new ChatStarsController(
      fakeApi({ fetchStarState: vi.fn(() => Promise.reject(new Error('x'))) })
    );
    c.setConversation('t1');
    await c.load([M1]);
    expect(c.state(M1)).toBe('unknown');
  });

  it('drops a state response for a previous conversation', async () => {
    const d = deferred<string[]>();
    const c = new ChatStarsController(fakeApi({ fetchStarState: vi.fn(() => d.promise) }));
    c.setConversation('t1');
    const load = c.load([M1]);
    c.setConversation('t2');
    d.resolve([M1]);
    await load;
    expect(c.state(M1)).toBe('unknown');
  });

  it('is optimistic with one request in flight per message, and confirms on success', async () => {
    const d = deferred<string>();
    const api = fakeApi({ putStar: vi.fn(() => d.promise) });
    const c = new ChatStarsController(api);
    c.setConversation('t1');
    await c.load([M1]);
    const local = vi.fn();
    chatStarsLocalChanges.addEventListener('changed', local);
    const first = c.setStarred(M1, 't1', true);
    expect(c.state(M1)).toBe('starred');
    expect(c.isPending(M1)).toBe(true);
    await c.setStarred(M1, 't1', false); // ignored while pending
    expect(api.putStar).toHaveBeenCalledTimes(1);
    expect(api.deleteStar).not.toHaveBeenCalled();
    d.resolve('2026-10-22T10:00:00Z');
    await first;
    expect(c.isPending(M1)).toBe(false);
    expect(c.state(M1)).toBe('starred');
    expect(local).toHaveBeenCalledTimes(1);
    expect((local.mock.calls[0][0] as CustomEvent).detail).toEqual({ conversationKey: 't1' });
    chatStarsLocalChanges.removeEventListener('changed', local);
  });

  it('restores the prior state and rethrows when the mutation fails', async () => {
    const api = fakeApi({
      deleteStar: vi.fn(() => Promise.reject(new Error('offline'))),
      fetchStarState: vi.fn(() => Promise.resolve([M1])),
    });
    const c = new ChatStarsController(api);
    c.setConversation('t1');
    await c.load([M1]);
    expect(c.state(M1)).toBe('starred');
    await expect(c.setStarred(M1, 't1', false)).rejects.toThrow('offline');
    expect(c.state(M1)).toBe('starred');
    expect(c.isPending(M1)).toBe(false);
  });

  it('refetches known IDs on invalidation and coalesces invalidations during a refresh', async () => {
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn(() => {
      const d = deferred<string[]>();
      gates.push(d);
      return d.promise;
    });
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const load = c.load([M1, M2]);
    gates[0].resolve([]);
    await load;
    const collection = vi.fn();
    c.addEventListener('collection-changed', collection);

    const refresh = c.invalidate();
    void c.invalidate();
    void c.invalidate();
    expect(collection).toHaveBeenCalledTimes(3);
    gates[1].resolve([M2]);
    await vi.waitFor(() => expect(gates.length).toBe(3));
    gates[2].resolve([M1]);
    await refresh;
    expect(fetchState).toHaveBeenCalledTimes(3);
    expect(c.state(M1)).toBe('starred');
    expect(c.state(M2)).toBe('unstarred');
  });

  it('keeps a confirmed PUT when an older refresh answers after it', async () => {
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn(() => {
      const d = deferred<string[]>();
      gates.push(d);
      return d.promise;
    });
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const load = c.load([M1]);
    gates[0].resolve([]);
    await load;
    expect(c.state(M1)).toBe('unstarred');

    // A refresh reads the state, then the user stars M1 and the PUT is
    // confirmed before the (now stale) refresh answer arrives.
    const refresh = c.invalidate();
    expect(gates.length).toBe(2);
    await c.setStarred(M1, 't1', true);
    expect(c.state(M1)).toBe('starred');
    gates[1].resolve([]);
    await refresh;
    expect(c.state(M1)).toBe('starred');
  });

  it('does not let a refresh answer replace a pending mutation', async () => {
    const put = deferred<string>();
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn(() => {
      const d = deferred<string[]>();
      gates.push(d);
      return d.promise;
    });
    const c = new ChatStarsController(
      fakeApi({ fetchStarState: fetchState, putStar: vi.fn(() => put.promise) })
    );
    c.setConversation('t1');
    const load = c.load([M1]);
    gates[0].resolve([]);
    await load;

    const starring = c.setStarred(M1, 't1', true);
    const refresh = c.invalidate();
    gates[1].resolve([]);
    await refresh;
    put.resolve('2026-10-22T10:00:00Z');
    await starring;
    expect(c.state(M1)).toBe('starred');
  });

  it('keeps loading after a failed batch and retries it on the next invalidation', async () => {
    let call = 0;
    const fetchState = vi.fn((ids: readonly string[]) => {
      call++;
      if (call === 1) return Promise.reject(new Error('offline'));
      return Promise.resolve(ids.filter((id) => id === mid(0) || id === mid(300)));
    });
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const ids = Array.from({ length: 450 }, (_, i) => mid(i));
    await c.load(ids);
    expect(fetchState).toHaveBeenCalledTimes(3);
    expect(c.state(mid(0))).toBe('unknown'); // batch 1 failed
    expect(c.state(mid(300))).toBe('starred'); // batch 2 still ran
    expect(c.state(mid(449))).toBe('unstarred'); // batch 3 still ran

    await c.invalidate();
    const retried = fetchState.mock.calls.slice(3).flatMap((cl) => cl[0]);
    expect(retried).toEqual(expect.arrayContaining([mid(0), mid(199)]));
    expect(c.state(mid(0))).toBe('starred');
    expect(c.state(mid(199))).toBe('unstarred');
  });

  it('retries failed IDs on retryFailed and does nothing when none failed', async () => {
    let fail = true;
    const fetchState = vi.fn(() =>
      fail ? Promise.reject(new Error('offline')) : Promise.resolve([M1])
    );
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    await c.load([M1]);
    expect(c.state(M1)).toBe('unknown');
    fail = false;
    await c.retryFailed();
    expect(c.state(M1)).toBe('starred');
    await c.retryFailed();
    expect(fetchState).toHaveBeenCalledTimes(2);
  });

  it('marks live messages unstarred without a request, leaving known ones alone', async () => {
    const fetchState = vi.fn(() => Promise.resolve([M1]));
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    await c.load([M1]);
    c.markLive([M1, M2, 'local-row']);
    expect(c.state(M1)).toBe('starred');
    expect(c.state(M2)).toBe('unstarred');
    expect(c.state('local-row')).toBe('unknown');
    expect(fetchState).toHaveBeenCalledTimes(1);
  });

  it('refetches the IDs of a load that was in flight during an invalidation', async () => {
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn(() => {
      const d = deferred<string[]>();
      gates.push(d);
      return d.promise;
    });
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const load = c.load([M1]);
    expect(gates.length).toBe(1);
    // Another tab stars M1 while the load runs; the load read older data.
    await c.invalidate();
    gates[0].resolve([]);
    await vi.waitFor(() => expect(gates.length).toBe(2));
    expect(c.state(M1)).toBe('unknown');
    expect(fetchState.mock.calls[1][0]).toEqual([M1]);
    gates[1].resolve([M1]);
    await load;
    expect(c.state(M1)).toBe('starred');
  });
});

describe('star API helpers', () => {
  function stubFetch(status: number, body: unknown) {
    const fn = vi.fn((_url: string, _init?: RequestInit) =>
      Promise.resolve(
        new Response(status === 204 ? null : JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', fn);
    return fn;
  }

  it('PUTs the conversation key and returns the save time', async () => {
    const fn = stubFetch(200, { starred: true, starredAt: '2026-10-22T10:00:00Z' });
    await expect(putStar('m1', 't1')).resolves.toBe('2026-10-22T10:00:00Z');
    expect(fn.mock.calls[0][0]).toBe('/api/v1/chat/stars/m1');
    expect(fn.mock.calls[0][1]?.method).toBe('PUT');
    expect(JSON.parse(fn.mock.calls[0][1]?.body as string)).toEqual({ conversationKey: 't1' });
  });

  it('repeats messageId for state and passes conversation, limit and cursor for pages', async () => {
    let fn = stubFetch(200, { starredMessageIds: ['a'] });
    await expect(fetchStarState(['a', 'b'])).resolves.toEqual(['a']);
    expect(fn.mock.calls[0][0]).toBe('/api/v1/chat/stars/state?messageId=a&messageId=b');
    fn = stubFetch(200, { items: [], nextCursor: 'c1.x' });
    await expect(fetchStarPage('t1', 20, 'c1.prev')).resolves.toEqual({
      items: [],
      nextCursor: 'c1.x',
    });
    expect(fn.mock.calls[0][0]).toBe(
      '/api/v1/chat/stars?conversationKey=t1&limit=20&cursor=c1.prev'
    );
  });

  it('answers null for a star that is no longer available', async () => {
    stubFetch(404, { error: { message: 'Message not found' } });
    await expect(fetchStarDetail('m1')).resolves.toBeNull();
  });
});
