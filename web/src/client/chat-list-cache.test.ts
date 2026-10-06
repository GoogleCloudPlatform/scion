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

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

import { SharedJsonLoad, chatDMsLoad, chatSpacesLoad } from './chat-list-cache.js';

/** A fetcher whose calls resolve only when the test says so. */
function deferredFetcher(): {
  fetchOnce: () => Promise<{ n: number } | null>;
  calls: number;
  resolve: (index: number, value: { n: number } | null) => void;
  reject: (index: number) => void;
} {
  const pending: Array<{
    resolve: (v: { n: number } | null) => void;
    reject: (e: Error) => void;
  }> = [];
  const state = {
    calls: 0,
    fetchOnce: () => {
      state.calls++;
      return new Promise<{ n: number } | null>((resolve, reject) => {
        pending.push({ resolve, reject });
      });
    },
    resolve: (index: number, value: { n: number } | null) => pending[index].resolve(value),
    reject: (index: number) => pending[index].reject(new Error('offline')),
  };
  return state;
}

describe('SharedJsonLoad', () => {
  let now = 1_000;
  const clock = (): number => now;

  beforeEach(() => {
    now = 1_000;
  });

  it('shares one in-flight request between initial loads', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    const a = load.load({ maxAgeMs: 5_000 });
    const b = load.load({ maxAgeMs: 5_000 });
    expect(f.calls).toBe(1);

    f.resolve(0, { n: 1 });
    expect(await a).toEqual({ n: 1 });
    expect(await b).toBe(await a);
  });

  it('reuses a completed result while it is young enough, then fetches again', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);
    const first = load.load({ maxAgeMs: 5_000 });
    f.resolve(0, { n: 1 });
    await first;

    now += 5_000;
    expect(await load.load({ maxAgeMs: 5_000 })).toEqual({ n: 1 });
    expect(f.calls).toBe(1);

    now += 1;
    const later = load.load({ maxAgeMs: 5_000 });
    expect(f.calls).toBe(2);
    f.resolve(1, { n: 2 });
    expect(await later).toEqual({ n: 2 });
  });

  it('a refresh always starts a new request, even with one in flight', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    const initial = load.load({ maxAgeMs: 5_000 });
    // Something changed after the first request went out: it may predate
    // the change, so the refresh must not join it.
    const refresh = load.load();
    expect(f.calls).toBe(2);

    f.resolve(0, { n: 1 });
    f.resolve(1, { n: 2 });
    expect(await initial).toEqual({ n: 1 });
    expect(await refresh).toEqual({ n: 2 });
  });

  it('a later initial load joins the refresh rather than the older request', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    void load.load({ maxAgeMs: 5_000 });
    const refresh = load.load();
    const joiner = load.load({ maxAgeMs: 5_000 });
    expect(f.calls).toBe(2);

    f.resolve(0, { n: 1 });
    f.resolve(1, { n: 2 });
    expect(await joiner).toBe(await refresh);
  });

  it('does not cache a failure, so the next caller retries', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    const failed = load.load({ maxAgeMs: 5_000 });
    f.resolve(0, null);
    expect(await failed).toBeNull();

    const retry = load.load({ maxAgeMs: 5_000 });
    expect(f.calls).toBe(2);
    f.resolve(1, { n: 1 });
    expect(await retry).toEqual({ n: 1 });
  });

  it('turns a thrown fetch into null for every caller sharing it', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    const a = load.load({ maxAgeMs: 5_000 });
    const b = load.load({ maxAgeMs: 5_000 });
    f.reject(0);
    expect(await a).toBeNull();
    expect(await b).toBeNull();
  });

  it('a late failure of a superseded request does not drop the newer result', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    const old = load.load({ maxAgeMs: 5_000 });
    const fresh = load.load();
    f.resolve(1, { n: 2 });
    await fresh;
    f.resolve(0, null);
    await old;

    expect(await load.load({ maxAgeMs: 5_000 })).toEqual({ n: 2 });
    expect(f.calls).toBe(2);
  });

  it('startedAfter shares only a request that started after the event', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);

    void load.load(); // sent before the event
    const eventAt = now;
    // Same tick as the event: it may have been sent before it.
    expect(f.calls).toBe(1);
    void load.load({ startedAfter: eventAt });
    expect(f.calls).toBe(2);

    // A request another owner sent after the event is shared.
    now += 1;
    const ownerRefresh = load.load();
    const shared = load.load({ startedAfter: eventAt });
    expect(f.calls).toBe(3);
    f.resolve(2, { n: 3 });
    expect(await shared).toBe(await ownerRefresh);
  });

  it('startedAfter shares a completed request too, however old the event', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);
    const eventAt = now;
    now += 1;
    const first = load.load();
    f.resolve(0, { n: 1 });
    await first;

    now += 60_000;
    expect(await load.load({ startedAfter: eventAt })).toEqual({ n: 1 });
    expect(f.calls).toBe(1);
  });

  it('invalidate makes the next initial load fetch', async () => {
    const f = deferredFetcher();
    const load = new SharedJsonLoad(f.fetchOnce, clock);
    const first = load.load({ maxAgeMs: 5_000 });
    f.resolve(0, { n: 1 });
    await first;

    load.invalidate();
    void load.load({ maxAgeMs: 5_000 });
    expect(f.calls).toBe(2);
  });
});

describe('page-wide chat list loads', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    chatSpacesLoad.invalidate();
    chatDMsLoad.invalidate();
  });

  it('read their endpoints and hand every caller the parsed body', async () => {
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve(
        new Response(JSON.stringify(url.endsWith('/dms') ? { dms: [1] } : { spaces: [2] }), {
          status: 200,
        })
      )
    );

    const [spacesA, spacesB, dms] = await Promise.all([
      chatSpacesLoad.load({ maxAgeMs: 5_000 }),
      chatSpacesLoad.load({ maxAgeMs: 5_000 }),
      chatDMsLoad.load({ maxAgeMs: 5_000 }),
    ]);

    expect(spacesA).toEqual({ spaces: [2] });
    expect(spacesB).toBe(spacesA);
    expect(dms).toEqual({ dms: [1] });
    expect(apiFetch.mock.calls.map((c) => c[0])).toEqual([
      '/api/v1/chat/spaces',
      '/api/v1/chat/dms',
    ]);
  });

  it('resolve null for a non-OK response', async () => {
    apiFetch.mockResolvedValue(new Response('{}', { status: 503 }));
    expect(await chatSpacesLoad.load()).toBeNull();
  });
});
