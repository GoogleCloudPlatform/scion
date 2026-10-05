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
 * Shared loads of the chat space list and DM list.
 *
 * At startup three owners need both lists at once: the tab-title unread
 * counter, the chat page (DM unread dots, DM deep links) and the space rail.
 * Each used to issue its own request, so a cold `/chat` asked the hub for
 * `/chat/spaces` and `/chat/dms` twice each — and those are among the
 * heaviest chat endpoints. Going through this module they share one request.
 *
 * Two ways to ask:
 *
 * - `{ maxAgeMs }` (an initial load): join a request already in flight, or
 *   reuse a result whose request started at most `maxAgeMs` ago.
 * - no options (a refresh after something may have changed — an SSE event,
 *   a reconnect, a mutation): always start a new request. A request already
 *   in flight may have been answered before the change, so joining it would
 *   be wrong. Later initial loads can then join or reuse this one.
 *
 * Results are parsed JSON bodies (a `Response` body can only be read once).
 * A failed load resolves to `null` and is not cached, so the next caller
 * retries.
 */

import { apiFetch } from './api.js';

/**
 * How old a completed startup load may be and still be reused by another
 * owner's initial load. Long enough to cover the gap between the unread
 * counter's request (issued before the router renders) and the chat page's
 * and rail's own (issued after their lazy imports); short enough that a
 * real navigation back to chat later fetches again.
 */
export const CHAT_STARTUP_REUSE_MS = 5_000;

export interface SharedLoadOptions {
  /** Accept an in-flight request, or a result whose request started at most this long ago. */
  maxAgeMs?: number;
}

interface Entry<T> {
  startedAt: number;
  promise: Promise<T | null>;
}

/**
 * One shared, coalesced GET whose parsed body is reused by several owners.
 */
export class SharedJsonLoad<T> {
  private entry: Entry<T> | null = null;

  constructor(
    private readonly fetchOnce: () => Promise<T | null>,
    private readonly now: () => number = () => Date.now()
  ) {}

  load(options: SharedLoadOptions = {}): Promise<T | null> {
    const { maxAgeMs } = options;
    const current = this.entry;
    if (current && maxAgeMs !== undefined && this.now() - current.startedAt <= maxAgeMs) {
      return current.promise;
    }
    return this.start();
  }

  /** Forget the cached result; the next load of any kind fetches. */
  invalidate(): void {
    this.entry = null;
  }

  private start(): Promise<T | null> {
    const entry: Entry<T> = { startedAt: this.now(), promise: Promise.resolve(null) };
    entry.promise = this.fetchOnce()
      .catch(() => null)
      .then((value) => {
        // A failure is not worth sharing: drop it so the next caller retries,
        // unless a newer request has replaced this one in the meantime.
        if (value === null && this.entry === entry) this.entry = null;
        return value;
      });
    this.entry = entry;
    return entry.promise;
  }
}

/** Fetches `path` and returns its JSON body, or null for a non-OK response or a network error. */
async function fetchJson<T>(path: string): Promise<T | null> {
  try {
    const res = await apiFetch(path);
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  }
}

/** The body of `GET /api/v1/chat/spaces`, as far as the shared loader is concerned. */
export interface ChatSpacesBody {
  spaces?: unknown[];
}

/** The body of `GET /api/v1/chat/dms`, as far as the shared loader is concerned. */
export interface ChatDMsBody {
  dms?: unknown[];
}

/** The page-wide shared `/api/v1/chat/spaces` load. */
export const chatSpacesLoad = new SharedJsonLoad<ChatSpacesBody>(() =>
  fetchJson<ChatSpacesBody>('/api/v1/chat/spaces')
);

/** The page-wide shared `/api/v1/chat/dms` load. */
export const chatDMsLoad = new SharedJsonLoad<ChatDMsBody>(() =>
  fetchJson<ChatDMsBody>('/api/v1/chat/dms')
);
