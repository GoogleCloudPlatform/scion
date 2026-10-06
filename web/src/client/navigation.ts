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
 * Base-path-aware navigation helpers for components.
 *
 * Components must not import `client/main.ts` (its module load boots the
 * app) or call `window.history.pushState` / `replaceState` themselves (raw
 * calls skip the base-path prefix the UI needs when served under a proxy
 * path). They use these helpers instead; an ESLint rule enforces this.
 *
 * `navigateTo` dispatches the `nav-click` event that the router in `main.ts`
 * listens for, so this module has no dependency on the router.
 */

/**
 * Prefix an app path (e.g. `/projects/abc`) with the configured base path
 * for use as a browser URL. Returns the path unchanged when the app is
 * served at `/`.
 */
export function browserPath(path: string): string {
  const base = import.meta.env.BASE_URL;
  return base && base !== '/' ? base.replace(/\/$/, '') + path : path;
}

/**
 * Navigate to an app path (may include a query string) through the
 * client-side router. The router applies the base path, pushes a history
 * entry and renders the route.
 */
export function navigateTo(path: string): void {
  document.dispatchEvent(
    new CustomEvent('nav-click', { detail: { path }, bubbles: true, composed: true })
  );
}

/**
 * Push a history entry for an app path without rendering anything, e.g. to
 * put back the current page's URL after the user cancels leaving it.
 */
export function pushUrl(path: string): void {
  window.history.pushState({}, '', browserPath(path));
}

/**
 * Replace the query string of the current URL without rendering or adding a
 * history entry, keeping the path (already base-prefixed) and the hash. Used
 * by pages that mirror filter state into the URL. An empty value clears the
 * query.
 */
export function replaceSearch(search: URLSearchParams | string): void {
  const qs = (typeof search === 'string' ? search : search.toString()).replace(/^\?/, '');
  const url = `${window.location.pathname}${qs ? `?${qs}` : ''}${window.location.hash}`;
  window.history.replaceState(window.history.state, '', url);
}
