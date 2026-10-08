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
 * Hand-off rules for the server-prefetched page payload (__SCION_DATA__).
 *
 * The server prefetches the API response for the requested path, as the
 * session user, while rendering this document. The payload belongs to this
 * one document and is offered to one render only, the first: whatever that
 * render does with it, it is dropped afterwards (takeInitialPageData), so a
 * later client-side navigation never sees it. That single-document,
 * single-render scope is what keeps it from outliving a navigation or a
 * user change: signing in or out loads a new document.
 *
 * On top of that, the first render only gets the payload when:
 * - the payload's path equals the path being rendered (no query string or
 *   other difference);
 * - the payload names a user and the render is for that user. On a cold
 *   load the client's current user is taken from the same payload, so this
 *   is a defence-in-depth check that the payload is a signed-in one, not
 *   the source of user-change safety;
 * - the document is young: time since navigation start is at most
 *   MAX_SSR_PAGE_DATA_AGE_MS. This bounds the payload's age only for a
 *   document fetched from the server, so a document reached by back or
 *   forward navigation (which may come from the HTTP cache, with a fresh
 *   time origin and an old payload) never gets it, and neither does a
 *   document whose navigation type is unknown.
 *
 * Page components still check that the payload is the resource they show
 * (for example the project id) and keep their fetch fallback.
 */

import type { PageData, User } from '../shared/types.js';

/** Oldest prefetched payload a first render may still use, in ms since navigation start. */
export const MAX_SSR_PAGE_DATA_AGE_MS = 15_000;

/** What the hand-off needs to know about the current document's navigation. */
export interface DocumentTiming {
  /** Milliseconds since this document's navigation start. */
  msSinceNavigationStart: number;
  /** The navigation entry's type ('navigate', 'reload', 'back_forward', ...), or null if unknown. */
  navigationType: string | null;
}

/**
 * The prefetched data to hand to the first rendered page, or undefined when
 * the payload does not match the path and user being rendered, or the
 * document is too old or was reached by back or forward navigation.
 */
export function initialPageDataFor(
  ssr: PageData | null,
  path: string,
  user: Pick<User, 'id'> | null | undefined,
  timing: DocumentTiming
): PageData['data'] | undefined {
  if (!ssr || !ssr.data) return undefined;
  const age = timing.msSinceNavigationStart;
  if (!(age >= 0 && age <= MAX_SSR_PAGE_DATA_AGE_MS)) return undefined;
  if (timing.navigationType !== 'navigate' && timing.navigationType !== 'reload') {
    return undefined;
  }
  if (ssr.path !== path) return undefined;
  const ssrUserId = ssr.user?.id;
  if (!ssrUserId || !user?.id || ssrUserId !== user.id) return undefined;
  return ssr.data;
}

/** The current document's timing, failing closed (unknown type) where the API is missing. */
export function currentDocumentTiming(): DocumentTiming {
  let navigationType: string | null = null;
  try {
    const entry = performance.getEntriesByType('navigation')[0] as
      | PerformanceNavigationTiming
      | undefined;
    navigationType = entry?.type ?? null;
  } catch {
    navigationType = null;
  }
  return { msSinceNavigationStart: performance.now(), navigationType };
}

/** The payload not yet offered to a render; set once at startup. */
let pendingPayload: PageData | null = null;

/** Stores this document's prefetched payload for the first render. */
export function setInitialPageData(payload: PageData | null): void {
  pendingPayload = payload;
}

/**
 * Takes the payload for a render and clears it, whether or not it matched,
 * so only the first render can ever receive it.
 */
export function takeInitialPageData(
  path: string,
  user: Pick<User, 'id'> | null | undefined,
  timing: DocumentTiming = currentDocumentTiming()
): PageData['data'] | undefined {
  const payload = pendingPayload;
  pendingPayload = null;
  return initialPageDataFor(payload, path, user, timing);
}
