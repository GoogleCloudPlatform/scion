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
 * The chat page's bounded user reads: the shared recent-users page, the
 * server-side search, and by-id lookups, all feeding one name cache.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiFetch } from './api.js';
import {
  loadRecentUsers,
  peekUser,
  RECENT_USERS_TTL_MS,
  RESOLVE_USER_TIMEOUT_MS,
  USER_SEARCH_TIMEOUT_MS,
  resetUserDirectory,
  resolveUser,
  searchUsers,
  setUserDirectoryClock,
  userDisplayName,
} from './user-directory.js';

vi.mock('./api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

let clock = 0;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

function urls(): string[] {
  return vi.mocked(apiFetch).mock.calls.map((c) => c[0]);
}

beforeEach(() => {
  clock = 1_000;
  setUserDirectoryClock(() => clock);
  resetUserDirectory();
  vi.mocked(apiFetch).mockReset();
});

afterEach(() => {
  setUserDirectoryClock(() => Date.now());
});

describe('loadRecentUsers', () => {
  it('asks for one page of the most recently seen users and reports truncation', async () => {
    vi.mocked(apiFetch).mockResolvedValue(
      json({ users: [{ id: 'u1', displayName: 'Ann' }], nextCursor: '100' })
    );
    const page = await loadRecentUsers();
    expect(urls()).toEqual(['/api/v1/users?sort=lastSeen&dir=desc&limit=100']);
    expect(page).toEqual({ users: [{ id: 'u1', displayName: 'Ann' }], truncated: true });
    expect(peekUser('u1')?.displayName).toBe('Ann');
  });

  it('a page with no cursor is not truncated', async () => {
    vi.mocked(apiFetch).mockResolvedValue(json({ users: [{ id: 'u1', displayName: 'Ann' }] }));
    expect((await loadRecentUsers())?.truncated).toBe(false);
  });

  it('concurrent and later callers share one request until the TTL, then it reloads', async () => {
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(json({ users: [] })));
    await Promise.all([loadRecentUsers(), loadRecentUsers(), loadRecentUsers()]);
    expect(urls().length).toBe(1);

    clock += RECENT_USERS_TTL_MS - 1;
    await loadRecentUsers();
    expect(urls().length).toBe(1);

    clock += 1;
    await loadRecentUsers();
    expect(urls().length).toBe(2);
  });

  it('a failed load is not cached: the next call asks again', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(json({}, 500))
      .mockResolvedValueOnce(json({ users: [{ id: 'u1', displayName: 'Ann' }] }));
    expect(await loadRecentUsers()).toBeNull();
    expect((await loadRecentUsers())?.users.map((u) => u.id)).toEqual(['u1']);
    expect(urls().length).toBe(2);
  });
});

describe('searchUsers', () => {
  it("uses the hub's server-side search and seeds the name cache", async () => {
    vi.mocked(apiFetch).mockResolvedValue(
      json({ users: [{ id: 'u9', displayName: 'Zed', email: 'zed@example.com' }] })
    );
    const found = await searchUsers('  zed ');
    expect(urls()).toEqual(['/api/v1/users?search=zed&limit=20']);
    expect(found.map((u) => u.id)).toEqual(['u9']);
    expect(peekUser('u9')?.email).toBe('zed@example.com');
  });

  it('passes a status filter to the hub when given one', async () => {
    vi.mocked(apiFetch).mockResolvedValue(json({ users: [] }));
    await searchUsers('zed', { status: 'active' });
    expect(urls()).toEqual(['/api/v1/users?search=zed&limit=20&status=active']);
  });

  it('an empty query asks nothing', async () => {
    expect(await searchUsers('   ')).toEqual([]);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('a failure resolves empty; an abort rejects', async () => {
    vi.mocked(apiFetch).mockResolvedValueOnce(json({}, 500));
    expect(await searchUsers('a')).toEqual([]);

    const controller = new AbortController();
    vi.mocked(apiFetch).mockImplementationOnce(() => {
      controller.abort();
      return Promise.reject(new DOMException('aborted', 'AbortError'));
    });
    await expect(searchUsers('a', { signal: controller.signal })).rejects.toThrow();
  });
});

describe('searchUsers timeout', () => {
  it('a hung search resolves empty at the timeout rather than rejecting', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(apiFetch).mockImplementationOnce(
        (_url, init) =>
          new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError'))
            );
          })
      );
      const result = searchUsers('zed', { signal: new AbortController().signal });
      await vi.advanceTimersByTimeAsync(USER_SEARCH_TIMEOUT_MS);
      expect(await result).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("the caller's abort reaches the request and rejects", async () => {
    const caller = new AbortController();
    vi.mocked(apiFetch).mockImplementationOnce(
      (_url, init) =>
        new Promise<Response>((_, reject) => {
          init?.signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError'))
          );
        })
    );
    const result = searchUsers('zed', { signal: caller.signal });
    caller.abort();
    await expect(result).rejects.toThrow();
  });
});

describe('resolveUser', () => {
  it('fetches an unknown user once; concurrent callers share the request', async () => {
    vi.mocked(apiFetch).mockResolvedValue(json({ id: 'u1', displayName: 'Ann' }));
    const [a, b] = await Promise.all([resolveUser('u1'), resolveUser('u1')]);
    expect(a?.displayName).toBe('Ann');
    expect(b).toBe(a);
    expect(await resolveUser('u1')).toBe(a);
    expect(urls()).toEqual(['/api/v1/users/u1']);
  });

  it('answers from a row any other read already cached, with no request', async () => {
    vi.mocked(apiFetch).mockResolvedValue(json({ users: [{ id: 'u1', displayName: 'Ann' }] }));
    await loadRecentUsers();
    vi.mocked(apiFetch).mockClear();
    expect((await resolveUser('u1'))?.displayName).toBe('Ann');
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('remembers a 404 for the TTL only, so a user added later resolves', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(json({}, 404))
      .mockResolvedValueOnce(json({ id: 'u1', displayName: 'Ann' }));
    expect(await resolveUser('u1')).toBeNull();
    expect(await resolveUser('u1')).toBeNull();
    expect(urls().length).toBe(1);

    clock += RECENT_USERS_TTL_MS;
    expect((await resolveUser('u1'))?.displayName).toBe('Ann');
    expect(urls().length).toBe(2);
  });

  it('a hung lookup is aborted at the timeout, not remembered, and asked again', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(apiFetch).mockImplementationOnce(
        (_url, init) =>
          new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError'))
            );
          })
      );
      const hung = resolveUser('u1');
      // Joined while in flight.
      expect(resolveUser('u1')).toBe(hung);
      await vi.advanceTimersByTimeAsync(RESOLVE_USER_TIMEOUT_MS);
      expect(await hung).toBeNull();

      vi.mocked(apiFetch).mockResolvedValueOnce(json({ id: 'u1', displayName: 'Ann' }));
      expect((await resolveUser('u1'))?.displayName).toBe('Ann');
      expect(urls().length).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a lookup whose fetch throws synchronously is not pinned as in flight', async () => {
    vi.mocked(apiFetch)
      .mockImplementationOnce(() => {
        throw new Error('boom');
      })
      .mockResolvedValueOnce(json({ id: 'u1', displayName: 'Ann' }));
    expect(await resolveUser('u1')).toBeNull();
    expect((await resolveUser('u1'))?.displayName).toBe('Ann');
  });

  it('does not remember other failures', async () => {
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(json({}, 500))
      .mockResolvedValueOnce(json({ id: 'u1', displayName: 'Ann' }));
    expect(await resolveUser('u1')).toBeNull();
    expect((await resolveUser('u1'))?.displayName).toBe('Ann');
    expect(urls().length).toBe(2);
  });
});

describe('userDisplayName', () => {
  it('falls back from display name to email to id', () => {
    expect(userDisplayName({ id: 'u1', displayName: 'Ann', email: 'a@x' })).toBe('Ann');
    expect(userDisplayName({ id: 'u1', displayName: '', email: 'a@x' })).toBe('a@x');
    expect(userDisplayName({ id: 'u1', displayName: '' })).toBe('u1');
  });
});
