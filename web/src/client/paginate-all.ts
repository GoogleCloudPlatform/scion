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

import { apiErrorMessageFromBody, apiFetch } from './api.js';
import type { ApiFetchOptions } from './api.js';

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
  /**
   * Aborts the whole walk: the page in flight is aborted through its own
   * signal, no further page is requested, and the walk rejects with an
   * `AbortError` `DOMException`.
   */
  signal?: AbortSignal;
  /**
   * Called after each page is parsed, with that page's items and every item
   * accumulated so far (including this page's), before the next page is
   * requested.
   */
  onPage?: (pageItems: readonly T[], all: readonly T[]) => void;
  /** Issues each page request. Defaults to `apiFetch`. */
  fetch?: (path: string, options: ApiFetchOptions) => Promise<Response>;
}

const DEFAULT_MAX_PAGES = 500;
const DEFAULT_PAGE_TIMEOUT_MS = 60_000;

/** Extra context carried by a {@link PaginationError} raised for a non-2xx page response. */
export interface PaginationErrorDetails {
  /** HTTP status of the failed page response. */
  status?: number;
  /** The failed response's body: parsed JSON when it was JSON, else its text capped at 500 characters (absent if unreadable or empty). */
  body?: unknown;
  /** The hub's human-readable error message from a JSON body, when it had one, capped at 500 characters. */
  hubMessage?: string;
}

/** Raised when a page request fails or times out, a response body is malformed, or pagination does not terminate within the safety bound. */
export class PaginationError extends Error {
  /** HTTP status, set when a page response was not ok. */
  readonly status?: number;
  /** The failed page response's body, set when a page response was not ok and its body was readable. */
  readonly body?: unknown;
  /** The hub's error message from the failed response body, when it had one. */
  readonly hubMessage?: string;

  constructor(message: string, details: PaginationErrorDetails = {}) {
    super(message);
    this.name = 'PaginationError';
    if (details.status !== undefined) this.status = details.status;
    if (details.body !== undefined) this.body = details.body;
    if (details.hubMessage !== undefined) this.hubMessage = details.hubMessage;
  }
}

/** Cap on the hub message and text body kept from a failed response, so a large error page is not carried around or shown whole. */
const MAX_ERROR_TEXT = 500;

function capErrorText(text: string): string {
  if (text.length <= MAX_ERROR_TEXT) return text;
  // Do not split a surrogate pair (e.g. an emoji) at the cut.
  const code = text.charCodeAt(MAX_ERROR_TEXT - 1);
  const end = code >= 0xd800 && code <= 0xdbff ? MAX_ERROR_TEXT - 1 : MAX_ERROR_TEXT;
  return `${text.slice(0, end)}…`;
}

/**
 * Read a failed response's body for error reporting: parsed JSON if it is
 * JSON, else its text. Text (including a JSON string body) is capped at
 * {@link MAX_ERROR_TEXT} characters.
 */
async function readErrorBody(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!text) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(text) as unknown;
  } catch {
    return capErrorText(text);
  }
  // A JSON string body is text too; cap it the same way.
  return typeof parsed === 'string' ? capErrorText(parsed) : parsed;
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

/**
 * Raised when the walk reaches `maxPages` with a cursor still pending. A
 * subclass of {@link PaginationError}, so callers that treat any pagination
 * failure alike need no change, but it carries every item accumulated up to
 * the bound for a caller that can show a truncated list.
 */
export class PaginationTruncatedError<T = unknown> extends PaginationError {
  readonly items: T[];

  constructor(label: string, items: T[]) {
    super(`${label} did not terminate within the page safety bound`);
    this.name = 'PaginationTruncatedError';
    this.items = items;
  }
}

function walkAbortedError(): DOMException {
  return new DOMException('pagination aborted', 'AbortError');
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
  const { path, pageSize, parsePage, shouldContinue, signal, onPage } = options;
  const fetchPage = options.fetch ?? apiFetch;
  const maxPages = options.maxPages ?? DEFAULT_MAX_PAGES;
  const label = options.label ?? path;
  const pageTimeoutMs = options.pageTimeoutMs ?? DEFAULT_PAGE_TIMEOUT_MS;

  const all: T[] = [];
  const seenCursors = new Set<string>();
  let cursor = '';
  let pages = 0;

  do {
    if (signal?.aborted) throw walkAbortedError();
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
    const onWalkAbort = (): void => controller.abort();
    signal?.addEventListener('abort', onWalkAbort);
    let raw: unknown;
    try {
      let res: Response;
      try {
        res = await fetchPage(url, { signal: controller.signal });
      } catch (err) {
        if (signal?.aborted) throw walkAbortedError();
        if (controller.signal.aborted) throw pageTimeoutError(label, pageTimeoutMs);
        throw err;
      }
      if (!res.ok) {
        // Keep the hub's explanation (e.g. why a 403 was refused) instead of
        // reporting only the status. The body read is still bounded by this
        // page's timeout; if it cannot be read, report the status alone.
        let body: unknown;
        try {
          body = await readErrorBody(res);
        } catch {
          if (signal?.aborted) throw walkAbortedError();
        }
        const details: PaginationErrorDetails = { status: res.status };
        if (body !== undefined) details.body = body;
        // Only a JSON error body carries a hub message; a non-JSON body
        // (e.g. a proxy's HTML error page) is kept as text but not shown.
        const message = typeof body === 'object' ? apiErrorMessageFromBody(body) : undefined;
        const hubMessage = message ? capErrorText(message) : undefined;
        if (hubMessage !== undefined) details.hubMessage = hubMessage;
        throw new PaginationError(`${label} request failed: ${res.status}`, details);
      }
      try {
        raw = await res.json();
      } catch {
        if (signal?.aborted) throw walkAbortedError();
        if (controller.signal.aborted) throw pageTimeoutError(label, pageTimeoutMs);
        throw new PaginationError(`${label} response was not valid JSON`);
      }
    } finally {
      clearTimeout(timer);
      signal?.removeEventListener('abort', onWalkAbort);
    }
    if (signal?.aborted) throw walkAbortedError();
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
    onPage?.(page.items, all);
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
    throw new PaginationTruncatedError<T>(label, all);
  }

  return all;
}
