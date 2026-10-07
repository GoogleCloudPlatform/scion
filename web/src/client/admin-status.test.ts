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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { clearAdminStatus, loadAdminStatus } from './admin-status.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Pending {
  resolve(r: Response): void;
  reject(e: unknown): void;
}

/** A fetch stub that records admin-status calls and lets a test hold them open. */
function stubFetch() {
  const pending: Pending[] = [];
  let hold = false;
  let next: () => Promise<Response> = () =>
    Promise.resolve(json({ isAdmin: true, isSuperAdmin: false, permissions: ['users.read'] }));
  const fn = vi.fn((input: string | URL | Request) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (!url.includes('/api/v1/auth/admin-status')) return Promise.resolve(json({}, 404));
    if (hold) {
      return new Promise<Response>((resolve, reject) => pending.push({ resolve, reject }));
    }
    return next();
  });
  vi.stubGlobal('fetch', fn);
  return {
    fn,
    calls: () => fn.mock.calls.length,
    hold: () => (hold = true),
    pending,
    respondWith: (f: () => Promise<Response>) => (next = f),
  };
}

const ADMIN = { isAdmin: true, isSuperAdmin: false, permissions: ['users.read'] };

describe('loadAdminStatus', () => {
  beforeEach(() => clearAdminStatus());
  afterEach(() => {
    clearAdminStatus();
    vi.unstubAllGlobals();
  });

  it('concurrent callers share one request', async () => {
    const f = stubFetch();
    const [a, b, c] = await Promise.all([
      loadAdminStatus('u1'),
      loadAdminStatus('u1'),
      loadAdminStatus('u1'),
    ]);
    expect(f.calls()).toBe(1);
    expect(a).toEqual(ADMIN);
    expect(b).toEqual(ADMIN);
    expect(c).toEqual(ADMIN);
  });

  it('a later caller for the same user reuses the result', async () => {
    const f = stubFetch();
    await loadAdminStatus('u1');
    expect(await loadAdminStatus('u1')).toEqual(ADMIN);
    expect(f.calls()).toBe(1);
  });

  it('fresh always sends a new request and replaces the shared value', async () => {
    const f = stubFetch();
    await loadAdminStatus('u1');
    f.respondWith(() => Promise.resolve(json({ isAdmin: false })));
    const fresh = await loadAdminStatus('u1', { fresh: true });
    expect(f.calls()).toBe(2);
    expect(fresh?.isAdmin).toBe(false);
    // The revocation is what the next shared reader sees.
    expect((await loadAdminStatus('u1'))?.isAdmin).toBe(false);
    expect(f.calls()).toBe(2);
  });

  it('a different user drops the value and sends a new request', async () => {
    const f = stubFetch();
    await loadAdminStatus('u1');
    f.respondWith(() => Promise.resolve(json({ isAdmin: false })));
    const other = await loadAdminStatus('u2');
    expect(f.calls()).toBe(2);
    expect(other?.isAdmin).toBe(false);
  });

  it('a request in flight for a previous user resolves to null and is not kept', async () => {
    const f = stubFetch();
    f.hold();
    const stale = loadAdminStatus('u1');
    const current = loadAdminStatus('u2');
    expect(f.calls()).toBe(2);
    // The old user's (admin) response lands after the user change.
    f.pending[0].resolve(json(ADMIN));
    expect(await stale).toBeNull();
    f.pending[1].resolve(json({ isAdmin: false }));
    expect((await current)?.isAdmin).toBe(false);
    // Nothing from the previous user was kept.
    expect((await loadAdminStatus('u2'))?.isAdmin).toBe(false);
    expect(f.calls()).toBe(2);
  });

  it('clearAdminStatus (sign-out) abandons a request in flight', async () => {
    const f = stubFetch();
    f.hold();
    const p = loadAdminStatus('u1');
    clearAdminStatus();
    f.pending[0].resolve(json(ADMIN));
    expect(await p).toBeNull();
  });

  it('nothing from an abandoned request is kept: the next caller asks again', async () => {
    const f = stubFetch();
    f.hold();
    const p = loadAdminStatus('u1');
    clearAdminStatus();
    f.pending[0].resolve(json(ADMIN));
    await p;
    expect(f.calls()).toBe(1);
    const again = loadAdminStatus('u1');
    expect(f.calls()).toBe(2);
    f.pending[1].resolve(json({ isAdmin: false }));
    expect((await again)?.isAdmin).toBe(false);
  });

  it('a non-2xx response is not cached and is not admin; the next caller retries', async () => {
    const f = stubFetch();
    f.respondWith(() => Promise.resolve(json({ isAdmin: true }, 500)));
    expect(await loadAdminStatus('u1')).toBeNull();
    f.respondWith(() => Promise.resolve(json(ADMIN)));
    expect(await loadAdminStatus('u1')).toEqual(ADMIN);
    expect(f.calls()).toBe(2);
  });

  it('a network error is not cached either', async () => {
    const f = stubFetch();
    f.respondWith(() => Promise.reject(new TypeError('offline')));
    expect(await loadAdminStatus('u1')).toBeNull();
    f.respondWith(() => Promise.resolve(json(ADMIN)));
    expect(await loadAdminStatus('u1')).toEqual(ADMIN);
    expect(f.calls()).toBe(2);
  });

  it('a fresh revocation is not overwritten by an older request that resolves later', async () => {
    const f = stubFetch();
    f.hold();
    const old = loadAdminStatus('u1'); // a nav's request
    const fresh = loadAdminStatus('u1', { fresh: true }); // the admin route guard
    expect(f.calls()).toBe(2);
    f.pending[1].resolve(json({ isAdmin: false }));
    expect((await fresh)?.isAdmin).toBe(false);
    // The older request now lands with the stale admin answer.
    f.pending[0].resolve(json(ADMIN));
    expect((await old)?.isAdmin).toBe(false);
    expect((await loadAdminStatus('u1'))?.isAdmin).toBe(false);
    expect(f.calls()).toBe(2);
  });

  it('a failed fresh request drops the shared value; the next caller retries', async () => {
    const f = stubFetch();
    expect(await loadAdminStatus('u1')).toEqual(ADMIN);
    f.respondWith(() => Promise.resolve(json({}, 503)));
    expect(await loadAdminStatus('u1', { fresh: true })).toBeNull();
    f.respondWith(() => Promise.reject(new TypeError('offline')));
    expect(await loadAdminStatus('u1')).toBeNull(); // not the old admin value
    expect(f.calls()).toBe(3);
    f.respondWith(() => Promise.resolve(json({ isAdmin: false })));
    expect((await loadAdminStatus('u1'))?.isAdmin).toBe(false);
    expect(f.calls()).toBe(4);
  });

  it('a superseded request waits for the newer one still in flight (cached empty)', async () => {
    const f = stubFetch();
    f.hold();
    const n = loadAdminStatus('u1'); // startup or a nav
    const fresh = loadAdminStatus('u1', { fresh: true }); // the guard
    // N settles first, as admin, while nothing is cached yet.
    f.pending[0].resolve(json(ADMIN));
    let nValue: unknown = 'pending';
    void n.then((v) => (nValue = v));
    await new Promise((r) => setTimeout(r, 0));
    expect(nValue).toBe('pending'); // not null, not its own stale answer
    f.pending[1].resolve(json({ isAdmin: true, isSuperAdmin: true, permissions: [] }));
    const fValue = await fresh;
    expect(await n).toEqual(fValue);
    expect(fValue?.isSuperAdmin).toBe(true);
  });

  it('an earlier fresh request resolves to the newest one, not the seeded value', async () => {
    const f = stubFetch();
    expect(await loadAdminStatus('u1')).toEqual(ADMIN); // seeded
    f.hold();
    const f1 = loadAdminStatus('u1', { fresh: true });
    const f2 = loadAdminStatus('u1', { fresh: true });
    f.pending[0].resolve(json({ isAdmin: false })); // F1: a revocation
    f.pending[1].resolve(
      json({ isAdmin: true, isSuperAdmin: false, permissions: ['groups.read'] })
    );
    const v2 = await f2;
    expect(await f1).toEqual(v2);
    expect(v2?.permissions).toEqual(['groups.read']);
    expect(await loadAdminStatus('u1')).toEqual(v2);
  });

  it('no user id: null and no request', async () => {
    const f = stubFetch();
    expect(await loadAdminStatus('')).toBeNull();
    expect(await loadAdminStatus(null)).toBeNull();
    expect(f.calls()).toBe(0);
  });
});

describe('cold load: startup plus both nav instances', () => {
  beforeAll(async () => {
    await import('../components/shared/nav.js');
  }, 60_000);
  beforeEach(() => clearAdminStatus());
  afterEach(() => {
    document.body.innerHTML = '';
    clearAdminStatus();
    vi.unstubAllGlobals();
  });

  it('sends admin-status exactly once', async () => {
    const f = stubFetch();
    const user = { id: 'u1', email: 'u@example.com', name: 'U', role: 'member' };
    // Startup starts the shared request (main.ts init) ...
    void loadAdminStatus(user.id);
    // ... and the shell renders two nav instances for the same user.
    const navs = [0, 1].map(() => {
      const el = document.createElement('scion-nav') as HTMLElement & {
        user: unknown;
        updateComplete: Promise<boolean>;
      };
      el.user = user;
      document.body.appendChild(el);
      return el;
    });
    await Promise.all(navs.map((n) => n.updateComplete));
    await vi.waitFor(() => {
      for (const n of navs) {
        expect((n as unknown as { adminStatus: unknown }).adminStatus).toEqual(ADMIN);
      }
    });
    expect(f.calls()).toBe(1);
  });
});
