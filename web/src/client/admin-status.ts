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
 * One shared GET /api/v1/auth/admin-status per signed-in user.
 *
 * The app shell (two nav instances) and the startup code all need the
 * current user's admin flags; this module lets them share one in-flight
 * request and its result instead of each sending its own. Rules:
 *
 * - The value and any in-flight request are bound to a user id. Asking for
 *   a different user id, or calling {@link clearAdminStatus} (sign-out,
 *   account teardown), drops both, and a request started for an earlier
 *   user resolves to null for its callers instead of landing late.
 * - Only a successful (2xx) response is kept. A failed or non-2xx response
 *   resolves to null (no admin) and is not cached, so the next caller
 *   retries.
 * - `fresh: true` always sends a new request (the admin route guard, so a
 *   grant or revocation applies as soon as an admin page is entered); its
 *   result replaces the shared value, and an older request still in flight
 *   can no longer write it (its callers get the newest value instead). A
 *   failed fresh request drops the shared value, so nobody keeps showing an
 *   admin result that a recheck could not confirm.
 * - It uses a plain credentialed fetch, as the startup check always did, so
 *   a 401 here never triggers the session-expired login redirect on its own
 *   (other requests on the page still do).
 *
 * This only decides what the UI shows; the server authorizes every admin
 * request itself.
 */

import type { AdminStatus } from '../lib/admin-permissions.js';

interface Inflight {
  userId: string;
  generation: number;
  promise: Promise<AdminStatus | null>;
}

let cached: { userId: string; status: AdminStatus | null } | null = null;
let inflight: Inflight | null = null;
let generation = 0;
/** Bumped by every request; only the newest request may write `cached`. */
let writeSequence = 0;

/** Drops the shared value and abandons any in-flight request. */
export function clearAdminStatus(): void {
  cached = null;
  inflight = null;
  generation++;
}

async function fetchAdminStatus(): Promise<{ ok: boolean; status: AdminStatus | null }> {
  try {
    const res = await fetch('/api/v1/auth/admin-status', { credentials: 'include' });
    if (!res.ok) return { ok: false, status: null };
    const data = (await res.json()) as {
      isAdmin?: unknown;
      isSuperAdmin?: unknown;
      permissions?: unknown;
    };
    return {
      ok: true,
      status: {
        isAdmin: data.isAdmin === true,
        isSuperAdmin: data.isSuperAdmin === true,
        permissions: Array.isArray(data.permissions)
          ? data.permissions.filter((p): p is string => typeof p === 'string')
          : [],
      },
    };
  } catch {
    return { ok: false, status: null };
  }
}

/**
 * The admin status of `userId`: the shared value or in-flight request when
 * there is one for this user, otherwise a new request. With `fresh`, always
 * a new request. Resolves to null without a user id, on any failure, and
 * when the user changed while the request was in flight.
 */
export function loadAdminStatus(
  userId: string | null | undefined,
  options: { fresh?: boolean } = {}
): Promise<AdminStatus | null> {
  if (!userId) return Promise.resolve(null);
  if ((cached && cached.userId !== userId) || (inflight && inflight.userId !== userId)) {
    clearAdminStatus();
  }
  if (!options.fresh) {
    if (cached) return Promise.resolve(cached.status);
    if (inflight) return inflight.promise;
  }

  const myGeneration = generation;
  const mySequence = ++writeSequence;
  const entry: Inflight = {
    userId,
    generation: myGeneration,
    promise: fetchAdminStatus().then(({ ok, status }) => {
      if (inflight === entry) inflight = null;
      // A different user, or a clear, since this request started.
      if (generation !== myGeneration) return null;
      // A newer (fresh) request superseded this one: it alone writes the
      // shared value, and this request's callers get that value.
      if (mySequence !== writeSequence) {
        return cached && cached.userId === userId ? cached.status : null;
      }
      if (ok) cached = { userId, status };
      else if (options.fresh) cached = null;
      return status;
    }),
  };
  inflight = entry;
  return entry.promise;
}
