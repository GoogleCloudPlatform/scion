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
 * The Starred rail (web.chat_starred_messages): the conversations in which
 * the signed-in user has at least one star, from
 * `GET /api/v1/chat/starred-conversations`.
 *
 * The rows always come from that endpoint, never from the loaded All/Unread
 * rail. The hub sorts and pages them (Recent or Alphabetical); the rail
 * groups them into a DMs section and project headings and sorts within
 * each group with its own comparators. Nothing here is persisted.
 */

import { apiFetch, extractApiError } from './api.js';

/** One row of the Starred rail as the hub returns it. */
export interface StarredConversationRow {
  conversationKey: string;
  conversationKind: 'thread' | 'dm';
  conversationName: string;
  /** UTC instant; absent when the conversation has no recorded activity. */
  lastActivityAt?: string;
  /** Present together for threads and agent DMs; absent for DMs between users. */
  projectId?: string;
  projectName?: string;
}

/** One page of the Starred rail. */
export interface StarredConversationPage {
  items: StarredConversationRow[];
  nextCursor?: string;
}

/** The hub's Starred rail orders. */
export type StarredRailSort = 'activity' | 'alpha';

/** The page size of the Starred rail. */
export const STARRED_RAIL_PAGE = 30;

/** The largest page the hub serves; refreshes reload loaded rows in pages of this size. */
const STARRED_RAIL_MAX_PAGE = 100;

/** A refresh reloads at most this many pages (it never walks an unbounded list). */
const STARRED_RAIL_MAX_REFRESH_PAGES = 10;

/** While the Starred rail is visible it reconciles at most this often. */
export const STARRED_RAIL_RECONCILE_MS = 60_000;

/** Thrown by fetchStarredConversations with the hub's message and status. */
export class StarredRailError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'StarredRailError';
  }
}

/** One page of the Starred rail. */
export async function fetchStarredConversations(
  sort: StarredRailSort,
  limit: number,
  cursor?: string,
  signal?: AbortSignal
): Promise<StarredConversationPage> {
  const q = new URLSearchParams({ sort, limit: String(limit) });
  if (cursor) q.set('cursor', cursor);
  const res = await apiFetch(
    `/api/v1/chat/starred-conversations?${q.toString()}`,
    signal ? { signal } : undefined
  );
  if (!res.ok) {
    throw new StarredRailError(
      await extractApiError(res, 'Could not load starred conversations'),
      res.status
    );
  }
  const body = (await res.json()) as StarredConversationPage;
  const page: StarredConversationPage = { items: body.items ?? [] };
  if (body.nextCursor) page.nextCursor = body.nextCursor;
  return page;
}

/**
 * The Starred rail's order for the shared rail sort preference: Recent and
 * Alphabetical map to themselves; Custom (an All/Unread arrangement) shows
 * Recent, without changing the preference.
 */
export function starredRailSort(spaceSortMode: string | undefined): StarredRailSort {
  return spaceSortMode === 'alpha' ? 'alpha' : 'activity';
}

/** `rows` with `incoming` merged in by conversation key (incoming wins, no duplicates). */
export function mergeStarredRows(
  rows: readonly StarredConversationRow[],
  incoming: readonly StarredConversationRow[]
): StarredConversationRow[] {
  const byKey = new Map<string, StarredConversationRow>();
  for (const r of rows) byKey.set(r.conversationKey, r);
  for (const r of incoming) byKey.set(r.conversationKey, r);
  return [...byKey.values()];
}

/** A project heading of the Starred rail and its thread rows. */
export interface StarredRailProject {
  projectId: string;
  projectName: string;
  rows: StarredConversationRow[];
}

/** The Starred rail's sections: DMs (agent and human), then project headings. */
export interface StarredRailGroups {
  dms: StarredConversationRow[];
  projects: StarredRailProject[];
}

/** A row's activity as epoch milliseconds (0 when none). */
export function starredRowTime(row: { lastActivityAt?: string }): number {
  const t = row.lastActivityAt ? new Date(row.lastActivityAt).getTime() : 0;
  return Number.isNaN(t) ? 0 : t;
}

/** The comparators the rail uses everywhere (name, then id; newest, then id). */
export interface StarredRailComparators {
  byName(aName: string, bName: string, aId: string, bId: string): number;
  byActivity(aTime: number, bTime: number, aId: string, bId: string): number;
}

/**
 * Group rows into the DMs section and project headings (threads only; an
 * agent DM is listed with the DMs), sorted under `sort` with the rail's
 * comparators: rows within a group, and the headings by name or by their
 * newest row. Empty headings never appear.
 */
export function groupStarredRows(
  rows: readonly StarredConversationRow[],
  sort: StarredRailSort,
  cmp: StarredRailComparators
): StarredRailGroups {
  const rowCmp = (a: StarredConversationRow, b: StarredConversationRow): number =>
    sort === 'alpha'
      ? cmp.byName(a.conversationName, b.conversationName, a.conversationKey, b.conversationKey)
      : cmp.byActivity(starredRowTime(a), starredRowTime(b), a.conversationKey, b.conversationKey);

  const dms: StarredConversationRow[] = [];
  const projects = new Map<string, StarredRailProject>();
  for (const row of rows) {
    if (row.conversationKind === 'dm' || !row.projectId) {
      dms.push(row);
      continue;
    }
    let p = projects.get(row.projectId);
    if (!p) {
      p = { projectId: row.projectId, projectName: row.projectName ?? '', rows: [] };
      projects.set(row.projectId, p);
    }
    p.rows.push(row);
  }
  dms.sort(rowCmp);
  for (const p of projects.values()) p.rows.sort(rowCmp);

  const newest = (p: StarredRailProject): number =>
    p.rows.reduce((m, r) => Math.max(m, starredRowTime(r)), 0);
  const ordered = [...projects.values()].sort((a, b) =>
    sort === 'alpha'
      ? cmp.byName(a.projectName, b.projectName, a.projectId, b.projectId)
      : cmp.byActivity(newest(a), newest(b), a.projectId, b.projectId)
  );
  return { dms, projects: ordered };
}

/** What the Starred rail shows. */
export type StarredRailStatus = 'idle' | 'loading' | 'ready' | 'error';

/** The API the controller uses; tests replace it. */
export type StarredRailFetch = typeof fetchStarredConversations;

/**
 * The Starred rail's rows and paging. REST is authoritative: every change
 * signal (a stars invalidation, a local star change, a topic or activity
 * event, reconnect, focus, refresh) is a refetch, never a local edit.
 *
 * - `start(sort)` loads the first page (a sort change restarts).
 * - `loadMore()` appends the next page, merged by key (explicit Load more).
 * - `refresh()` reloads the rows loaded so far (at least one page) and
 *   replaces them, so a conversation that lost its last star, or became
 *   unreadable, leaves; refreshes that arrive while one runs coalesce into
 *   one more afterwards.
 * - A generation counter and an abort controller drop answers for an older
 *   sort, a stopped rail or a superseded request.
 *
 * Events: `change` whenever rows or status change.
 */
export class StarredRailController extends EventTarget {
  rows: StarredConversationRow[] = [];
  nextCursor: string | undefined;
  status: StarredRailStatus = 'idle';
  /** Load more: 'idle', 'loading', or 'error' (retryable, rows kept). */
  moreStatus: 'idle' | 'loading' | 'error' = 'idle';
  error = '';
  sort: StarredRailSort = 'activity';
  /**
   * The hub answered 404: the starred-messages experiment is off there. The
   * rail leaves the Starred filter (and stops polling) when it sees this.
   */
  unavailable = false;

  private generation = 0;
  private abort: AbortController | null = null;
  private refreshing = false;
  private refreshAgain = false;
  private active = false;

  constructor(private readonly fetchPage: StarredRailFetch = fetchStarredConversations) {
    super();
  }

  /** Whether the controller is showing (between start and stop). */
  get isActive(): boolean {
    return this.active;
  }

  /** Load the first page under `sort`; a new sort or a stopped rail restarts. */
  async start(sort: StarredRailSort): Promise<void> {
    if (this.active && sort === this.sort && this.status !== 'idle') return;
    this.active = true;
    this.sort = sort;
    this.resetRows();
    // A restart (a new sort) shows loading, never the empty collection,
    // until its first page arrives.
    this.status = 'loading';
    this.unavailable = false;
    this.emit();
    await this.reload();
  }

  /** Stop: drop requests and rows (the rail left Starred, sign-out, flag off). */
  stop(): void {
    this.active = false;
    this.resetRows();
    this.status = 'idle';
    this.emit();
  }

  /** Reload what is shown (coalesced). */
  async refresh(): Promise<void> {
    if (!this.active) return;
    await this.reload();
  }

  /** Append the next page. */
  async loadMore(): Promise<void> {
    const cursor = this.nextCursor;
    if (!this.active || !cursor || this.moreStatus === 'loading' || this.refreshing) return;
    const generation = this.generation;
    const signal = this.abort?.signal;
    this.moreStatus = 'loading';
    this.emit();
    try {
      const page = await this.fetchPage(this.sort, STARRED_RAIL_PAGE, cursor, signal);
      if (generation !== this.generation) return;
      this.rows = mergeStarredRows(this.rows, page.items);
      this.nextCursor = page.nextCursor;
      this.moreStatus = 'idle';
    } catch (err) {
      if (generation !== this.generation) return;
      if (err instanceof StarredRailError && err.status === 404) this.unavailable = true;
      this.moreStatus = 'error';
    }
    this.emit();
  }

  private resetRows(): void {
    this.generation++;
    this.abort?.abort();
    this.abort = null;
    this.rows = [];
    this.nextCursor = undefined;
    this.moreStatus = 'idle';
    this.error = '';
    this.refreshAgain = false;
  }

  private async reload(): Promise<void> {
    if (this.refreshing) {
      this.refreshAgain = true;
      return;
    }
    this.refreshing = true;
    try {
      do {
        this.refreshAgain = false;
        await this.reloadOnce();
      } while (this.refreshAgain && this.active);
    } finally {
      this.refreshing = false;
    }
  }

  /**
   * Fetch as many rows as are loaded now (at least one page), in pages of
   * up to STARRED_RAIL_MAX_PAGE, and replace the rows. A Load more in flight
   * is superseded. A failure keeps the rows shown and sets `error`.
   */
  private async reloadOnce(): Promise<void> {
    this.generation++;
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    const generation = this.generation;
    const want = Math.max(STARRED_RAIL_PAGE, this.rows.length);
    const first = this.status !== 'ready';
    if (first) this.status = 'loading';
    this.moreStatus = 'idle';
    this.emit();
    try {
      let rows: StarredConversationRow[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < STARRED_RAIL_MAX_REFRESH_PAGES; i++) {
        const limit = Math.min(STARRED_RAIL_MAX_PAGE, Math.max(1, want - rows.length));
        const page = await this.fetchPage(this.sort, limit, cursor, abort.signal);
        if (generation !== this.generation) return;
        rows = mergeStarredRows(rows, page.items);
        cursor = page.nextCursor;
        // A short page with a cursor (hidden candidates spent the hub's scan
        // budget) is continued only on an explicit Load more.
        if (!cursor || rows.length >= want || page.items.length < limit) break;
      }
      this.rows = rows;
      this.nextCursor = cursor;
      this.status = 'ready';
      this.error = '';
    } catch (err) {
      if (generation !== this.generation || abort.signal.aborted) return;
      // Rows already shown stay; the error is reported next to them.
      if (err instanceof StarredRailError && err.status === 404) this.unavailable = true;
      this.error = err instanceof Error ? err.message : 'Could not load starred conversations';
      this.status = this.rows.length > 0 ? 'ready' : 'error';
    }
    this.emit();
  }

  private emit(): void {
    this.dispatchEvent(new Event('change'));
  }
}
