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
 * The guard: a review may change nothing outside its marks, and the hub
 * refuses a review that does. The editor checks this twice:
 *
 * - per edit, cheaply: the clean reading (insertions and replacements' new
 *   sides dropped, deletions and old sides kept, comments dropped) of each
 *   changed top-level block must be the same before and after;
 * - per save: the marks parseCritic reads from the saved text must be the
 *   marks of the document (kinds and texts), which catches a mark written
 *   in a form that would be read as literal text.
 *
 * The page also runs the full onlyMarksChanged check on the saved text, as
 * it does for the source view.
 */

import type { Node } from '@milkdown/prose/model';

import { parseCritic } from '../../../utils/critic.js';
import { COMMENT, isMine, markKeyOf, runKeyOf } from './schema.js';
import type { MarkKey } from './schema.js';

/** The clean reading of a node: what the text is with every mark rejected. */
export function cleanReading(node: Node): string {
  const parts: string[] = [];
  const walk = (n: Node): void => {
    if (n.isText) {
      const k = markKeyOf(n.marks);
      if (k !== 'ins' && k !== 'subNew') parts.push(n.text ?? '');
      return;
    }
    if (n.type.name === COMMENT) return;
    if (n.isLeaf) {
      const k = markKeyOf(n.marks);
      if (k !== 'ins' && k !== 'subNew') parts.push(`￼${n.type.name}${JSON.stringify(n.attrs)}`);
      return;
    }
    parts.push(`<${n.type.name}${JSON.stringify(n.attrs)}>`);
    n.forEach(walk);
    parts.push(`</${n.type.name}>`);
  };
  walk(node);
  return parts.join('');
}

/**
 * Whether the clean reading of the top-level blocks between from and to
 * (positions in before) is the same in after, whose blocks are found by
 * mapping. Both documents must have the same number of top-level blocks.
 */
export function cleanUnchanged(before: Node, after: Node, from: number, to: number): boolean {
  if (before.childCount !== after.childCount) return false;
  const first = before.resolve(from).index(0);
  const last = before.resolve(to).index(0);
  for (let i = first; i <= last && i < before.childCount; i++) {
    if (cleanReading(before.child(i)) !== cleanReading(after.child(i))) return false;
  }
  return true;
}

/** One mark as the document holds it and as parseCritic reads it. */
export interface MarkRun {
  kind: 'insertion' | 'deletion' | 'substitution' | 'highlight' | 'comment';
  /** The mark's text (a replacement's old side); a comment's note. */
  text: string;
  /** A replacement's new side. */
  next?: string;
  /** Added in this editing session (document runs only). */
  mine?: boolean;
}

const KIND_OF: Record<MarkKey, MarkRun['kind']> = {
  ins: 'insertion',
  del: 'deletion',
  subOld: 'substitution',
  subNew: 'substitution',
  hl: 'highlight',
};

/**
 * The marks of a document in order, with their text (a replacement once,
 * with both sides). Runs follow runKeyOf, as the serializer writes them.
 */
export function docMarkRuns(doc: Node): MarkRun[] {
  const out: MarkRun[] = [];
  let prev: string | null = null;
  let cur: MarkRun | null = null;
  let side: 'text' | 'next' = 'text';
  doc.descendants((n) => {
    if (n.type.name === COMMENT) {
      out.push({ kind: 'comment', text: String(n.attrs.note), mine: n.attrs.mine === true });
      prev = null;
      cur = null;
      return false;
    }
    if (n.isBlock) return true; // A run that continues into the next block stays one mark.
    const k = markKeyOf(n.marks);
    const run = runKeyOf(n.marks);
    if (k === null || run === null) {
      prev = null;
      cur = null;
      return false;
    }
    if (run !== prev) {
      if (prev === 'subOld' && k === 'subNew' && cur) {
        side = 'next';
        cur.next = '';
      } else {
        cur = { kind: KIND_OF[k], text: '', mine: isMine(n.marks) };
        side = 'text';
        if (k === 'subOld' || k === 'subNew') cur.next = '';
        if (k === 'subNew') side = 'next';
        out.push(cur);
      }
    }
    const t = n.isText ? (n.text ?? '') : '';
    if (cur) {
      if (side === 'next') cur.next = (cur.next ?? '') + t;
      else cur.text += t;
    }
    prev = run;
    return false;
  });
  return out;
}

/**
 * The document ranges of each mark run, in the order of docMarkRuns (a
 * replacement's two sides are one run; a run may span blocks).
 */
export function docMarkRanges(doc: Node): { from: number; to: number }[][] {
  const out: { from: number; to: number }[][] = [];
  let prev: string | null = null;
  doc.descendants((n, pos) => {
    if (n.type.name === COMMENT) {
      out.push([{ from: pos, to: pos + 1 }]);
      prev = null;
      return false;
    }
    if (n.isBlock) return true;
    const run = runKeyOf(n.marks);
    if (run === null) {
      prev = null;
      return false;
    }
    if (run !== prev && !(prev === 'subOld' && run === 'subNew')) out.push([]);
    out[out.length - 1].push({ from: pos, to: pos + n.nodeSize });
    prev = run;
    return false;
  });
  return out;
}

/** The sequence of mark kinds in a document, in order (a replacement once). */
export function docMarkKinds(doc: Node): string[] {
  return docMarkRuns(doc).map((r) => r.kind);
}

/** The sequence of mark kinds parseCritic reads in text. */
export function textMarkKinds(text: string): string[] {
  return parseCritic(text)
    .filter((s) => s.kind !== 'text')
    .map((s) => s.kind);
}

/** The marks parseCritic reads in text, their texts read by plain() (comments as written). */
export function textMarkRuns(
  text: string,
  plain: (md: string) => string,
  from = 0,
  to = Infinity,
  skip: ReadonlySet<number> = new Set()
): MarkRun[] {
  return new TextMarks(text).runs(plain, from, to, skip);
}

/**
 * The segments parseCritic reads in one text, read once, for the marks of
 * many ranges of it (textMarkRuns for each range, without reading the
 * whole text again each time).
 */
export class TextMarks {
  private readonly segs: ReturnType<typeof parseCritic>;
  /** Whether the segments' starts strictly increase (a range is then found by search). */
  private readonly ordered: boolean;

  constructor(text: string) {
    this.segs = parseCritic(text);
    this.ordered = this.segs.every((s, i) => i === 0 || this.segs[i - 1].start < s.start);
  }

  /** textMarkRuns(text, plain, from, to, skip). */
  runs(
    plain: (md: string) => string,
    from = 0,
    to = Infinity,
    skip: ReadonlySet<number> = new Set()
  ): MarkRun[] {
    const out: MarkRun[] = [];
    const segs = this.segs;
    for (let k = this.ordered ? this.firstAt(from) : 0; k < segs.length; k++) {
      const s = segs[k];
      if (this.ordered && s.start >= to) break;
      if (s.kind === 'text' || s.start < from || s.start >= to || skip.has(s.start)) continue;
      if (s.kind === 'comment') out.push({ kind: 'comment', text: s.text });
      else if (s.kind === 'substitution') {
        out.push({ kind: 'substitution', text: plain(s.text), next: plain(s.next ?? '') });
      } else out.push({ kind: s.kind, text: plain(s.text) });
    }
    return out;
  }

  /** The index of the first segment starting at or after from. */
  private firstAt(from: number): number {
    let lo = 0;
    let hi = this.segs.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.segs[mid].start < from) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }
}

// Whitespace is not compared: line breaks inside a mark read differently
// as Markdown text and as the editor's text.
const squash = (t: string): string => t.replace(/\s+/g, '');

/** Whether two mark sequences agree (kinds; texts when plain; notes exactly). */
export function sameRuns(d: MarkRun[], t: MarkRun[], texts: boolean): boolean {
  return (
    d.length === t.length &&
    d.every((r, i) => {
      const o = t[i];
      if (r.kind !== o.kind) return false;
      if (!texts) return true;
      if (r.kind === 'comment') return r.text === o.text;
      return squash(r.text) === squash(o.text) && squash(r.next ?? '') === squash(o.next ?? '');
    })
  );
}

/**
 * Whether the saved text reads back as the document's marks: the same
 * kinds in the same order and, when plain is given (it reads a mark's
 * Markdown as the editor shows it), the same texts. Returns both readings
 * when they differ, for a diagnostic.
 */
export function marksReadBack(
  doc: Node,
  text: string,
  plain?: (md: string) => string
): { doc: MarkRun[]; text: MarkRun[] } | null {
  const d = docMarkRuns(doc);
  const t = plain
    ? textMarkRuns(text, plain)
    : textMarkKinds(text).map((kind) => ({ kind, text: '' }) as MarkRun);
  return sameRuns(d, t, !!plain) ? null : { doc: d, text: t };
}

/** How the save invariant compares a mark: kind, text, new side (a note is a comment's text). */
export function markKey(x: { kind: string; text: string; next?: string }): string {
  return `${x.kind}\u0000${x.text}\u0000${x.next ?? ''}`;
}

/** A mark of a text as the save invariant compares it. */
export interface KeyedMark {
  /** Kind, text, new side (and note, for a folded pair), then the place. */
  key: string;
  /** Source offset of the mark (of the highlight, for a folded pair). */
  start: number;
  /** Source offsets of every segment folded into it. */
  starts: number[];
  /** This session's own (every folded segment is). */
  mine: boolean;
}

/**
 * The marks of md in order, as the save invariant compares them (design
 * E5, S-c). Each key holds the mark's kind, text and new side and its
 * PLACE: its offset in md with every mark removed (the clean projection
 * of the Markdown source, so moving a mark across inline syntax such as a
 * link's closing bracket or an emphasis delimiter changes it). A
 * highlight directly followed by a comment of the same owner is ONE mark,
 * so a note placed between them changes the key. mineAt tells the owner
 * of the segment at a source offset; withPlace false leaves the place out.
 */
export function keyedMarks(
  md: string,
  mineAt: (start: number) => boolean = () => false,
  withPlace = true
): KeyedMark[] {
  const segs: { s: ReturnType<typeof parseCritic>[number]; place: number }[] = [];
  let clean = 0;
  for (const x of parseCritic(md)) {
    if (x.kind !== 'text') segs.push({ s: x, place: clean });
    if (
      x.kind === 'text' ||
      x.kind === 'deletion' ||
      x.kind === 'highlight' ||
      x.kind === 'substitution'
    ) {
      clean += x.text.replace(/\r\n?/g, '\n').length;
    }
  }
  const out: KeyedMark[] = [];
  const at = (place: number): string => (withPlace ? `\u0000@${place}` : '');
  for (let i = 0; i < segs.length; i++) {
    const { s, place } = segs[i];
    const n = segs[i + 1]?.s;
    const mine = mineAt(s.start);
    if (
      s.kind === 'highlight' &&
      n?.kind === 'comment' &&
      n.start === s.end &&
      mineAt(n.start) === mine
    ) {
      out.push({
        key: `${markKey(s)}\u0000${markKey(n)}${at(place)}`,
        start: s.start,
        starts: [s.start, n.start],
        mine,
      });
      i++;
      continue;
    }
    out.push({ key: `${markKey(s)}${at(place)}`, start: s.start, starts: [s.start], mine });
  }
  return out;
}

/**
 * The save invariant (I2), exact and positional: out's marks are aligned
 * index by index with the document's runs (runs, as the read-back compares
 * them); the marks whose runs are this session's own are dropped, and
 * what is left must equal before (the earlier marks, keyed by keyedMarks
 * with the same withPlace) exactly. Returns false when out's marks do not
 * align with runs.
 */
export function earlierKept(
  before: string[],
  out: string,
  runs: MarkRun[],
  withPlace = true
): boolean {
  const segs = parseCritic(out).filter((x) => x.kind !== 'text');
  if (segs.length !== runs.length) return false;
  const mineAt = new Map<number, boolean>();
  segs.forEach((s, i) => mineAt.set(s.start, runs[i].mine === true));
  const kept = keyedMarks(out, (st) => mineAt.get(st) === true, withPlace)
    .filter((m) => !m.mine)
    .map((m) => m.key);
  return kept.length === before.length && kept.every((k, i) => k === before[i]);
}
