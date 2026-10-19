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

import { describe, it, expect, vi, afterEach } from 'vitest';
import { FrameBatcher, SortedLogBuffer } from './sorted-log-buffer.js';

interface Row {
  id: string;
  ts: number;
}

/**
 * A row whose `ts` reads are counted, so a test can tell the O(1) append
 * path (reads the new and the last entry only) from a binary search.
 */
function countingRow(id: string, ts: number, counter: { reads: number }): Row {
  return {
    id,
    get ts(): number {
      counter.reads++;
      return ts;
    },
  };
}

function buffer(cap = 100): SortedLogBuffer<Row> {
  return new SortedLogBuffer<Row>({ cap, keyOf: (r) => r.id });
}

function ids(buf: SortedLogBuffer<Row>): string[] {
  return buf.items.map((r) => r.id);
}

describe('SortedLogBuffer', () => {
  it('in-order appends are O(1) (no comparator call)', () => {
    const counter = { reads: 0 };
    const buf = buffer(10_000);
    for (let i = 0; i < 1000; i++) {
      counter.reads = 0;
      expect(buf.insert(countingRow(`r${i}`, 1000 + i, counter))).toBe(true);
      // The new entry's ts and the current last entry's ts; never a search.
      expect(counter.reads).toBeLessThanOrEqual(2);
    }
    // An equal timestamp is still in order (arrival order breaks the tie).
    counter.reads = 0;
    buf.insert(countingRow('tie', 1999, counter));
    expect(counter.reads).toBeLessThanOrEqual(2);
    expect(buf.size).toBe(1001);
    expect(buf.newest?.id).toBe('tie');
  });

  it('out-of-order lines are inserted at the right index', () => {
    const buf = buffer();
    buf.insertMany([
      { id: 'a', ts: 10 },
      { id: 'c', ts: 30 },
      { id: 'e', ts: 50 },
    ]);
    buf.insert({ id: 'd', ts: 40 });
    buf.insert({ id: 'b', ts: 20 });
    buf.insert({ id: 'first', ts: 1 });
    // Equal to an existing ts: goes after it.
    buf.insert({ id: 'c2', ts: 30 });
    expect(ids(buf)).toEqual(['first', 'a', 'b', 'c', 'c2', 'd', 'e']);
    expect(buf.oldest?.id).toBe('first');
    expect(buf.newest?.id).toBe('e');
  });

  it('cap trimming keeps the newest N', () => {
    const buf = buffer(3);
    buf.insertMany([
      { id: 't1', ts: 1 },
      { id: 't2', ts: 2 },
      { id: 't3', ts: 3 },
      { id: 't4', ts: 4 },
    ]);
    expect(ids(buf)).toEqual(['t2', 't3', 't4']);
    // An evicted key can come back only if it is newer than the oldest.
    expect(buf.has('t1')).toBe(false);

    // Older than everything in a full buffer: evicted at once, reported false.
    expect(buf.insert({ id: 'old', ts: 0 })).toBe(false);
    expect(buf.has('old')).toBe(false);
    expect(ids(buf)).toEqual(['t2', 't3', 't4']);

    // Out-of-order but within range: kept, the oldest goes.
    expect(buf.insert({ id: 't2.5', ts: 2.5 })).toBe(true);
    expect(ids(buf)).toEqual(['t2.5', 't3', 't4']);
    expect(buf.size).toBe(3);
  });

  it('ignores duplicate keys', () => {
    const buf = buffer();
    expect(buf.insert({ id: 'x', ts: 5 })).toBe(true);
    expect(buf.insert({ id: 'x', ts: 1 })).toBe(false);
    expect(
      buf.insertMany([
        { id: 'x', ts: 9 },
        { id: 'y', ts: 6 },
      ])
    ).toBe(1);
    expect(ids(buf)).toEqual(['x', 'y']);
    expect(buf.items[0].ts).toBe(5);
  });

  it('orders unparsable (NaN) timestamps first', () => {
    const buf = buffer();
    buf.insertMany([
      { id: 'a', ts: 10 },
      { id: 'nan', ts: Number.NaN },
      { id: 'b', ts: 20 },
    ]);
    expect(ids(buf)).toEqual(['nan', 'a', 'b']);
  });

  it('clear empties the buffer and forgets keys', () => {
    const buf = buffer();
    buf.insert({ id: 'a', ts: 1 });
    buf.clear();
    expect(buf.size).toBe(0);
    expect(buf.newest).toBeUndefined();
    expect(buf.insert({ id: 'a', ts: 1 })).toBe(true);
  });
});

describe('FrameBatcher', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function stubFrames(): FrameRequestCallback[] {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => frames.push(cb));
    vi.stubGlobal('cancelAnimationFrame', (h: number) => {
      frames[h - 1] = (): void => {};
    });
    return frames;
  }

  it('runs flush once per frame however often it is scheduled', () => {
    const frames = stubFrames();
    const flush = vi.fn();
    const batch = new FrameBatcher(flush);
    for (let i = 0; i < 500; i++) batch.schedule();
    expect(frames).toHaveLength(1);
    expect(batch.pending).toBe(true);
    expect(flush).not.toHaveBeenCalled();

    frames[0](0);
    expect(flush).toHaveBeenCalledTimes(1);
    expect(batch.pending).toBe(false);

    batch.schedule();
    expect(frames).toHaveLength(2);
  });

  it('cancel drops a scheduled flush', () => {
    const frames = stubFrames();
    const flush = vi.fn();
    const batch = new FrameBatcher(flush);
    batch.schedule();
    batch.cancel();
    expect(batch.pending).toBe(false);
    frames[0](0);
    expect(flush).not.toHaveBeenCalled();
  });
});
