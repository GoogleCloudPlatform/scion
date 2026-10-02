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
 * Generic cursor-pagination walker for `nextCursor`-paginated list endpoints.
 *
 * Fetches every page of a list endpoint via `apiFetch`, following
 * `nextCursor` until it is empty — not until a page's `items` are empty,
 * since a filtered intermediate page can legitimately return zero items
 * while still carrying a cursor. A cursor value repeating across pages is
 * treated as a load error rather than looping forever, and a safety bound on
 * page count guards against a server bug that never terminates. Each page
 * request, including reading its body, is bounded by a timeout: when it
 * expires the request is aborted through its signal and the walk rejects,
 * instead of a request that never settles leaving it pending forever.
 *
 * This mirrors the agents/users pagination contract already implemented by
 * `chat-palette-data.ts`'s `fetchAllPaletteAgents`/`fetchAllPaletteUsers`;
 * it is factored out here so other full-list consumers (for example
 * `chat.ts`'s hub members sidebar) don't hand-roll the same cursor loop.
 *
 * The existing generic helper `apiFetchAllPages` (`api.ts`) is not reused
 * because it returns a partial list when a later page fails, silently stops
 * after 50 pages, and offers no way to stop the walk early.
 */

import { apiFetch } from './api.js';

/** One parsed page: its items, plus the cursor for the next page (absent/empty on the last page). */
export interface ParsedPage<T> {
  items: T[];
  nextCursor?: string;
}

export interface PaginateAllOptions<T> {
  /** The endpoint path, e.g. `/api/v1/agents`. Must not already include a `limit=`/`cursor=` query param. */
  path: string;
  /** Page size requested via `limit=`. */
  pageSize: number;
  /** Extract this page's items and next cursor from the parsed JSON response body. */
  parsePage: (body: unknown) => ParsedPage<T>;
  /** Safety bound on the number of pages followed. Defaults to 500 — well above any realistic hub size. */
  maxPages?: number;
  /** A human-readable name for this list, used only in thrown error messages. Defaults to `path`. */
  label?: string;
  /**
   * Checked before every page fetch, including the first. Once it returns
   * false, the walk stops fetching further pages and rejects with
   * {@link PaginationStoppedError} (carrying whatever it had accumulated so
   * far) rather than resolving — for a caller whose result will be discarded
   * if the thing it was walking for (a view, a connected element) is gone
   * before the walk finishes, so there is no point paying for the remaining
   * pages. Rejecting rather than resolving with a partial list means a
   * caller that only publishes on success (for example via
   * `Promise.allSettled` and acting solely on `'fulfilled'` results) can't
   * mistake a stopped walk's partial result for a complete one.
   */
  shouldContinue?: () => boolean;
  /**
   * Time limit, in milliseconds, for each page: the request and the read of
   * its response body together. A page that has not finished by then is
   * aborted through the request's signal, and the walk rejects with
   * {@link PaginationError}. Defaults to 60000 — well above the slowest
   * list endpoint's measured response time.
   */
  pageTimeoutMs?: number;
}

const DEFAULT_MAX_PAGES = 500;
const DEFAULT_PAGE_TIMEOUT_MS = 60_000;

/** Raised when a page request fails or times out, a response body is malformed, or pagination does not terminate within the safety bound. */
export class PaginationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PaginationError';
  }
}

/**
 * Raised when `shouldContinue` returns false before the walk has followed
 * every page's `nextCursor` to its end — deliberately a sibling of
 * {@link PaginationError}, not a subclass, so callers that distinguish the
 * two with `instanceof` (for example to decide whether a stopped walk should
 * be retried, where a genuine request failure should not be) don't have to
 * also exclude this case by hand. Carries whatever items were accumulated
 * before the stop, for a caller that wants them anyway, but the walk itself
 * is not considered to have completed — the caller must not treat `items` as
 * the full list.
 */
export class PaginationStoppedError<T = unknown> extends Error {
  readonly items: T[];

  constructor(items: T[]) {
    super('pagination stopped before completion');
    this.name = 'PaginationStoppedError';
    this.items = items;
  }
}

function pageTimeoutError(label: string, timeoutMs: number): PaginationError {
  return new PaginationError(`${label} page request timed out after ${timeoutMs}ms`);
}

/**
 * Fetch every page of `options.path`, following `nextCursor` until it is
 * empty. Throws {@link PaginationError} on the first page that fails,
 * including a page whose request and body read do not finish within
 * `pageTimeoutMs` (that page's request is aborted through its signal) —
 * callers that need to preserve previously loaded data on a failed walk
 * should keep their own copy until this resolves, rather than publishing
 * partial results. Throws {@link PaginationStoppedError} if `shouldContinue`
 * returns false before the walk reaches its last page — this is distinct
 * from a request failure, and is never resolved as a (possibly partial)
 * success, so a caller can't mistake a stopped walk's incomplete result for
 * a complete one.
 */
export async function paginateAll<T>(options: PaginateAllOptions<T>): Promise<T[]> {
  const { path, pageSize, parsePage, shouldContinue } = options;
  const maxPages = options.maxPages ?? DEFAULT_MAX_PAGES;
  const label = options.label ?? path;
  const pageTimeoutMs = options.pageTimeoutMs ?? DEFAULT_PAGE_TIMEOUT_MS;

  const all: T[] = [];
  const seenCursors = new Set<string>();
  let cursor = '';
  let pages = 0;

  do {
    if (shouldContinue && !shouldContinue()) throw new PaginationStoppedError<T>(all);
    const separator = path.includes('?') ? '&' : '?';
    const url = cursor
      ? `${path}${separator}limit=${pageSize}&cursor=${encodeURIComponent(cursor)}`
      : `${path}${separator}limit=${pageSize}`;
    // Each page gets its own abort signal and timer, covering both the
    // request and the body read: aborting a fetch rejects the pending
    // request or body read with an AbortError, which is reported here as a
    // timeout. The timer is cleared once the page has settled either way.
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), pageTimeoutMs);
    let raw: unknown;
    try {
      let res: Response;
      try {
        res = await apiFetch(url, { signal: controller.signal });
      } catch (err) {
        if (controller.signal.aborted) throw pageTimeoutError(label, pageTimeoutMs);
        throw err;
      }
      if (!res.ok) {
        throw new PaginationError(`${label} request failed: ${res.status}`);
      }
      try {
        raw = await res.json();
      } catch {
        if (controller.signal.aborted) throw pageTimeoutError(label, pageTimeoutMs);
        throw new PaginationError(`${label} response was not valid JSON`);
      }
    } finally {
      clearTimeout(timer);
    }
    // A JSON body can be any of null, an array, or a primitive (string,
    // number, boolean) and still parse successfully — none of those are a
    // valid list page, and handing one to `parsePage` would either silently
    // read as "zero items" or throw a confusing TypeError deep inside the
    // caller's extractor. Reject anything that isn't a plain object here,
    // in one place.
    if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
      throw new PaginationError(`${label} response body was not an object`);
    }
    const page = parsePage(raw);
    all.push(...page.items);
    const next = page.nextCursor ?? '';
    if (next) {
      if (seenCursors.has(next)) {
        throw new PaginationError(`${label} returned a repeated pagination cursor`);
      }
      seenCursors.add(next);
    }
    cursor = next;
    pages++;
  } while (cursor && pages < maxPages);

  if (cursor && pages >= maxPages) {
    throw new PaginationError(`${label} did not terminate within the page safety bound`);
  }

  return all;
}
