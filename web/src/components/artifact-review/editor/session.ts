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

import { normalizeCritic, projectCritic } from '../../../utils/critic.js';

import { chooseAlphabet, chooseOpeningAlphabet } from '../../../utils/critic-sentinels.js';
import { loadDocument, saveDoc } from './bridge.js';
import type { LoadedDoc } from './bridge.js';
import type { Core } from './create.js';
import {
  docMarkRuns,
  earlierKept,
  keyedMarks,
  marksReadBack,
  sameRuns,
  textMarkRuns,
} from './guard.js';
import {
  consumedAsInSource,
  docStrings,
  makeBaseline,
  savePreserving,
  sentinelInDefinition,
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

  constructor(private readonly core: Core) {}

  /**
   * Opens md in the editor. Returns false (and leaves the editor empty)
   * when the document cannot be represented faithfully, so the caller can
   * fall back to the source view: no free alphabet, a mark or note that
   * would show in another place than the source has it, or with other
   * text (the load invariant: consumedAsInSource, then readOnlyUnits).
   */
  load(md: string): boolean {
    const { view, schema, parse, parseMd } = this.core;
    // The alphabet must be free in the source and in what the Markdown
    // parser makes of the sentinel form (character references decode to
    // candidate characters, which would otherwise read as sentinels).
    const a = chooseOpeningAlphabet(
      md,
      (text) => `${docStrings(parse(md))}\n${docStrings(parse(text))}`
    );
    if (!a) return false;
    const loaded = loadDocument(md, parse, schema, a);
    if (!loaded) return false;
    if (sentinelInDefinition(md, a, this.core.parseMdRaw)) return false;
    const held = consumedAsInSource(md, a, parseMd, loaded.doc.childCount, loaded.consumed);
    if (!held) return false;
    const base = makeBaseline(md, loaded.doc, parseMd, a);
    const readOnly = this.readOnlyUnits(md, loaded, base, held);
    if (!readOnly) return false;
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
    this.baseline = makeBaseline(md, view.state.doc, parseMd, a);
    this.source = md;
    this.readOnly = readOnly;
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
    for (let k = 0; k < base.blocks.length; k++) {
      const [i, j] = unitRangeOf(base, k, k);
      k = j;
      const nodes: Node[] = [];
      for (let q = i; q <= j; q++) nodes.push(base.nodes[q]);
      const runs = docMarkRuns(schema.topNodeType.create(null, nodes));
      const end = starts[j] + base.blocks[j].src.length;
      const plain = this.core.plain;
      if (sameRuns(runs, textMarkRuns(md, plain, starts[i], end), true)) continue;
      // The only differences allowed are the held tokens.
      if (!sameRuns(runs, textMarkRuns(md, plain, starts[i], end, held), true)) return null;
      for (let q = i; q <= j; q++) out.add(q);
    }
    return out;
  }

  private unitRange(first: number, last: number): [number, number] {
    return unitRangeOf(this.baseline!, first, last);
  }

  private unitSource(i: number, j: number): string {
    return unitSourceOf(this.baseline!, i, j);
  }

  /**
   * The marks of blocks i..j of the opening text (the whole text when i
   * is undefined), all earlier, keyed as the save invariant compares them:
   * each with its place in that unit's source with the marks removed
   * (withPlace).
   */
  private earlierOf(i?: number, j?: number, withPlace = true): string[] {
    const whole = i === undefined || j === undefined;
    const src = whole ? this.source : this.unitSource(i, j);
    return keyedMarks(src, () => false, withPlace).map((m) => m.key);
  }

  /**
   * Whether the top-level blocks first..last of doc (the units they belong
   * to) would save as text the hub accepts and the reviewer meant: the
   * same clean text as their source, marks that read back as the
   * document's, and every earlier mark and note of the source unchanged,
   * in order and in its place (earlierKept). Returns null when they would,
   * 'moved' when only an earlier mark's place would change, and 'generic'
   * otherwise (also for a read-only unit, or blocks that no longer line up
   * with the opening text).
   */
  unitCheck(doc: Node, first: number, last: number): HintKind | null {
    const { schema, serialize } = this.core;
    const clean = (t: string): string => normalizeCritic(projectCritic(t, 'clean'));
    const base = this.baseline;
    if (!base || doc.childCount !== base.blocks.length) return 'generic';
    const [i, j] = this.unitRange(first, last);
    for (let k = i; k <= j; k++) if (this.readOnly.has(k)) return 'generic';
    const src = this.unitSource(i, j);
    const kids: Node[] = [];
    for (let k = i; k <= j; k++) kids.push(doc.child(k));
    const unit = schema.topNodeType.create(null, kids);
    const a = chooseAlphabet(docStrings(unit));
    if (!a) return 'generic';
    const md = saveDoc(unit, serialize, schema, a).replace(/\n+$/, '');
    if (clean(md) !== clean(src)) return 'generic';
    if (marksReadBack(unit, md, this.core.plain) !== null) return 'generic';
    // The written unit must open again as it is shown (for example, an
    // backslashed autolink-looking text must not become a link that takes in
    // a mark's closing token).
    if (!this.reopens(md)) return 'generic';
    const runs = docMarkRuns(unit);
    if (earlierKept(this.earlierOf(i, j), md, runs)) return null;
    return earlierKept(this.earlierOf(i, j, false), md, runs, false) ? 'moved' : 'generic';
  }

  /** Whether md opens in the editor (the load checks, without showing it). */
  private reopens(md: string): boolean {
    const { schema, parse, parseMd } = this.core;
    const a = chooseOpeningAlphabet(
      md,
      (text) => `${docStrings(parse(md))}\n${docStrings(parse(text))}`
    );
    if (!a) return false;
    const loaded = loadDocument(md, parse, schema, a);
    if (!loaded || sentinelInDefinition(md, a, this.core.parseMdRaw)) return false;
    const held = consumedAsInSource(md, a, parseMd, loaded.doc.childCount, loaded.consumed);
    if (!held) return false;
    return this.readOnlyUnits(md, loaded, makeBaseline(md, loaded.doc, parseMd, a), held) !== null;
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
    const r = savePreserving(doc, this.baseline, serialize, schema, this.readOnly);
    if (!r) return null;
    for (const u of r.units) {
      if (!u.written) continue;
      const kids: Node[] = [];
      if (u.i < 0) doc.forEach((n) => kids.push(n));
      else for (let k = u.i; k <= u.j; k++) kids.push(doc.child(k));
      const unit = schema.topNodeType.create(null, kids);
      if (marksReadBack(unit, u.md, this.core.plain) !== null) return null;
      const runs = docMarkRuns(unit);
      const before = u.i < 0 ? this.earlierOf() : this.earlierOf(u.i, u.j);
      if (!earlierKept(before, u.md, runs)) return null;
    }
    return r.md;
  }
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

/** The unit (blocks joined by a spanning mark) around blocks first..last. */
function unitRangeOf(base: Baseline, first: number, last: number): [number, number] {
  let i = first;
  let j = last;
  while (i > 0 && base.blocks[i - 1].unit === base.blocks[i].unit) i--;
  while (j + 1 < base.blocks.length && base.blocks[j + 1].unit === base.blocks[j].unit) j++;
  return [i, j];
}

/** The source text of blocks i..j. */
function unitSourceOf(base: Baseline, i: number, j: number): string {
  let src = '';
  for (let k = i; k <= j; k++) src += base.blocks[k].src + (k < j ? base.blocks[k].after : '');
  return src;
}
