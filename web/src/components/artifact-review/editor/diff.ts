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
 * A character diff (Myers, O(ND)) and the three-way patch that carries an
 * edit made in the graphical view back onto the original Markdown.
 *
 * The serializer writes Markdown in its own style (list bullets, table
 * padding, backslashes, fences), so the text it writes for an edited block
 * differs from the block's source in more places than the edit. But it
 * writes the block before the edit (base) in the same style as after it
 * (now), so diff(base, now) is the edit alone: usually the mark tokens and
 * their text. rebase applies that diff to the original source, with base
 * positions mapped to the source through diff(source, base).
 */

/** A replacement of from..to (in the old text) by insert. */
export interface Edit {
  from: number;
  to: number;
  insert: string;
}

/**
 * The most differences diff works through before giving up (the caller
 * then falls back). An edit adds a few dozen characters, and the
 * difference between a unit's source and the serializer's text of it is a
 * few characters per normalized spot; list items and table rows are units
 * of their own, so a block rarely comes near this. The walk back keeps
 * about D * D 32-bit numbers, so the cap bounds that at about 4 MB.
 */
export const MAX_DIFF = 1000;

/**
 * The edits turning a into b, in order and not overlapping, or null when
 * they number more than maxD characters.
 */
export function diff(a: string, b: string, maxD = MAX_DIFF): Edit[] | null {
  // Most edits are local: take off the common prefix and suffix first.
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (
    suf < a.length - pre &&
    suf < b.length - pre &&
    a[a.length - 1 - suf] === b[b.length - 1 - suf]
  ) {
    suf++;
  }
  const A = a.slice(pre, a.length - suf);
  const B = b.slice(pre, b.length - suf);
  const n = A.length;
  const m = B.length;
  if (n === 0 && m === 0) return [];
  if (n === 0 || m === 0) return [{ from: pre, to: pre + n, insert: B }];
  // The difference is at least the difference in length.
  if (Math.abs(n - m) > maxD) return null;
  const max = Math.min(n + m, maxD);
  // v[k] for diagonals -d..d, kept per d for the walk back (O(D^2) memory).
  const trace: Int32Array[] = [];
  let v = new Int32Array(3);
  let end = -1;
  for (let d = 0; d <= max; d++) {
    const next = new Int32Array(2 * d + 3);
    // get(k) of the previous round's v, which holds diagonals -(d-1)..(d-1).
    const get = (k: number): number => {
      const i = k + d; // index in v of size 2(d-1)+3 = 2d+1, offset d.
      return i >= 0 && i < v.length ? v[i] : -1;
    };
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && get(k - 1) < get(k + 1)) ? get(k + 1) : get(k - 1) + 1;
      if (x < 0) x = 0;
      let y = x - k;
      while (x < n && y < m && A[x] === B[y]) {
        x++;
        y++;
      }
      next[k + d + 1] = x;
      if (x >= n && y >= m) {
        end = d;
        break;
      }
    }
    trace.push(next);
    v = next;
    if (end >= 0) break;
  }
  if (end < 0) return null;
  // Walk back from (n, m) to (0, 0), collecting single-character steps.
  const at = (d: number, k: number): number => {
    const t = trace[d];
    const i = k + d + 1;
    return i >= 0 && i < t.length ? t[i] : -1;
  };
  type Op = { t: '=' | '-' | '+'; ai: number; bi: number };
  const ops: Op[] = [];
  let x = n;
  let y = m;
  for (let d = end; d > 0; d--) {
    const k = x - y;
    const prevK = k === -d || (k !== d && at(d - 1, k - 1) < at(d - 1, k + 1)) ? k + 1 : k - 1;
    const prevX = at(d - 1, prevK);
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      x--;
      y--;
      ops.push({ t: '=', ai: x, bi: y });
    }
    if (x === prevX) {
      y--;
      ops.push({ t: '+', ai: x, bi: y });
    } else {
      x--;
      ops.push({ t: '-', ai: x, bi: y });
    }
  }
  ops.reverse();
  const edits: Edit[] = [];
  let cur: Edit | null = null;
  for (const op of ops) {
    if (op.t === '=') {
      if (cur) edits.push(cur);
      cur = null;
      continue;
    }
    cur ??= { from: pre + op.ai, to: pre + op.ai, insert: '' };
    if (op.t === '-') cur.to = pre + op.ai + 1;
    else cur.insert += B[op.bi];
  }
  if (cur) edits.push(cur);
  return edits;
}

/** Applies edits (in order, not overlapping) to text. */
export function apply(text: string, edits: readonly Edit[]): string {
  let out = '';
  let pos = 0;
  for (const e of edits) {
    out += text.slice(pos, e.from) + e.insert;
    pos = e.to;
  }
  return out + text.slice(pos);
}

/** Where the regions that diff(a, b) changed lie in b, with their range in a. */
interface Region {
  bFrom: number;
  bTo: number;
  aFrom: number;
  aTo: number;
}

function regions(edits: readonly Edit[]): Region[] {
  let delta = 0;
  return edits.map((e) => {
    const bFrom = e.from + delta;
    delta += e.insert.length - (e.to - e.from);
    return { bFrom, bTo: bFrom + e.insert.length, aFrom: e.from, aTo: e.to };
  });
}

/**
 * Maps position p of b to a, given the regions of diff(a, b). side tells
 * which end of a region an exact boundary of it (or an empty region at p,
 * text a has and b dropped) maps to. Returns null when p lies strictly
 * inside a region b wrote differently, where there is no exact place in a.
 */
function mapBack(rs: readonly Region[], p: number, side: 'before' | 'after'): number | null {
  let delta = 0;
  for (const r of rs) {
    if (p < r.bFrom) break;
    if (p > r.bFrom && p < r.bTo) return null;
    if (p === r.bFrom && p === r.bTo) return side === 'before' ? r.aFrom : r.aTo;
    if (p === r.bFrom) return r.aFrom;
    if (p === r.bTo) return r.aTo;
    delta = r.aTo - r.bTo;
  }
  return p + delta;
}

/**
 * The three-way patch: original with the edit from base to now applied,
 * where base and now are the serializer's text of the same block before
 * and after the edit and original is the block's source. toOriginal is
 * diff(original, base), when the caller has it already (it does not
 * change while the block is edited). Returns null when the patch does not
 * apply cleanly: an edit of diff(base, now) starts or ends inside text the
 * serializer wrote differently from the source, or a diff is too large.
 */
export function rebase(
  original: string,
  base: string,
  now: string,
  toOriginal: Edit[] | null = diff(original, base)
): string | null {
  if (base === now) return original;
  if (original === base) return now;
  const edits = diff(base, now);
  if (!toOriginal || !edits) return null;
  const rs = regions(toOriginal);
  const mapped: Edit[] = [];
  for (const e of edits) {
    let from: number | null;
    let to: number | null;
    if (e.from === e.to) {
      from = to = mapBack(rs, e.from, 'before');
    } else {
      // A range keeps out text the source has at its ends and base dropped.
      from = mapBack(rs, e.from, 'after');
      to = mapBack(rs, e.to, 'before');
    }
    if (from === null || to === null || to < from) return null;
    const prev = mapped[mapped.length - 1];
    if (prev && from < prev.to) return null;
    mapped.push({ from, to, insert: e.insert });
  }
  return apply(original, mapped);
}
