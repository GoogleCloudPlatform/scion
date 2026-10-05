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

import { describe, it, expect, afterEach } from 'vitest';
import {
  findTopVisibleRow,
  rememberChatScrollAnchor,
  scrollTopForAnchor,
  takeChatScrollAnchor,
  type AnchorRow,
  type ChatScrollAnchor,
} from './chat-scroll-anchor.js';

/** Ten 100px rows laid out from y=0, seen with the list scrolled by `scrollTop`. */
function rows(scrollTop: number): {
  count: number;
  rowAt: (i: number) => AnchorRow;
  reads: number[];
} {
  const reads: number[] = [];
  return {
    count: 10,
    reads,
    rowAt: (i) => {
      reads.push(i);
      return { id: `m${i}`, top: i * 100 - scrollTop, bottom: (i + 1) * 100 - scrollTop };
    },
  };
}

describe('findTopVisibleRow', () => {
  it('picks the first row reaching below the top edge', () => {
    const r = rows(250);
    expect(findTopVisibleRow(r.count, r.rowAt, 0)).toMatchObject({ id: 'm2', top: -50 });
  });

  it('skips a row whose bottom sits exactly on the top edge', () => {
    const r = rows(300);
    expect(findTopVisibleRow(r.count, r.rowAt, 0)?.id).toBe('m3');
  });

  it('respects a scroller that does not start at y=0', () => {
    const r = rows(0);
    expect(findTopVisibleRow(r.count, r.rowAt, 120)?.id).toBe('m1');
  });

  it('returns null when there are no rows, or all are above the edge', () => {
    expect(findTopVisibleRow(0, () => ({ id: '', top: 0, bottom: 0 }), 0)).toBeNull();
    const r = rows(5000);
    expect(findTopVisibleRow(r.count, r.rowAt, 0)).toBeNull();
  });

  it('reads only a logarithmic number of rows', () => {
    const r = rows(650);
    findTopVisibleRow(r.count, r.rowAt, 0);
    expect(r.reads.length).toBeLessThanOrEqual(4);
  });
});

describe('scrollTopForAnchor', () => {
  it('moves the row back to its recorded offset', () => {
    // Row is 380px below the top edge now; it was 20px above it.
    expect(scrollTopForAnchor(1000, 380, 0, -20)).toBe(1400);
    expect(scrollTopForAnchor(1000, 180, 100, 80)).toBe(1000);
  });
});

describe('chat scroll anchor hand-over', () => {
  const anchor: ChatScrollAnchor = {
    conversationKey: 'topic-1',
    pinnedToBottom: false,
    messageId: 'm4',
    offset: -12,
  };

  afterEach(() => {
    takeChatScrollAnchor();
  });

  it('is taken once', () => {
    rememberChatScrollAnchor(anchor);
    expect(takeChatScrollAnchor()).toEqual(anchor);
    expect(takeChatScrollAnchor()).toBeNull();
  });

  it('stores a copy, and a later page replaces or clears it', () => {
    const mutable = { ...anchor };
    rememberChatScrollAnchor(mutable);
    mutable.offset = 999;
    expect(takeChatScrollAnchor()?.offset).toBe(-12);

    rememberChatScrollAnchor(anchor);
    rememberChatScrollAnchor(null);
    expect(takeChatScrollAnchor()).toBeNull();
  });
});
