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
 * Block-level source preservation. A Markdown serializer normalizes what it
 * writes (list bullets, table padding, backslashes), so re-serializing a whole
 * document after a one-word edit would change text outside the marks.
 * Instead each top-level block keeps the exact source it was loaded from.
 * On save, a block that is still the same node object as at load
 * (ProseMirror shares untouched nodes between states) is written from that
 * source, and only edited blocks go through the serializer.
 *
 * Blocks inside a mark that spans blocks are never kept verbatim on their
 * own; they are re-serialized with their neighbours.
 */

import type { Node, Schema } from '@milkdown/prose/model';

import {
  chooseAlphabet,
  criticToSentinelText,
  readTag,
  sentinelSet,
} from '../../../utils/critic-sentinels.js';
import type { SentinelAlphabet } from '../../../utils/critic-sentinels.js';
import { saveDoc } from './bridge.js';
import type { Consumed } from './bridge.js';
import type { MdNode as MdTreeNode } from './create.js';

interface Block {
  src: string;
  /** The source between this block and the next (or the end). */
  after: string;
  /** Blocks joined by a mark that spans the break between them share a unit. */
  unit: number;
}

export interface Baseline {
  lead: string;
  blocks: Block[];
  /** The top-level nodes as loaded, by position. */
  nodes: Node[];
}

interface MdNode {
  position?: { start: { offset?: number }; end: { offset?: number } };
}

/**
 * The baseline of doc, loaded from md under alphabet a, or null when the
 * document's top-level blocks do not line up one to one with the
 * Markdown's. parseMd is the Markdown parser the editor uses.
 */
export function makeBaseline(
  md: string,
  doc: Node,
  parseMd: (s: string) => { children: MdNode[] },
  a: SentinelAlphabet
): Baseline | null {
  const { text, starts, ends } = criticToSentinelText(md, a, true);
  const kids = parseMd(text).children;
  if (
    kids.length === 0 ||
    kids.length !== doc.childCount ||
    kids.some((k) => k.position?.start.offset === undefined || k.position?.end.offset === undefined)
  ) {
    return null;
  }
  const opens = new Set([a.insOpen, a.delOpen, a.subOpen, a.hlOpen]);
  const closes = new Set([a.insClose, a.delClose, a.subClose, a.hlClose]);
  // Whether text offset i is inside a mark (marks may span blocks).
  const inside: boolean[] = [];
  let open = false;
  for (let i = 0; i <= text.length; i++) {
    inside.push(open);
    if (opens.has(text[i])) open = true;
    else if (closes.has(text[i])) open = false;
  }
  const mdStart = (t: number): number => (t < text.length ? starts[t] : md.length);
  const mdEnd = (t: number): number => (t > 0 ? ends[t - 1] : 0);
  const spans = kids.map((k) => [k.position!.start.offset!, k.position!.end.offset!] as const);
  let unit = 0;
  const blocks: Block[] = spans.map(([s, e], i) => {
    // A mark open at the end of the previous block joins this one to it.
    if (i > 0 && !(inside[spans[i - 1][1]] && inside[s])) unit++;
    const next = i + 1 < spans.length ? mdStart(spans[i + 1][0]) : md.length;
    return { src: md.slice(mdStart(s), mdEnd(e)), after: md.slice(mdEnd(e), next), unit };
  });
  const lead = md.slice(0, mdStart(spans[0][0]));
  const nodes: Node[] = [];
  doc.forEach((n) => nodes.push(n));
  return { lead, blocks, nodes };
}

/** Every string the serializer could write for doc: text, notes and attributes. */
export function docStrings(doc: Node): string {
  const parts: string[] = [];
  const attrs = (o: Record<string, unknown>): void => {
    for (const v of Object.values(o)) if (typeof v === 'string') parts.push(v);
  };
  doc.descendants((n) => {
    if (n.isText) parts.push(n.text ?? '');
    attrs(n.attrs);
    for (const m of n.marks) attrs(m.attrs);
    return true;
  });
  return parts.join('\n');
}

/**
 * Saves doc, writing every unit of blocks unchanged since load from its
 * source and serializing the others (a unit with a mark that spans blocks
 * is serialized as a whole, so the mark stays one run). Units holding a
 * block of verbatim are always written from their source. Returns null when
 * no sentinel alphabet is free for the edited text, or when blocks were
 * added or removed while verbatim is not empty.
 */
export function savePreserving(
  doc: Node,
  base: Baseline | null,
  serialize: (d: Node) => string,
  schema: Schema,
  verbatim: ReadonlySet<number> = new Set()
): SaveResult | null {
  const a = chooseAlphabet(docStrings(doc));
  if (!a) return null;
  if (!base || doc.childCount !== base.blocks.length) {
    if (verbatim.size > 0) return null;
    const whole = saveDoc(doc, serialize, schema, a);
    return {
      md: whole,
      rewritten: doc.childCount,
      units: [{ i: -1, j: -1, md: whole, at: 0, written: true }],
    };
  }
  let md = base.lead;
  let rewritten = 0;
  const units: SaveResult['units'] = [];
  let i = 0;
  while (i < base.blocks.length) {
    let j = i;
    while (j + 1 < base.blocks.length && base.blocks[j + 1].unit === base.blocks[i].unit) j++;
    let same = true;
    for (let k = i; k <= j; k++) if (doc.child(k) !== base.nodes[k]) same = false;
    for (let k = i; k <= j; k++) if (verbatim.has(k)) same = true;
    if (same) {
      const at = md.length;
      let unit = '';
      for (let k = i; k <= j; k++) {
        unit += base.blocks[k].src;
        if (k < j) unit += base.blocks[k].after;
      }
      units.push({ i, j, md: unit, at, written: false });
      md += unit;
    } else {
      const kids: Node[] = [];
      for (let k = i; k <= j; k++) kids.push(doc.child(k));
      rewritten += kids.length;
      const unit = saveDoc(schema.topNodeType.create(null, kids), serialize, schema, a).replace(
        /\n+$/,
        ''
      );
      units.push({ i, j, md: unit, at: md.length, written: true });
      md += unit;
    }
    md += base.blocks[j].after;
    i = j + 1;
  }
  return { md, rewritten, units };
}

/** What savePreserving wrote. */
export interface SaveResult {
  md: string;
  rewritten: number;
  /**
   * Every unit (blocks i..j), its text, where it starts in md, and whether
   * the serializer wrote it (otherwise it is its source); the whole
   * document as i = j = -1 when the blocks do not line up.
   */
  units: { i: number; j: number; md: string; at: number; written: boolean }[];
}

/**
 * The load invariant (I1), positional, on what the sentinel pass consumed
 * (md opened in the tagged sentinel form, where each note sentinel carries
 * its comment's ordinal):
 * - every sentinel lies inside a top-level block of the Markdown parser's
 *   reading (none in the gaps between blocks, where link reference
 *   definitions are);
 * - for every top-level block, the sentinels consumed while visiting it
 *   are exactly that block's sentinels, in order, and each note was taken
 *   by its own ordinal: the note the critic parser reads at that
 *   sentinel's source offset, not the next one in sequence.
 * Returns the source offsets of the tokens kept as text (in code or an
 * attribute, or a mark around nothing), or null when the check fails (the
 * editor then does not open).
 */
export function consumedAsInSource(
  md: string,
  a: SentinelAlphabet,
  parseMd: (s: string) => { children: MdNode[] },
  blockCount: number,
  consumed: Consumed[]
): Set<number> | null {
  const { text, starts } = criticToSentinelText(md, a, true);
  const sentinels = sentinelSet(a);
  const kids = parseMd(text).children;
  const any = [...text].some((c) => sentinels.has(c));
  if (
    kids.length !== blockCount ||
    kids.some((k) => k.position?.start.offset === undefined || k.position?.end.offset === undefined)
  ) {
    return !any && consumed.length === 0 ? new Set() : null;
  }
  const want: { ch: string; ordinal?: number; at: number }[][] = kids.map(() => []);
  let ordinal = 0;
  let k = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (!sentinels.has(c)) continue;
    while (k < kids.length && kids[k].position!.end.offset! <= i) k++;
    if (k >= kids.length || kids[k].position!.start.offset! > i) return null;
    if (c === a.note) {
      want[k].push({ ch: c, ordinal: ordinal++, at: starts[i] });
      i += readTag(text, i, a)?.digits ?? 0;
    } else {
      want[k].push({ ch: c, at: starts[i] });
    }
  }
  const got: Consumed[][] = kids.map(() => []);
  for (const x of consumed) {
    if (x.block < 0 || x.block >= got.length) return null;
    got[x.block].push(x);
  }
  const held = new Set<number>();
  for (let b = 0; b < want.length; b++) {
    if (want[b].length !== got[b].length) return null;
    for (let i = 0; i < want[b].length; i++) {
      const w = want[b][i];
      const g = got[b][i];
      if (w.ch !== g.ch || w.ordinal !== g.ordinal) return null;
      if (g.held) held.add(w.at);
    }
  }
  return held;
}

/**
 * Whether a sentinel of md's tagged sentinel form lies inside a link
 * reference definition at any depth (inside a block quote or a list item
 * too), read from the Markdown parser's tree before its transforms (which
 * copy definitions onto references and drop them).
 */
export function sentinelInDefinition(
  md: string,
  a: SentinelAlphabet,
  parseRaw: (s: string) => MdTreeNode
): boolean {
  const { text } = criticToSentinelText(md, a, true);
  const sentinels = sentinelSet(a);
  const hit = (n: MdTreeNode): boolean => {
    if (n.type === 'definition') {
      const from = n.position?.start.offset;
      const to = n.position?.end.offset;
      if (from === undefined || to === undefined) return true;
      for (let i = from; i < to; i++) if (sentinels.has(text[i])) return true;
      return false;
    }
    return (n.children ?? []).some(hit);
  };
  return hit(parseRaw(text));
}
