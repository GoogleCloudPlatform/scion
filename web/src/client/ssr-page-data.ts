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
 * session user, while rendering the shell. The client may hand that
 * response to the first page it renders, and only then:
 *
 * - the payload is offered to exactly one render, the first one. Whatever
 *   that render does with it, the caller drops the payload afterwards, so it
 *   can never be reused by a later client-side navigation (stale data);
 * - the payload's path must equal the path being rendered;
 * - the payload must name a user, and that user must be the user the client
 *   is rendering for. A payload without a user, or for a different user, is
 *   never handed over;
 * - the document must be young: the payload was built while the server
 *   answered this document's navigation, so the time since navigation start
 *   bounds its age. A render later than MAX_SSR_PAGE_DATA_AGE_MS after it
 *   (a slow boot, a tab restored much later) fetches instead.
 *
 * Page components still check that the payload is the resource they show
 * (for example the project id) and keep their fetch fallback.
 */

import type { PageData, User } from '../shared/types.js';

/**
 * The prefetched data to hand to the first rendered page, or undefined when
 * the payload does not match the path and user being rendered.
 */
/** Oldest prefetched payload a first render may still use, in ms since navigation start. */
export const MAX_SSR_PAGE_DATA_AGE_MS = 15_000;

export function initialPageDataFor(
  ssr: PageData | null,
  path: string,
  user: Pick<User, 'id'> | null | undefined,
  msSinceNavigationStart: number
): PageData['data'] | undefined {
  if (!ssr || !ssr.data) return undefined;
  if (!(msSinceNavigationStart >= 0 && msSinceNavigationStart <= MAX_SSR_PAGE_DATA_AGE_MS)) {
    return undefined;
  }
  if (ssr.path !== path) return undefined;
  const ssrUserId = ssr.user?.id;
  if (!ssrUserId || !user?.id || ssrUserId !== user.id) return undefined;
  return ssr.data;
}
