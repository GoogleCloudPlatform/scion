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
 * page count guards against a server bug that never terminates.
 *
 * This mirrors the agents/users pagination contract already implemented by
 * `chat-palette-data.ts`'s `fetchAllPaletteAgents`/`fetchAllPaletteUsers`;
 * it is factored out here so other full-list consumers (for example
 * `chat.ts`'s hub members sidebar) don't hand-roll the same cursor loop.
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
   * false, the walk stops and resolves with whatever it has accumulated so
   * far, instead of throwing or fetching another page — for a caller whose
   * result will be discarded if the thing it was walking for (a view, a
   * connected element) is gone before the walk finishes, so there is no
   * point paying for the remaining pages.
   */
  shouldContinue?: () => boolean;
}

const DEFAULT_MAX_PAGES = 500;

/** Raised when a page request fails, a response body is malformed, or pagination does not terminate within the safety bound. */
export class PaginationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PaginationError';
  }
}

/**
 * Fetch every page of `options.path`, following `nextCursor` until it is
 * empty. Throws {@link PaginationError} on the first page that fails —
 * callers that need to preserve previously loaded data on a failed walk
 * should keep their own copy until this resolves, rather than publishing
 * partial results.
 */
export async function paginateAll<T>(options: PaginateAllOptions<T>): Promise<T[]> {
  const { path, pageSize, parsePage, shouldContinue } = options;
  const maxPages = options.maxPages ?? DEFAULT_MAX_PAGES;
  const label = options.label ?? path;

  const all: T[] = [];
  const seenCursors = new Set<string>();
  let cursor = '';
  let pages = 0;

  do {
    if (shouldContinue && !shouldContinue()) break;
    const separator = path.includes('?') ? '&' : '?';
    const url = cursor
      ? `${path}${separator}limit=${pageSize}&cursor=${encodeURIComponent(cursor)}`
      : `${path}${separator}limit=${pageSize}`;
    const res = await apiFetch(url);
    if (!res.ok) {
      throw new PaginationError(`${label} request failed: ${res.status}`);
    }
    let raw: unknown;
    try {
      raw = await res.json();
    } catch {
      throw new PaginationError(`${label} response was not valid JSON`);
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
