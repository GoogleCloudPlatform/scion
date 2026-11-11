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
 * Bounded, de-duplicated, timestamp-sorted buffer for the log and message
 * viewers (webperf C6, ptone/scion#5310).
 *
 * The viewers used to rebuild and re-sort their whole buffer on every
 * streamed line. This buffer keeps entries in ascending `ts` order:
 *
 * - An entry at or after the newest one (the streaming common case) is
 *   appended in O(1): only the new entry's and the last entry's `ts` are
 *   read.
 * - An older entry is placed by binary search (after any entries with an
 *   equal `ts`, so arrival order breaks ties).
 * - Past `cap`, the oldest entries are trimmed with one `splice`.
 *
 * `ts` must be precomputed by the caller (milliseconds; `NaN` for an
 * unparsable timestamp is treated as the oldest possible value).
 */

export interface SortedLogBufferOptions<T> {
  /** Maximum number of entries kept; the oldest are evicted first. */
  cap: number;
  /** Stable identity used for de-duplication (e.g. a log insertId). */
  keyOf: (entry: T) => string;
}

export class SortedLogBuffer<T extends { ts: number }> {
  private readonly cap: number;
  private readonly keyOf: (entry: T) => string;
  private readonly byKey = new Map<string, T>();
  private readonly list: T[] = [];

  constructor(options: SortedLogBufferOptions<T>) {
    this.cap = options.cap;
    this.keyOf = options.keyOf;
  }

  /**
   * Entries in ascending `ts` order. The array is owned by the buffer and
   * mutated in place; callers must not modify it.
   */
  get items(): readonly T[] {
    return this.list;
  }

  get size(): number {
    return this.list.length;
  }

  /** Oldest entry, or undefined when empty. */
  get oldest(): T | undefined {
    return this.list[0];
  }

  /** Newest entry, or undefined when empty. */
  get newest(): T | undefined {
    return this.list[this.list.length - 1];
  }

  has(key: string): boolean {
    return this.byKey.has(key);
  }

  /**
   * Inserts one entry. Returns true when the entry is buffered afterwards;
   * false when an entry with the same key was already buffered (nothing
   * changes) or when the entry is older than everything in a full buffer
   * (it is evicted at once).
   */
  insert(entry: T): boolean {
    const key = this.keyOf(entry);
    if (this.byKey.has(key)) return false;
    this.byKey.set(key, entry);

    const list = this.list;
    const n = list.length;
    const ts = sortKey(entry.ts);
    if (n === 0 || ts >= sortKey(list[n - 1].ts)) {
      list.push(entry);
    } else {
      list.splice(this.upperBound(ts), 0, entry);
    }

    if (list.length > this.cap) {
      const evicted = list.splice(0, list.length - this.cap);
      for (const e of evicted) this.byKey.delete(this.keyOf(e));
    }
    return this.byKey.has(key);
  }

  /** Inserts several entries; returns how many were new. */
  insertMany(entries: Iterable<T>): number {
    let added = 0;
    for (const entry of entries) {
      if (this.insert(entry)) added++;
    }
    return added;
  }

  clear(): void {
    this.byKey.clear();
    this.list.length = 0;
  }

  /** First index whose `ts` is greater than `ts`. */
  private upperBound(ts: number): number {
    const list = this.list;
    let lo = 0;
    let hi = list.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (sortKey(list[mid].ts) <= ts) {
        lo = mid + 1;
      } else {
        hi = mid;
      }
    }
    return lo;
  }
}

/** Orders an unparsable (NaN) timestamp before every real one. */
function sortKey(ts: number): number {
  return Number.isNaN(ts) ? -Infinity : ts;
}

/**
 * Coalesces render requests to at most one per animation frame. A burst of
 * streamed lines calls `schedule()` once per line; `flush` runs once, on the
 * next frame. Browsers do not run frames in hidden tabs, so a background
 * viewer renders once when it becomes visible again.
 */
export class FrameBatcher {
  private handle: number | null = null;

  constructor(private readonly flush: () => void) {}

  get pending(): boolean {
    return this.handle !== null;
  }

  schedule(): void {
    if (this.handle !== null) return;
    this.handle = requestAnimationFrame(() => {
      this.handle = null;
      this.flush();
    });
  }

  cancel(): void {
    if (this.handle === null) return;
    cancelAnimationFrame(this.handle);
    this.handle = null;
  }
}
