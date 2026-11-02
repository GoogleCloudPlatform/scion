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
 * One editing session of the review editor: the text it was opened with,
 * the document built from it, and the save that writes unchanged blocks
 * from their source. Saving without an edit returns the opening text as
 * is; it never goes through the serializer.
 */

import type { Node } from '@milkdown/prose/model';
import { EditorState, TextSelection } from '@milkdown/prose/state';

import {
  criticCodeRanges,
  normalizeCritic,
  onlyMarksChanged,
  parseCritic,
  projectCritic,
} from '../../../utils/critic.js';
import type { CriticRange } from '../../../utils/critic.js';

import { chooseOpeningAlphabet } from '../../../utils/critic-sentinels.js';
import type { SentinelAlphabet } from '../../../utils/critic-sentinels.js';
import { loadDocument } from './bridge.js';
import type { LoadedDoc } from './bridge.js';
import { definitionNodes } from './create.js';
import type { Core, MdNode, MdRead, MdRoot } from './create.js';
import {
  docMarkRuns,
  earlierKept,
  keyedMarks,
  marksReadBack,
  sameRuns,
  TextMarks,
  textMarkRuns,
} from './guard.js';
import {
  consumedAsInSource,
  docStrings,
  makeBaseline,
  savePreserving,
  sentinelInDefinition,
  unitRange,
  unitSource,
  writeUnit,
} from './preserve.js';
import type { Baseline } from './preserve.js';
import type { HintKind } from './policy.js';

export class ReviewSession {
  private source = '';
  private baseline: Baseline | null = null;
  private dirty = false;
  /**
   * Read-only top-level blocks, always saved from their source: blocks
   * holding mark tokens inside attributes, and the units whose source the
   * editor does not read as the critic parser does (design 4.2, item 3).
   */
  private readOnly = new Set<number>();
  /** The source offset of each top-level block of the opening text. */
  private starts: number[] = [];
  /**
   * Source offsets of the mark tokens the critic parser reads but the
   * editor keeps as text (in code or an attribute, or a mark around
   * nothing); they lie in read-only units.
   */
  private held: ReadonlySet<number> = new Set();
  /**
   * Per mark kind (by its closer): the source offset of the first opener
   * of that kind the critic parser leaves as text. It found no closer
   * after it, so a closer of that kind written after it would end a mark
   * there instead (see addsCloser).
   */
  private literalOpeners = new Map<string, number>();
  /**
   * The source's link reference definitions, so that a unit read on its
   * own resolves its reference links as it does in the document; null
   * until read (sourceDefinitions).
   */
  private definitions: string | null = '';
  /** Counts loads, so a reading scheduled by an earlier load is dropped. */
  private loads = 0;
  /**
   * Per top-level block: whether another block in its line run (blocks
   * with no blank line between them, whose backticks the critic parser can
   * pair across the blocks) holds a backtick (see changesCode).
   */
  private codeNeighbours: boolean[] = [];
  /**
   * Whether the opening text holds a literal opener (attribute-held tokens
   * included) or a backtick shared across the blocks of a line run: then
   * an edit can change how tokens pair across units, which the per-unit
   * checks cannot see, and unitCheck also runs the whole-text check
   * (design E7). Suggest mode cannot create either, so it is set once per
   * load; when unsure, it is true.
   */
  private hazardous = true;
  /** The clean reading of the opening text, as the hub compares it. */
  private cleanSource = '';

  constructor(private readonly core: Core) {}

  /** Parses Markdown with the document's link reference definitions. */
  private readonly parseInDocument = (md: string): Node => {
    const defs = this.sourceDefinitions();
    return this.core.parse(defs ? `${md}\n\n${defs}\n` : md);
  };

  /**
   * The opening text's link reference definitions. When the load did not
   * read the source as Markdown, they are read when the browser is next
   * idle, or here, when an edit or save needs them first.
   */
  private sourceDefinitions(): string {
    if (this.definitions === null) {
      this.definitions = definitionsOf(
        this.source,
        definitionNodes(this.core.parseMdRaw(this.source))
      );
    }
    return this.definitions;
  }

  /**
   * Opens md in the editor. Returns false (and leaves the editor empty)
   * when the document cannot be represented faithfully, so the caller can
   * fall back to the source view: no free alphabet, a mark or note that
   * would show in another place than the source has it, or with other
   * text (the load invariant: consumedAsInSource, then readOnlyUnits).
   */
  load(md: string): boolean {
    const { view, parseMd } = this.core;
    const o = this.opening(md);
    if (!o) return false;
    const { a, loaded, held, base, readOnly } = o;
    // A fresh state: the opened document, with no undo history. Every
    // mark in it is earlier: a mark is the reviewer's own only if it is
    // made in this editor after this load (design E5/E6), and that is
    // recorded on the mark when it is made, never worked out later.
    view.updateState(
      EditorState.create({
        doc: loaded.doc,
        plugins: view.state.plugins,
        selection: TextSelection.atStart(loaded.doc),
      })
    );
    // The state holds the opened document itself, so its baseline is the
    // one the load checks built (block nodes are matched by identity).
    this.baseline =
      view.state.doc === loaded.doc ? base : makeBaseline(md, view.state.doc, parseMd, a);
    this.source = md;
    this.readOnly = readOnly;
    this.starts = this.baseline ? blockStarts(this.baseline) : [];
    this.held = held;
    this.literalOpeners = literalOpeners(md);
    this.definitions = o.definitions;
    const load = ++this.loads;
    if (o.definitions === null) {
      whenIdle(() => {
        if (load === this.loads) this.sourceDefinitions();
      });
    }
    this.codeNeighbours = this.baseline ? codeNeighbours(this.baseline) : [];
    this.hazardous =
      !this.baseline ||
      this.literalOpeners.size > 0 ||
      held.size > 0 ||
      readOnly.size > 0 ||
      this.codeNeighbours.some((x) => x);
    this.cleanSource = normalizeCritic(projectCritic(md, 'clean'));
    this.dirty = false;
    return true;
  }

  /**
   * The text part of the load invariant, per unit (blocks joined by a
   * spanning mark): the document's marks (kind, text, note, in order) must
   * be the critic parser's marks of the unit's source range, read in the
   * whole text. A unit holding tokens kept as text (held: in code or an
   * attribute, or a mark around nothing) is read-only and saved from its
   * source, and its other marks must still match exactly (the parser's
   * marks minus the held ones). Any other difference: null (the editor
   * does not open).
   */
  private readOnlyUnits(
    md: string,
    loaded: LoadedDoc,
    base: Baseline | null,
    held: ReadonlySet<number>
  ): Set<number> | null {
    const out = new Set(loaded.attributeBlocks);
    if (!base) return out;
    const { schema } = this.core;
    const starts = blockStarts(base);
    // The whole text is read once; each unit takes the marks of its range.
    const marks = new TextMarks(md);
    // plain is a function of its input alone: each distinct text is read once.
    const shown = new Map<string, string>();
    const plain = (t: string): string => {
      let v = shown.get(t);
      if (v === undefined) {
        v = this.core.plain(t);
        shown.set(t, v);
      }
      return v;
    };
    for (let k = 0; k < base.blocks.length; k++) {
      const [i, j] = unitRange(base, k, k);
      k = j;
      const nodes: Node[] = [];
      for (let q = i; q <= j; q++) nodes.push(base.nodes[q]);
      const runs = docMarkRuns(schema.topNodeType.create(null, nodes));
      const end = starts[j] + base.blocks[j].src.length;
      if (sameRuns(runs, marks.runs(plain, starts[i], end), true)) continue;
      // The only differences allowed are the held tokens.
      if (!sameRuns(runs, marks.runs(plain, starts[i], end, held), true)) return null;
      for (let q = i; q <= j; q++) out.add(q);
    }
    // Code the critic parser reads across the edge of a block (a code span
    // it runs on past the end of a heading, for example): the hub would
    // read a token typed in that block as code text, not as a mark, so the
    // units it touches are read-only (design 4.2, item 3).
    // Both lists are in source order: one walk over the blocks.
    const endOf = (k: number): number => starts[k] + base.blocks[k].src.length;
    let first = 0;
    for (const r of criticCodeRanges(md)) {
      let to = r.end;
      while (to > r.start && (md[to - 1] === '\n' || md[to - 1] === '\r')) to--;
      while (first < base.blocks.length && endOf(first) <= r.start) first++;
      let last = first;
      while (last < base.blocks.length && starts[last] < to) last++;
      // Blocks first..last-1 overlap the range; it is fine within one.
      if (
        last === first ||
        (last === first + 1 && starts[first] <= r.start && to <= endOf(first))
      ) {
        continue;
      }
      for (let k = first; k < last; k++) {
        const [i, j] = unitRange(base, k, k);
        for (let q = i; q <= j; q++) out.add(q);
      }
    }
    return out;
  }

  /**
   * The marks of blocks i..j of the opening text (the whole text when i
   * is undefined), all earlier, keyed as the save invariant compares them:
   * each with its place in that unit's source with the marks removed
   * (withPlace).
   */
  private earlierOf(i?: number, j?: number, withPlace = true): string[] {
    const whole = i === undefined || j === undefined;
    const src = whole ? this.source : unitSource(this.baseline!, i, j);
    return keyedMarks(src, () => false, withPlace).map((m) => m.key);
  }

  /**
   * Whether the top-level blocks first..last of doc (the units they belong
   * to) would save as text the hub accepts and the reviewer meant. The
   * text checked is exactly what save writes for them (writeUnit: the
   * three-way patch, item and row units): it must have the same clean
   * text as their source, marks that read back as the document's, open
   * again as shown, and keep every earlier mark and note of the source
   * unchanged, in order and in its place (earlierKept). Returns null when
   * it would, 'moved' when only an earlier mark's place would change, and
   * 'generic' otherwise (also for a read-only unit, or blocks that no
   * longer line up with the opening text).
   */
  unitCheck(doc: Node, first: number, last: number): HintKind | null {
    const { schema, serialize } = this.core;
    const clean = (t: string): string => normalizeCritic(projectCritic(t, 'clean'));
    const base = this.baseline;
    if (!base || doc.childCount !== base.blocks.length) return 'generic';
    const [i, j] = unitRange(base, first, last);
    for (let k = i; k <= j; k++) if (this.readOnly.has(k)) return 'generic';
    const src = unitSource(base, i, j);
    const kids: Node[] = [];
    for (let k = i; k <= j; k++) kids.push(doc.child(k));
    const unit = schema.topNodeType.create(null, kids);
    const md = writeUnit(doc, base, i, j, serialize, schema, this.parseInDocument);
    if (md === null) return 'generic';
    if (clean(md) !== clean(src)) return 'generic';
    if (this.addsCloser(i, j, md)) return 'generic';
    if (this.changesCode(doc, i, j, md)) return 'code-run';
    if (marksReadBack(unit, md, this.core.plain) !== null) return 'generic';
    // The written unit must open again as it is shown (for example, an
    // backslashed autolink-looking text must not become a link that takes in
    // a mark's closing token).
    if (!this.reopens(md)) return 'generic';
    const runs = docMarkRuns(unit);
    if (!earlierKept(this.earlierOf(i, j), md, runs)) {
      return earlierKept(this.earlierOf(i, j, false), md, runs, false) ? 'moved' : 'generic';
    }
    if (this.hazardous && !this.wholeTextOk(doc)) return 'generic';
    return null;
  }

  /**
   * Design E7: whether the text save would write for doc reads, as a
   * whole, as the hub reads it: the same clean text as the opening text,
   * and the critic parser's mark kinds over the whole text equal to the
   * document's (the tokens kept as text in read-only units aside). Save
   * checks the mark texts too.
   */
  private wholeTextOk(doc: Node): boolean {
    const { schema, serialize } = this.core;
    const r = savePreserving(
      doc,
      this.baseline,
      serialize,
      schema,
      this.parseInDocument,
      this.readOnly
    );
    if (!r) return false;
    if (normalizeCritic(projectCritic(r.md, 'clean')) !== this.cleanSource) return false;
    const held = this.heldIn(r.units);
    const kinds = parseCritic(r.md)
      .filter((x) => x.kind !== 'text' && !held.has(x.start))
      .map((x) => x.kind);
    const want = docMarkRuns(doc).map((x) => x.kind);
    return kinds.length === want.length && kinds.every((k, n) => k === want[n]);
  }

  /** Whether the opened text is hazardous (design E7): the per-edit whole-text check runs. */
  get isHazardous(): boolean {
    return this.hazardous;
  }

  /** Whether md opens in the editor (the load checks, without showing it). */
  private reopens(md: string): boolean {
    return this.opening(md) !== null;
  }

  /**
   * The load checks for md, without showing it: what load needs, or null
   * when md does not open (no free alphabet, a note or sentinel not placed
   * as in the source, a sentinel in a link reference definition, or marks
   * other than the critic parser's). Each distinct text is read as Markdown
   * once (Core.read) and that reading serves every check that reads it.
   */
  private opening(md: string): Opening | null {
    const { schema } = this.core;
    const reads = new Map<string, MdRead>();
    const read = (text: string): MdRead => {
      let r = reads.get(text);
      if (!r) {
        r = this.core.read(text);
        reads.set(text, r);
      }
      return r;
    };
    // The alphabet must be free in the source and in what the Markdown
    // parser makes of the sentinel form (character references decode to
    // candidate characters, which would otherwise read as sentinels).
    const a = chooseOpeningAlphabet(
      md,
      (text) => `${docStrings(read(md).doc)}\n${docStrings(read(text).doc)}`
    );
    if (!a) return null;
    let tagged = '';
    const loaded = loadDocument(
      md,
      (text) => {
        tagged = text;
        return read(text).doc;
      },
      schema,
      a
    );
    if (!loaded) return null;
    if (sentinelInDefinition(md, a, read(tagged).definitions)) return null;
    const tree = (text: string): MdRoot => read(text).tree;
    const held = consumedAsInSource(md, a, tree, loaded.doc.childCount, loaded.consumed);
    if (!held) return null;
    const base = makeBaseline(md, loaded.doc, tree, a);
    const readOnly = this.readOnlyUnits(md, loaded, base, held);
    if (!readOnly) return null;
    const source = reads.get(md);
    const definitions = source ? definitionsOf(md, source.definitions) : null;
    return { a, loaded, held, base, readOnly, definitions };
  }

  /** unitCheck passes. */
  unitOk(doc: Node, first: number, last: number): boolean {
    return this.unitCheck(doc, first, last) === null;
  }

  /**
   * The hint for an edit refused in block because it touches a mark the
   * reviewer did not make: the earlier-review hint, or the generic one in
   * a read-only unit.
   */
  earlierHint(block: number): HintKind {
    return this.readOnly.has(block) ? 'generic' : 'earlier';
  }

  /** Whether the document holds a mark or comment made in this editor since the last load. */
  hasOwnMarks(): boolean {
    return docMarkRuns(this.core.view.state.doc).some((r) => r.mine === true);
  }

  /** Records whether the document differs from the opening text (a composition that changed nothing). */
  setDirty(dirty: boolean): void {
    this.dirty = dirty;
  }

  /** The read-only top-level blocks, in order. */
  readOnlyBlocks(): number[] {
    return [...this.readOnly].sort((x, y) => x - y);
  }

  /** Records that the document changed after load. */
  markDirty(): void {
    this.dirty = true;
  }

  get isDirty(): boolean {
    return this.dirty;
  }

  /**
   * The current text. Without an edit, the opening text. Returns null if
   * the edited document cannot be written (no free sentinel alphabet), or
   * if any unit the serializer wrote would not read back as the document
   * or would change an earlier mark or note (key, order or place). Records
   * which marks of the text are own, for the next load.
   */
  save(): string | null {
    if (!this.dirty) return this.source;
    const { view, schema, serialize } = this.core;
    const doc = view.state.doc;
    const r = savePreserving(
      doc,
      this.baseline,
      serialize,
      schema,
      this.parseInDocument,
      this.readOnly
    );
    if (!r) return null;
    for (const u of r.units) {
      if (!u.written) continue;
      const kids: Node[] = [];
      if (u.i < 0) doc.forEach((n) => kids.push(n));
      else for (let k = u.i; k <= u.j; k++) kids.push(doc.child(k));
      const unit = schema.topNodeType.create(null, kids);
      if (marksReadBack(unit, u.md, this.core.plain) !== null) return null;
      if (u.i >= 0 && this.addsCloser(u.i, u.j, u.md)) return null;
      if (u.i >= 0 && this.changesCode(doc, u.i, u.j, u.md)) return null;
      const runs = docMarkRuns(unit);
      const before = u.i < 0 ? this.earlierOf() : this.earlierOf(u.i, u.j);
      if (!earlierKept(before, u.md, runs)) return null;
    }
    // The whole text, as the hub reads it (design 4.6 items 2 and 3): the
    // same clean text as the opening text, and the marks the critic parser
    // reads in it, over the whole text, are the document's (the tokens
    // kept as text in read-only units aside).
    if (!onlyMarksChanged(r.md, projectCritic(this.source, 'clean'))) return null;
    const runs = textMarkRuns(r.md, this.core.plain, 0, Infinity, this.heldIn(r.units));
    if (!sameRuns(docMarkRuns(doc), runs, true)) return null;
    return r.md;
  }

  /**
   * Whether the written text md of units i..j holds more closers of a
   * mark kind than their source (for a replacement, more closers or more
   * ~> separators), for a kind whose first literal opener comes before
   * them (for a ~>, before their end: in these units or earlier): the
   * critic parser would read a mark from that opener with the new token,
   * over text outside these units (a {~~ followed by a ~~} but no ~> is
   * literal until a ~> appears between them). Tokens inside the code the
   * critic parser reads in md (or the source) are not counted.
   */
  private addsCloser(i: number, j: number, md: string): boolean {
    const base = this.baseline;
    if (!base || this.literalOpeners.size === 0) return false;
    const src = unitSource(base, i, j);
    const end = this.starts[j] + base.blocks[j].src.length;
    for (const [close, at] of this.literalOpeners) {
      // A closer in the opener's own unit is seen by the unit's own checks;
      // a ~> there is not, as the ~~} it needs lies in a later unit.
      if (at < this.starts[i] && countOutsideCode(md, close) > countOutsideCode(src, close)) {
        return true;
      }
      if (close === '~~}' && at < end && countOutsideCode(md, '~>') > countOutsideCode(src, '~>')) {
        return true;
      }
    }
    return false;
  }

  /**
   * Whether the written text md of units i..j of doc could change how the
   * critic parser pairs backticks with text outside it (its code ranges
   * depend only on backtick runs, their backslashes, and blank and fence
   * lines, and suggest mode adds no blank line): its fence-opener lines
   * differ from its source's, or its backtick runs differ while another
   * block of its line run holds a backtick, or, for a list or table, while
   * an item or row it did not change holds one (the per-unit checks read
   * the unit alone, so they cannot see such a pairing).
   */
  private changesCode(doc: Node, i: number, j: number, md: string): boolean {
    const base = this.baseline;
    if (!base) return false;
    const src = unitSource(base, i, j);
    if (fenceLines(md) !== fenceLines(src)) return true;
    if (backtickRuns(md) === backtickRuns(src)) return false;
    for (let k = i; k <= j; k++) {
      if (this.codeNeighbours[k]) return true;
      const subs = base.blocks[k].subs;
      const was = base.nodes[k];
      const now = doc.child(k);
      if (!subs || now.childCount !== was.childCount) continue;
      for (let c = 0; c < subs.length; c++) {
        if (now.child(c) !== was.child(c)) continue;
        if (base.blocks[k].src.slice(subs[c].from, subs[c].to).includes('`')) return true;
      }
    }
    return false;
  }

  /**
   * The offsets in the saved text of the held tokens, from savePreserving's
   * units: they lie in read-only units, which are written from their source.
   */
  private heldIn(
    units: readonly { i: number; md: string; at: number; written: boolean }[]
  ): Set<number> {
    const out = new Set<number>();
    if (this.held.size === 0) return out;
    for (const u of units) {
      if (u.written || u.i < 0) continue;
      const from = this.starts[u.i];
      const to = from + u.md.length;
      for (const h of this.held) if (h >= from && h < to) out.add(u.at + h - from);
    }
    return out;
  }
}

/**
 * The backtick runs of text in order, each as its length and whether an
 * odd number of backslashes comes before it ("e"), comma-separated.
 */
export function backtickRuns(text: string): string {
  const out: string[] = [];
  for (const m of text.matchAll(/`+/g)) {
    let k = m.index ?? 0;
    let slashes = 0;
    while (k > 0 && text[k - 1] === '\\') {
      slashes++;
      k--;
    }
    out.push(`${m[0].length}${slashes % 2 ? 'e' : ''}`);
  }
  return out.join(',');
}

/** The lines of text that would open (or close) a fenced code block. */
export function fenceLines(text: string): number {
  return text.split('\n').filter((l) => /^ {0,3}(`{3,}|~{3,})/.test(l)).length;
}

/**
 * Per top-level block of base: whether another block of its line run
 * holds a backtick, or the source between blocks of the run (or before its
 * first block) does: link reference definitions live there, and the critic
 * parser pairs their backticks too. A line run is a sequence of blocks with
 * no blank line in the source between them; text between blocks that holds
 * a backtick counts for the runs on both sides of it, blank line or not.
 */
export function codeNeighbours(base: Baseline): boolean[] {
  const n = base.blocks.length;
  const run: number[] = [];
  let r = 0;
  for (let k = 0; k < n; k++) {
    run.push(r);
    if (/\n[ \t]*\r?\n/.test(base.blocks[k].after.replace(/\r\n/g, '\n'))) r++;
  }
  const ticks = base.blocks.map((b) => b.src.includes('`'));
  // Per line run, how many of its blocks hold a backtick.
  const runTicks = new Map<number, number>();
  for (let k = 0; k < n; k++) if (ticks[k]) runTicks.set(run[k], (runTicks.get(run[k]) ?? 0) + 1);
  const gapTicks = new Set<number>();
  if (n > 0 && base.lead.includes('`')) gapTicks.add(run[0]);
  for (let k = 0; k < n; k++) {
    if (!base.blocks[k].after.includes('`')) continue;
    gapTicks.add(run[k]);
    if (k + 1 < n) gapTicks.add(run[k + 1]);
  }
  return base.blocks.map(
    (_, k) => gapTicks.has(run[k]) || (runTicks.get(run[k]) ?? 0) > (ticks[k] ? 1 : 0)
  );
}

/** The opener of each mark kind, by its closer. */
const OPENERS: [open: string, close: string][] = [
  ['{++', '++}'],
  ['{--', '--}'],
  ['{~~', '~~}'],
  ['{==', '==}'],
  ['{>>', '<<}'],
];

/** Whether p lies in one of ranges (in order, not overlapping). */
function inRanges(ranges: readonly CriticRange[], p: number): boolean {
  let lo = 0;
  let hi = ranges.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (ranges[mid].end <= p) lo = mid + 1;
    else if (ranges[mid].start > p) hi = mid - 1;
    else return true;
  }
  return false;
}

/** The occurrences of s in text outside the code the critic parser reads there. */
function countOutsideCode(text: string, s: string): number {
  const code = criticCodeRanges(text);
  let n = 0;
  for (let i = text.indexOf(s); i >= 0; i = text.indexOf(s, i + 1)) if (!inRanges(code, i)) n++;
  return n;
}

/**
 * Per mark kind (by its closer), the offset of the first opener of that
 * kind the critic parser leaves as text in md (outside its code).
 */
function literalOpeners(md: string): Map<string, number> {
  const code = criticCodeRanges(md);
  const out = new Map<string, number>();
  for (const seg of parseCritic(md)) {
    if (seg.kind !== 'text') continue;
    const text = md.slice(seg.start, seg.end);
    for (const [open, close] of OPENERS) {
      if (out.has(close)) continue;
      for (let i = text.indexOf(open); i >= 0; i = text.indexOf(open, i + 1)) {
        if (!inRanges(code, seg.start + i)) {
          out.set(close, seg.start + i);
          break;
        }
      }
    }
  }
  return out;
}

/** The text of md's link reference definitions (definitionNodes of md), one per line, in order. */
function definitionsOf(md: string, definitions: readonly MdNode[]): string {
  const out: string[] = [];
  for (const n of definitions) {
    const from = n.position?.start.offset;
    const to = n.position?.end.offset;
    if (from !== undefined && to !== undefined) out.push(md.slice(from, to));
  }
  return out.join('\n');
}

/** Runs f when the browser is next idle (soon after, where it cannot say). */
function whenIdle(f: () => void): void {
  const idle = (
    globalThis as {
      requestIdleCallback?: (cb: () => void, opts?: { timeout: number }) => number;
    }
  ).requestIdleCallback;
  if (idle) idle(f, { timeout: 1000 });
  else setTimeout(f, 0);
}

/** What the load checks found for a text that opens (ReviewSession.opening). */
interface Opening {
  a: SentinelAlphabet;
  loaded: LoadedDoc;
  held: Set<number>;
  base: Baseline | null;
  readOnly: Set<number>;
  /** The text's link reference definitions, when the checks read the text itself; else null. */
  definitions: string | null;
}

/** The source offset of each top-level block. */
function blockStarts(base: Baseline): number[] {
  const out: number[] = [];
  let at = base.lead.length;
  for (const b of base.blocks) {
    out.push(at);
    at += b.src.length + b.after.length;
  }
  return out;
}
