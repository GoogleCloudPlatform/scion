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
 * Bounded, cached reads of hub users for the chat page.
 *
 * The chat hub view used to walk every page of `GET /api/v1/users` on entry
 * and again on every 60 s fallback poll. This module gives it three
 * bounded reads instead, all sharing one page-wide cache of user rows:
 *
 * - {@link loadRecentUsers}: one page of the most recently seen users,
 *   shared by every caller and reused for {@link RECENT_USERS_TTL_MS};
 * - {@link searchUsers}: the hub's server-side `?search=` (email or display
 *   name), for users beyond that page;
 * - {@link resolveUser}: one user by id, fetched at most once per id while
 *   cached (a 404 is remembered for {@link RECENT_USERS_TTL_MS} only, so a
 *   user added later resolves without a reload).
 */

import { apiFetch } from './api.js';
import { SharedJsonLoad } from './chat-list-cache.js';
import { paginateAll, PaginationTruncatedError } from './paginate-all.js';

/** A `/api/v1/users` row, as far as the chat page reads it. */
export interface DirectoryUser {
  id: string;
  displayName: string;
  email?: string;
  avatarUrl?: string;
  role?: string;
  status?: string;
}

/** The bounded recent-users page: its rows, and whether the hub has more. */
export interface RecentUsersPage {
  users: DirectoryUser[];
  truncated: boolean;
}

/** How many recently seen users the hub view lists before search takes over. */
export const RECENT_USERS_LIMIT = 100;

/** How long the recent-users page, and a 404 for an id, are reused. */
export const RECENT_USERS_TTL_MS = 5 * 60 * 1000;

/** Results per server-side search. */
export const USER_SEARCH_LIMIT = 20;

/**
 * Time limit for one by-id lookup, request and body read together. A hung
 * lookup is aborted, so it cannot pin the single-flight entry for its id.
 */
export const RESOLVE_USER_TIMEOUT_MS = 30_000;

/**
 * Time limit for one people search, request and body read together. A hung
 * search resolves empty instead of leaving the sidebar on "Searching…".
 */
export const USER_SEARCH_TIMEOUT_MS = 30_000;

/** Every user row seen by any read, by id. */
const known = new Map<string, DirectoryUser>();
/** Ids the hub answered 404 for, with when that answer expires. */
const missing = new Map<string, number>();
/** In-flight by-id lookups, so concurrent callers share one request. */
const inflight = new Map<string, Promise<DirectoryUser | null>>();

let now: () => number = () => Date.now();

function isUserRow(u: unknown): u is DirectoryUser {
  return !!u && typeof u === 'object' && typeof (u as DirectoryUser).id === 'string';
}

/** Remember user rows so later name lookups need no request. */
export function rememberUsers(rows: readonly DirectoryUser[]): void {
  for (const row of rows) {
    if (!isUserRow(row) || !row.id) continue;
    known.set(row.id, row);
    missing.delete(row.id);
  }
}

/** The cached row for `id`, with no request; undefined when none is cached. */
export function peekUser(id: string): DirectoryUser | undefined {
  return known.get(id);
}

/** The name a user row is shown by. */
export function userDisplayName(u: DirectoryUser): string {
  return u.displayName || u.email || u.id;
}

/** The body of `GET /api/v1/users`, as far as this module reads it. */
interface UsersBody {
  users?: unknown[];
  nextCursor?: string;
}

function parseUsersPage(body: unknown): { items: DirectoryUser[]; nextCursor?: string } {
  const data = body as UsersBody;
  const items = (Array.isArray(data.users) ? data.users : []).filter(isUserRow);
  return { items, ...(data.nextCursor ? { nextCursor: data.nextCursor } : {}) };
}

/**
 * One page through `paginateAll` (`maxPages: 1`), for its per-request
 * timeout and response checks: a hung request fails rather than leaving
 * every later caller joined to it. A hub with more users than the page
 * holds surfaces as `PaginationTruncatedError`, carrying the page.
 */
async function fetchRecentUsers(): Promise<RecentUsersPage | null> {
  let users: DirectoryUser[];
  let truncated = false;
  try {
    users = await paginateAll({
      path: '/api/v1/users?sort=lastSeen&dir=desc',
      pageSize: RECENT_USERS_LIMIT,
      parsePage: parseUsersPage,
      maxPages: 1,
      label: 'recent users',
    });
  } catch (err) {
    if (!(err instanceof PaginationTruncatedError)) return null;
    users = err.items as DirectoryUser[];
    truncated = true;
  }
  rememberUsers(users);
  return { users, truncated };
}

const recentLoad = new SharedJsonLoad<RecentUsersPage>(fetchRecentUsers, () => now());

/**
 * One page of the most recently seen users, shared by every caller and
 * reused while younger than {@link RECENT_USERS_TTL_MS}. Null when the
 * request failed (not cached: the next call retries).
 */
export function loadRecentUsers(): Promise<RecentUsersPage | null> {
  // `maxAgeMs` admits a page exactly that old; reuse only a younger one, so
  // a 60 s poll started with the load reloads at the TTL, not a poll later.
  return recentLoad.load({ maxAgeMs: RECENT_USERS_TTL_MS - 1 });
}

/**
 * Server-side user search (`GET /api/v1/users?search=`), matching email or
 * display name, optionally limited to one `status` (the hub filters by it).
 * Results seed the name cache. An abort by the caller rejects; any other
 * failure, including {@link USER_SEARCH_TIMEOUT_MS} passing, resolves to an
 * empty list.
 */
export async function searchUsers(
  query: string,
  options: { signal?: AbortSignal; limit?: number; status?: string } = {}
): Promise<DirectoryUser[]> {
  const q = query.trim();
  if (!q) return [];
  const limit = options.limit ?? USER_SEARCH_LIMIT;
  const status = options.status ? `&status=${encodeURIComponent(options.status)}` : '';
  // One signal for the request: the caller's abort and the timeout.
  const controller = new AbortController();
  const onCallerAbort = (): void => controller.abort();
  if (options.signal?.aborted) controller.abort();
  options.signal?.addEventListener('abort', onCallerAbort);
  const timer = setTimeout(() => controller.abort(), USER_SEARCH_TIMEOUT_MS);
  try {
    const res = await apiFetch(
      `/api/v1/users?search=${encodeURIComponent(q)}&limit=${limit}${status}`,
      { signal: controller.signal }
    );
    if (!res.ok) return [];
    const body = (await res.json()) as UsersBody | null;
    const users = (Array.isArray(body?.users) ? body.users : []).filter(isUserRow);
    rememberUsers(users);
    return users;
  } catch (err) {
    if (options.signal?.aborted) throw err;
    return [];
  } finally {
    clearTimeout(timer);
    options.signal?.removeEventListener('abort', onCallerAbort);
  }
}

/**
 * One user by id: the cached row, else `GET /api/v1/users/{id}` once
 * (concurrent callers share it). Null when the user is unknown or cannot be
 * read. A 404 is remembered for {@link RECENT_USERS_TTL_MS}; other failures
 * (including a lookup aborted after {@link RESOLVE_USER_TIMEOUT_MS}) are not
 * remembered, so the next call asks again.
 */
export function resolveUser(id: string): Promise<DirectoryUser | null> {
  if (!id) return Promise.resolve(null);
  const cached = known.get(id);
  if (cached) return Promise.resolve(cached);
  const missingUntil = missing.get(id);
  if (missingUntil !== undefined) {
    if (now() < missingUntil) return Promise.resolve(null);
    missing.delete(id);
  }
  const pending = inflight.get(id);
  if (pending) return pending;
  // `settled` covers a lookup that finishes before `p` is assigned (a fetch
  // that throws synchronously): it is then never registered as in flight.
  const entry: { promise: Promise<DirectoryUser | null> | null; settled: boolean } = {
    promise: null,
    settled: false,
  };
  const p = (async (): Promise<DirectoryUser | null> => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), RESOLVE_USER_TIMEOUT_MS);
    try {
      const res = await apiFetch(`/api/v1/users/${encodeURIComponent(id)}`, {
        suppressAccessDeniedToast: true,
        signal: controller.signal,
      });
      if (res.status === 404) {
        missing.set(id, now() + RECENT_USERS_TTL_MS);
        return null;
      }
      if (!res.ok) return null;
      const body = (await res.json()) as unknown;
      if (!isUserRow(body)) return null;
      rememberUsers([body]);
      return body;
    } catch {
      // A network error or the timeout: not remembered, so asked again.
      return null;
    } finally {
      clearTimeout(timer);
      entry.settled = true;
      // Only this lookup's own entry (a reset may have replaced it).
      if (inflight.get(id) === entry.promise) inflight.delete(id);
    }
  })();
  entry.promise = p;
  if (!entry.settled) inflight.set(id, p);
  return p;
}

/** Forget every cached row, 404 and recent page (tests). */
export function resetUserDirectory(): void {
  known.clear();
  missing.clear();
  inflight.clear();
  recentLoad.invalidate();
}

/** Replace the clock (tests). */
export function setUserDirectoryClock(clock: () => number): void {
  now = clock;
}
