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
  VisibleReconciler,
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
  it('serves project threads and both DM kinds', () => {
    expect(conversationSupportsStars('topic-1')).toBe(true);
    expect(conversationSupportsStars('dm:agent:a:user:b')).toBe(true);
    expect(conversationSupportsStars('dm:user:a:user:b')).toBe(true);
    expect(conversationSupportsStars('')).toBe(false);
  });
});

describe('ChatStarsController', () => {
  const M1 = mid(1);
  const M2 = mid(2);

  it('marks a starred-only page starred without a request, leaving pending and deleted IDs', async () => {
    const fetchState = vi.fn(() => Promise.resolve([]));
    let releasePut!: () => void;
    const put = vi.fn(() => new Promise<void>((r) => (releasePut = r)));
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState, deleteStar: put }));
    c.setConversation('topic-1');
    const events: string[] = [];
    c.addEventListener('change', () => events.push('change'));
    c.evict(M2);
    const pending = c.setStarred(mid(3), 'topic-1', false);
    events.length = 0;
    c.markStarred([M1, M2, mid(3), 'local-row']);
    expect(c.state(M1)).toBe('starred');
    expect(c.state(M2)).toBe('unstarred');
    expect(c.state(mid(3))).toBe('unstarred');
    expect(events).toEqual(['change']);
    // Already known as starred: no change event.
    c.markStarred([M1]);
    expect(events).toEqual(['change']);
    expect(fetchState).not.toHaveBeenCalled();
    releasePut();
    await pending;
  });

  it('evicts a deleted message: unstarred at once, lists refetch, no request', async () => {
    const fetchState = vi.fn((ids: readonly string[]) => Promise.resolve([...ids]));
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('dm:agent:a:user:b');
    await c.load([M1, M2]);
    expect(c.state(M1)).toBe('starred');
    const events: string[] = [];
    c.addEventListener('change', () => events.push('change'));
    c.addEventListener('collection-changed', () => events.push('collection-changed'));
    c.evict(M1);
    expect(c.state(M1)).toBe('unstarred');
    expect(c.state(M2)).toBe('starred');
    expect(events).toEqual(['change', 'collection-changed']);
    expect(fetchState).toHaveBeenCalledTimes(1);
    // A later answer that still lists it (a request that raced the
    // deletion) cannot restore the star.
    await c.invalidate();
    expect(c.state(M1)).toBe('unstarred');
    // Evicting an unknown or unstarrable ID is harmless.
    c.evict('local-row');
    c.evict(mid(99));
    expect(c.state(mid(99))).toBe('unstarred');
  });

  it('a star request in flight when the message is deleted does not leave it starred', async () => {
    const put = deferred<string>();
    const c = new ChatStarsController(fakeApi({ putStar: vi.fn(() => put.promise) }));
    c.setConversation('t1');
    await c.load([M1]);
    const pending = c.setStarred(M1, 't1', true);
    expect(c.state(M1)).toBe('starred');
    c.evict(M1);
    expect(c.state(M1)).toBe('unstarred');
    put.resolve('2026-10-22T10:00:00Z');
    await pending;
    expect(c.state(M1)).toBe('unstarred');
  });

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

  it('never marks live a message whose state request failed', async () => {
    let fail = true;
    const fetchState = vi.fn(() =>
      fail ? Promise.reject(new Error('offline')) : Promise.resolve([M1])
    );
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    await c.load([M1]);
    c.markLive([M1]);
    expect(c.state(M1)).toBe('unknown');
    fail = false;
    await c.retryFailed();
    expect(c.state(M1)).toBe('starred');
  });

  it('fetches a live message instead of marking it while a refresh is due', async () => {
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn((_ids: readonly string[]) => {
      const d = deferred<string[]>();
      gates.push(d);
      return d.promise;
    });
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    const load = c.load([M1]);
    gates[0].resolve([]);
    await load;
    // Another tab starred M2 just before its message event arrived.
    const refresh = c.invalidate();
    c.markLive([M2]);
    expect(c.state(M2)).toBe('unknown');
    gates[1].resolve([]);
    await vi.waitFor(() => expect(gates.length).toBe(3));
    // The extra pass asks only for the live message, not every known state.
    expect(fetchState.mock.calls[2][0]).toEqual([M2]);
    gates[2].resolve([M2]);
    await refresh;
    expect(c.state(M2)).toBe('starred');
    // Caught up again: a live message is marked without a request.
    const M3 = mid(3);
    c.markLive([M3]);
    expect(c.state(M3)).toBe('unstarred');
    expect(fetchState).toHaveBeenCalledTimes(3);
  });

  it('does not mark live after an invalidation whose refresh failed', async () => {
    let fail = false;
    const fetchState = vi.fn((_ids: readonly string[]) =>
      fail ? Promise.reject(new Error('offline')) : Promise.resolve([])
    );
    const c = new ChatStarsController(fakeApi({ fetchStarState: fetchState }));
    c.setConversation('t1');
    await c.load([M1]);
    fail = true;
    await c.invalidate(); // the refresh fails: the states did not catch up
    c.markLive([M2]);
    expect(c.state(M2)).toBe('unknown');
    await vi.waitFor(() => expect(fetchState.mock.calls.flatMap((cl) => cl[0])).toContain(M2));
    fail = false;
    // The refresh markLive started may still run; the retry then makes it
    // pass once more.
    await c.retryFailed();
    await vi.waitFor(() => expect(c.state(M2)).toBe('unstarred'));
    // Caught up: the next live message is marked without a request.
    const calls = fetchState.mock.calls.length;
    c.markLive([mid(3)]);
    expect(c.state(mid(3))).toBe('unstarred');
    expect(fetchState).toHaveBeenCalledTimes(calls);
  });

  it('refetches the IDs of a load that was in flight during an invalidation', async () => {
    const gates: Array<ReturnType<typeof deferred<string[]>>> = [];
    const fetchState = vi.fn((_ids: readonly string[]) => {
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

describe('VisibleReconciler', () => {
  let visibility: DocumentVisibilityState = 'visible';

  function setup(): { run: ReturnType<typeof vi.fn>; r: VisibleReconciler } {
    vi.useFakeTimers();
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility);
    const run = vi.fn();
    return { run, r: new VisibleReconciler(run, 60_000) };
  }

  afterEach(() => {
    visibility = 'visible';
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('runs once a minute while started and visible', async () => {
    const { run, r } = setup();
    r.start();
    await vi.advanceTimersByTimeAsync(59_999);
    expect(run).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(run).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(run).toHaveBeenCalledTimes(2);
    r.stop();
    await vi.advanceTimersByTimeAsync(300_000);
    expect(run).toHaveBeenCalledTimes(2);
  });

  it('a refetch for another reason restarts the interval', async () => {
    const { run, r } = setup();
    r.start();
    await vi.advanceTimersByTimeAsync(50_000);
    r.noteRefetched();
    await vi.advanceTimersByTimeAsync(50_000);
    expect(run).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(run).toHaveBeenCalledTimes(1);
    r.stop();
  });

  it('never polls while hidden, and runs once on return', async () => {
    const { run, r } = setup();
    r.start();
    visibility = 'hidden';
    await vi.advanceTimersByTimeAsync(600_000);
    expect(run).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
    visibility = 'visible';
    document.dispatchEvent(new Event('visibilitychange'));
    // The window's focus right after the same return refetches nothing more.
    window.dispatchEvent(new Event('focus'));
    expect(run).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(run).toHaveBeenCalledTimes(2);
    r.stop();
  });

  it('ignores page returns once stopped', () => {
    const { run, r } = setup();
    r.start();
    r.stop();
    vi.advanceTimersByTime(5_000);
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('focus'));
    expect(run).not.toHaveBeenCalled();
    expect(r.started).toBe(false);
  });
});
