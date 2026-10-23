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

import { hasInPageState, IN_PAGE_STATE_KEY } from './route-history.js';

/**
 * Prefix an app path (e.g. `/projects/abc`) with the configured base path
 * for use as a browser URL. Returns the path unchanged when the app is
 * served at `/`. The inverse of {@link stripBasePath}.
 */
export function browserPath(path: string): string {
  const base = import.meta.env.BASE_URL;
  return base && base !== '/' ? base.replace(/\/$/, '') + path : path;
}

/**
 * Strip the Vite base path prefix from a URL pathname so the client-side
 * router can match application routes when served behind a reverse proxy.
 * Uses import.meta.env.BASE_URL which Vite injects at build/dev time.
 * When base is '/' (no proxy), this is a no-op.
 */
export function stripBasePath(pathname: string): string {
  const base = import.meta.env.BASE_URL;
  if (!base || base === '/') return pathname;

  // Normalize: strip trailing slash from base for comparison
  const baseNoSlash = base.replace(/\/$/, '');

  // Exact match (base path without trailing slash, e.g. /foo)
  if (pathname === baseNoSlash) return '/';

  // Prefix match (e.g. /foo/bar → /bar)
  if (pathname.startsWith(base)) {
    const stripped = pathname.slice(base.length - 1); // keep leading /
    return stripped || '/';
  }

  return pathname;
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
  noteShownEntry();
}

/**
/**
 * Asked before the router leaves the page on screen; returns true to let it
 * leave (e.g. once the user has confirmed), false to stay.
 */
export type LeaveGuard = () => boolean;

const leaveGuards = new Set<LeaveGuard>();

/** The browser URL and history state of the entry on screen (see noteShownEntry). */
let shownEntry: { url: string; state: unknown } | null = null;

/**
 * Record the current browser URL and history state as the entry on screen,
 * for a vetoed Back/Forward to put back. The router calls it after each URL
 * it writes or renders, and so do the URL helpers here and the terminal
 * workspace's own URL updates.
 */
export function noteShownEntry(): void {
  shownEntry = {
    url: `${window.location.pathname}${window.location.search}${window.location.hash}`,
    state: window.history.state,
  };
}

/**
 * Let a page veto leaving it through the router: a link, `navigateTo`, or
 * browser Back/Forward. The router asks before it renders anything, and
 * only for a navigation that would remove the page (not for the terminal
 * workspace, which keeps it), so a veto leaves the page, and its unsaved
 * state, as it is. A full unload is not covered; use `beforeunload` for
 * that. Add the guard on connect and remove it, with the returned
 * function, on disconnect.
 */
export function addLeaveGuard(guard: LeaveGuard): () => void {
  leaveGuards.add(guard);
  return () => {
    leaveGuards.delete(guard);
  };
}

/** For the router: whether every leave guard lets the page go, asking each in turn. */
export function mayLeave(): boolean {
  for (const guard of Array.from(leaveGuards)) {
    if (!guard()) return false;
  }
  return true;
}

/**
 * For the router, on a popstate that would remove the guarded page: the
 * browser has already moved to the other entry. When a guard vetoes, push
 * the entry that was on screen back as a new entry and return true; the
 * router must then not render. (A vetoed Back so drops the forward entries;
 * a vetoed Forward leaves the target entry below the restored one.)
 */
export function vetoPopState(): boolean {
  if (!shownEntry || mayLeave()) return false;
  window.history.pushState(shownEntry.state, '', shownEntry.url);
  return true;
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
  noteShownEntry();
}

/**
 * Move to a fragment of the page on screen as an in-page history entry,
 * without rendering. The entry being left is marked in-page first (its
 * other state kept, `leaving` as its marker) unless it already is, and the
 * new entry carries `state` as its marker (IN_PAGE_STATE_KEY, see
 * route-history.ts). The router leaves Back and Forward between such
 * entries of one path to the page; a plain fragment navigation would
 * instead render the route again. The path (already base-prefixed) and the
 * query are kept. `fragment` starts with "#".
 */
export function pushInPageFragment(
  fragment: string,
  state: Record<string, unknown>,
  leaving: Record<string, unknown>
): void {
  const h = window.history;
  const current: unknown = h.state;
  if (!hasInPageState(current)) {
    const kept: Record<string, unknown> =
      typeof current === 'object' && current !== null ? (current as Record<string, unknown>) : {};
    h.replaceState({ ...kept, [IN_PAGE_STATE_KEY]: leaving }, '');
  }
  h.pushState(
    { [IN_PAGE_STATE_KEY]: state },
    '',
    `${window.location.pathname}${window.location.search}${fragment}`
  );
  noteShownEntry();
}
