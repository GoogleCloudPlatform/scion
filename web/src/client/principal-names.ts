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
 * Display names of artifact owners and publishers (agents and users),
 * looked up once per page load and shared by every component that shows
 * them. A lookup the caller may not make is not an error: the id is shown.
 */

import { apiFetch } from './api.js';

const cache = new Map<string, Promise<string>>();

/** Forgets every looked-up name (tests). */
export function resetPrincipalNames(): void {
  cache.clear();
}

/**
 * The display name of an agent or user: displayName, name or slug, or ''
 * when it cannot be looked up. Each principal is fetched at most once.
 */
export function principalName(kind: string, ref: string): Promise<string> {
  if (!ref || (kind !== 'agent' && kind !== 'user')) return Promise.resolve('');
  const key = `${kind}:${ref}`;
  let p = cache.get(key);
  if (!p) {
    p = (async (): Promise<string> => {
      try {
        const res = await apiFetch(
          `/api/v1/${kind === 'agent' ? 'agents' : 'users'}/${encodeURIComponent(ref)}`,
          { suppressAccessDeniedToast: true }
        );
        if (res.ok) {
          const body = (await res.json()) as { displayName?: string; name?: string; slug?: string };
          return body.displayName || body.name || body.slug || '';
        }
        // A refusal or an absent principal will not change; anything else
        // (a server error) may, so it is looked up again next time.
        if (res.status !== 403 && res.status !== 404) cache.delete(key);
        return '';
      } catch {
        cache.delete(key);
        return '';
      }
    })();
    cache.set(key, p);
  }
  return p;
}

/** A short form of an id, for when no name is known. */
function shortId(ref: string): string {
  return ref.length > 12 ? `${ref.slice(0, 8)}…` : ref;
}

/**
 * How a principal is shown: "You" for the signed-in user, "<name> (agent)"
 * for an agent, the name for a user; the short id while no name is known.
 */
export function principalLabel(kind: string, ref: string, name: string, me?: string): string {
  if (kind === 'user' && me && ref === me) return 'You';
  const shown = name || shortId(ref);
  return kind === 'agent' ? `${shown} (agent)` : shown;
}
