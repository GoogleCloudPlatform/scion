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
 * Markdown with CriticMarkup to the editor's document and back.
 *
 * Load: utils/critic-sentinels.ts replaces each mark token with a sentinel
 * (parseCritic reads the marks, so code is treated exactly as the hub
 * treats it), the stock Milkdown parser builds the document, then
 * applySentinels turns sentinel runs into flat marks and comment nodes and
 * deletes the sentinels. Save: the reverse.
 *
 * Where Markdown sees code but the critic parser does not, tokens inside
 * the code stay literal text, as written.
 */

import type { Mark, Node, Schema } from '@milkdown/prose/model';
import { Transform } from '@milkdown/prose/transform';

import {
  criticToSentinelText,
  readTag,
  sentinelSet,
  sentinelTextToCritic,
  tokenOf,
} from '../../../utils/critic-sentinels.js';
import type { SentinelAlphabet } from '../../../utils/critic-sentinels.js';
import { COMMENT, CRITIC_MARK_NAMES, MARKS, markKeyOf, runKeyOf } from './schema.js';
import type { MarkKey } from './schema.js';

interface Edit {
  pos: number;
  /** Characters replaced (a note sentinel and its ordinal tag); 1 when absent. */
  len?: number;
  note?: string;
  literal?: string;
  /** The consumed entry for this sentinel, marked held when it stays text. */
  entry?: Consumed;
}

/** Attribute names in the order their text appears in the Markdown source. */
const ATTR_ORDER: Record<string, string[]> = {
  image: ['alt', 'src', 'title'],
  link: ['href', 'title'],
  code_block: ['language'],
  html: ['value'],
};

/** What applySentinels found besides the marks. */
export interface SentinelResult {
  /** Notes consumed (every comment, in text, code or an attribute). */
  notesUsed: number;
  /**
   * Top-level blocks holding a token inside an attribute (a link target or
   * title, an image's alt, source or title, raw HTML, a code language). The
   * critic parser reads a mark there, which is not a mark in the editor, so
   * these blocks are read-only and always saved from their source.
   */
  attributeBlocks: Set<number>;
  /**
   * Every sentinel the pass consumed, in the order it visited them, with
   * the top-level block it was in and, for a comment, the note it took.
   */
  consumed: Consumed[];
}

/** One sentinel consumed by applySentinels. */
export interface Consumed {
  block: number;
  ch: string;
  note?: string;
  /** A note sentinel's ordinal tag (its comment's index in the source), -1 when missing. */
  ordinal?: number;
  /** The token stays text (inside code or an attribute, or a mark around nothing). */
  held?: boolean;
}

/**
 * Turns the sentinels of tr.doc into marks and comment nodes. A run may
 * cross block boundaries; the mark goes on every textblock in between that
 * allows it. A run that covers no markable text keeps its tokens as text.
 * Sentinels inside attributes become their tokens again (a comment with
 * its note), consuming notes in source order.
 */
export function applySentinels(
  tr: Transform,
  schema: Schema,
  notes: string[],
  a: SentinelAlphabet
): SentinelResult {
  const tokenMap = tokenOf(a);
  const tok = (c: string): string => tokenMap.get(c) ?? c;
  const sentinels = sentinelSet(a);
  const open = new Map<string, MarkKey>([
    [a.insOpen, 'ins'],
    [a.delOpen, 'del'],
    [a.subOpen, 'subOld'],
    [a.hlOpen, 'hl'],
  ]);
  const closeOf: Record<MarkKey, string> = {
    ins: a.insClose,
    del: a.delClose,
    subOld: a.subClose,
    subNew: a.subClose,
    hl: a.hlClose,
  };
  const openOf: Record<MarkKey, string> = {
    ins: a.insOpen,
    del: a.delOpen,
    subOld: a.subOpen,
    subNew: a.subOpen,
    hl: a.hlOpen,
  };
  const insType = schema.marks[MARKS.ins];
  let state: MarkKey | null = null;
  let runStart = 0;
  let openEdit: Edit | null = null;
  let sepEdit: Edit | null = null;
  let pending: { from: number; to: number; key: MarkKey }[] = [];
  const ranges: { from: number; to: number; key: MarkKey }[] = [];
  const edits: Edit[] = [];
  let noteIdx = 0;
  const attributeBlocks = new Set<number>();
  const consumed: Consumed[] = [];
  // A note is taken by the ordinal tag after its sentinel (its own place
  // in the source), never by the order the parse visits it in.
  const takeNote = (block: number, ch: string, ordinal: number, held = false): Consumed => {
    noteIdx++;
    const entry: Consumed = { block, ch, note: notes[ordinal] ?? '', ordinal, held };
    consumed.push(entry);
    return entry;
  };
  const take = (block: number, ch: string, held = false): Consumed => {
    const entry: Consumed = { block, ch, held };
    consumed.push(entry);
    return entry;
  };
  const attrEdits: (() => void)[] = [];
  // Restores the sentinels of attrs (in ATTR_ORDER), consuming notes;
  // returns the new attrs, or null when none held a sentinel.
  const restoreAttrs = (
    typeName: string,
    attrs: Record<string, unknown>,
    block: number
  ): Record<string, unknown> | null => {
    const names = ATTR_ORDER[typeName] ?? Object.keys(attrs);
    let changed = false;
    const out: Record<string, unknown> = { ...attrs };
    for (const name of names) {
      const v = attrs[name];
      if (typeof v !== 'string' || ![...v].some((c) => sentinels.has(c))) continue;
      let restored = '';
      for (let i = 0; i < v.length; i++) {
        const c = v[i];
        if (c === a.note) {
          const tag = readTag(v, i, a);
          restored += `{>>${takeNote(block, c, tag?.ordinal ?? -1, true).note}<<}`;
          i += tag?.digits ?? 0;
          continue;
        }
        if (sentinels.has(c)) take(block, c, true);
        restored += tok(c);
      }
      out[name] = restored;
      changed = true;
    }
    return changed ? out : null;
  };
  const markable = (from: number, to: number): boolean => {
    let ok = false;
    tr.doc.nodesBetween(from, to, (n, pos, parent) => {
      if (n.isText && parent?.type.allowsMarkType(insType)) {
        const t = (n.text ?? '').slice(Math.max(0, from - pos), to - pos);
        for (const c of t) {
          if (!sentinels.has(c)) {
            ok = true;
            break;
          }
        }
      }
      return !ok;
    });
    return ok;
  };
  const doc0 = tr.doc;
  // Every node in document order (as doc0.descendants), with the index of
  // the top-level block it is in.
  const visit = (
    node: Node,
    pos: number,
    parent: Node | null,
    index: number,
    block: number
  ): boolean => {
    if (!node.isText) {
      // Atoms (images, raw HTML) and a code block's language come before
      // their text in the source.
      const attrs = restoreAttrs(node.type.name, node.attrs, block);
      if (attrs) {
        attributeBlocks.add(block);
        attrEdits.push(() => {
          tr.setNodeMarkup(pos, undefined, attrs, node.marks);
        });
      }
      return true;
    }
    const t = node.text ?? '';
    const code = !parent?.type.allowsMarkType(insType);
    for (let i = 0; i < t.length; i++) {
      const c = t[i];
      if (!sentinels.has(c)) continue;
      const p = pos + i;
      if (c === a.note) {
        const tag = readTag(t, i, a);
        const len = 1 + (tag?.digits ?? 0);
        const entry = takeNote(block, c, tag?.ordinal ?? -1, code);
        i += len - 1;
        if (code) edits.push({ pos: p, len, literal: `{>>${entry.note}<<}`, entry });
        else edits.push({ pos: p, len, note: entry.note!, entry });
        continue;
      }
      const entry = take(block, c);
      if (code) {
        entry.held = true;
        edits.push({ pos: p, literal: tok(c), entry });
        continue;
      }
      const key = open.get(c);
      if (key && state === null) {
        state = key;
        runStart = p + 1;
        openEdit = { pos: p, entry };
        sepEdit = null;
        pending = [];
        edits.push(openEdit);
      } else if (c === a.subSep && state === 'subOld') {
        pending.push({ from: runStart, to: p, key: 'subOld' });
        state = 'subNew';
        runStart = p + 1;
        sepEdit = { pos: p, entry };
        edits.push(sepEdit);
      } else if (state !== null && c === closeOf[state]) {
        pending.push({ from: runStart, to: p, key: state });
        const closeEdit: Edit = { pos: p, entry };
        edits.push(closeEdit);
        if (pending.some((r) => markable(r.from, r.to))) {
          ranges.push(...pending);
        } else {
          openEdit!.literal = tok(openOf[state]);
          if (sepEdit) sepEdit.literal = tok(a.subSep);
          closeEdit.literal = tok(c);
        }
        state = null;
      } else {
        // Its partner was inside code: keep the token as written.
        edits.push({ pos: p, literal: tok(c), entry });
      }
    }
    // A link's target and title follow its text in the source: restore
    // them at the end of the link's run.
    const next = parent?.maybeChild(index + 1);
    for (const m of node.marks) {
      if (!ATTR_ORDER[m.type.name] || (next && m.isInSet(next.marks))) continue;
      const attrs = restoreAttrs(m.type.name, m.attrs, block);
      if (!attrs) continue;
      attributeBlocks.add(block);
      let start = pos;
      for (let k = index - 1; k >= 0; k--) {
        const prev = parent!.child(k);
        if (!m.isInSet(prev.marks)) break;
        start -= prev.nodeSize;
      }
      const end = pos + node.nodeSize;
      attrEdits.push(() => {
        tr.removeMark(start, end, m);
        tr.addMark(start, end, m.type.create(attrs));
      });
    }
    return false;
  };
  doc0.forEach((top, offset, block) => {
    if (visit(top, offset, doc0, block, block)) {
      top.descendants((node, pos, parent, index) =>
        visit(node, offset + 1 + pos, parent, index, block)
      );
    }
  });
  // Attribute changes keep every position, so they go first.
  for (const f of attrEdits) f();
  const endState = state as MarkKey | null;
  if (endState !== null && openEdit) {
    (openEdit as Edit).literal = tok(openOf[endState]);
    if (sepEdit) (sepEdit as Edit).literal = tok(a.subSep);
  }
  for (const r of ranges) {
    if (r.to > r.from) tr.addMark(r.from, r.to, schema.marks[MARKS[r.key]].create());
  }
  for (const e of edits) if (e.literal !== undefined && e.entry) e.entry.held = true;
  for (const e of edits.reverse()) {
    const end = e.pos + (e.len ?? 1);
    if (e.literal !== undefined) {
      const marks = tr.doc.resolve(e.pos + 1).marks();
      tr.replaceWith(e.pos, end, schema.text(e.literal, marks));
    } else if (e.note !== undefined) {
      // The comment keeps the formatting marks it was written under (a
      // comment at the end of link or emphasis text stays inside it).
      const marks = formatting(tr.doc.nodeAt(e.pos)?.marks ?? []);
      tr.replaceWith(e.pos, end, schema.nodes[COMMENT].create({ note: e.note }, null, marks));
    } else {
      tr.delete(e.pos, end);
    }
  }
  return { notesUsed: noteIdx, attributeBlocks, consumed };
}

interface Item {
  from: number;
  to: number;
  key: MarkKey | null;
  /** runKeyOf: neighbours with the same key but different runs are separate marks. */
  run: string | null;
  note?: string;
  /** The formatting marks of the node. */
  fmt: readonly Mark[];
}

/** A run of the serializer's walk: the mark key and its run identity. */
interface RunState {
  key: MarkKey;
  run: string;
}

/** The formatting marks of a set (not code, not critic). */
export function formatting(ms: readonly Mark[]): Mark[] {
  return ms.filter((m) => !m.type.spec.code && !CRITIC_MARK_NAMES.has(m.type.name));
}

/** Formatting marks two neighbours share (not code, not critic). */
export function sharedMarks(a: readonly Mark[], b: readonly Mark[]): Mark[] {
  const bm = formatting(b);
  return formatting(a).filter((m) => m.isInSet(bm));
}

/** The document with critic marks and comments replaced by sentinels. */
export function docToSentinels(
  doc: Node,
  schema: Schema,
  a: SentinelAlphabet
): { doc: Node; notes: string[] } {
  const openS: Record<MarkKey, string> = {
    ins: a.insOpen,
    del: a.delOpen,
    subOld: a.subOpen,
    subNew: a.subOpen + a.subSep,
    hl: a.hlOpen,
  };
  const closeS: Record<MarkKey, string> = {
    ins: a.insClose,
    del: a.delClose,
    subOld: a.subSep + a.subClose,
    subNew: a.subClose,
    hl: a.hlClose,
  };
  // A transition between runs: the closing token of the run that ends,
  // the opening token of the run that starts, or a replacement's
  // separator between its two sides.
  const transition = (
    from: RunState | null,
    to: RunState | null
  ): { close: string; open: string; sep: string } => {
    if (from?.run === to?.run) return { close: '', open: '', sep: '' };
    if (from?.key === 'subOld' && to?.key === 'subNew')
      return { close: '', open: '', sep: a.subSep };
    return { close: from ? closeS[from.key] : '', open: to ? openS[to.key] : '', sep: '' };
  };
  const blocks: { end: number; items: Item[] }[] = [];
  doc.descendants((node, pos) => {
    if (!node.isTextblock) return true;
    const items: Item[] = [];
    node.forEach((child, off) => {
      const from = pos + 1 + off;
      if (child.type.name === COMMENT) {
        items.push({
          from,
          to: from + 1,
          key: null,
          run: null,
          note: String(child.attrs.note),
          fmt: formatting(child.marks),
        });
      } else {
        items.push({
          from,
          to: from + child.nodeSize,
          key: markKeyOf(child.marks),
          run: runKeyOf(child.marks),
          fmt: formatting(child.marks),
        });
      }
    });
    blocks.push({ end: pos + 1 + node.content.size, items });
    return false;
  });
  // Tokens to insert at each position: closing ones go under the
  // formatting of the text before them (inside the run they close),
  // opening ones under the formatting of the text after them, so a mark
  // at the edge of link or emphasis text keeps its tokens inside it.
  // The formatting each run's tokens are written under: the formatting
  // that covers the whole run and continues past one of its ends (the run
  // is inside that link or emphasis). Formatting that starts and ends with
  // the run stays inside the mark ({++*x*++}).
  const openFmt = new Map<number, readonly Mark[]>();
  const closeFmt = new Map<number, readonly Mark[]>();
  for (const b of blocks) {
    const items = b.items;
    for (let i = 0; i < items.length; i++) {
      if (!items[i].run) continue;
      let j = i;
      while (
        j + 1 < items.length &&
        items[j + 1].run !== null &&
        (items[j + 1].run === items[j].run ||
          (items[j].key === 'subOld' && items[j + 1].key === 'subNew'))
      ) {
        j++;
      }
      const all = items[i].fmt.filter((m) =>
        items.slice(i, j + 1).every((it) => m.isInSet(it.fmt))
      );
      const outside = [...(items[i - 1]?.fmt ?? []), ...(items[j + 1]?.fmt ?? [])];
      const enclosing = all.filter((m) => m.isInSet(outside));
      openFmt.set(items[i].from, enclosing);
      closeFmt.set(items[j].to, enclosing);
      i = j;
    }
  }
  const ins = new Map<number, { close: string; open: string; sep: string }>();
  const add = (pos: number, t: { close: string; open: string; sep: string }): void => {
    if (!t.close && !t.open && !t.sep) return;
    const cur = ins.get(pos) ?? { close: '', open: '', sep: '' };
    ins.set(pos, { close: cur.close + t.close, open: cur.open + t.open, sep: cur.sep + t.sep });
  };
  const notes: string[] = [];
  const comments: number[] = [];
  let state: RunState | null = null;
  const runOf = (it: Item | undefined): RunState | null =>
    it?.key && it.run ? { key: it.key, run: it.run } : null;
  blocks.forEach((b, bi) => {
    for (const it of b.items) {
      const next = runOf(it);
      add(it.from, transition(state, next));
      state = next;
      if (it.note !== undefined) {
        comments.push(it.from);
        notes.push(it.note);
      }
    }
    // A run carries into the next block only if that block starts with it.
    const next = runOf(blocks[bi + 1]?.items[0]);
    if (state !== null && next?.run !== state.run) {
      add(b.end, transition(state, null));
      state = null;
    }
  });
  const tr = new Transform(doc);
  const ops = [
    ...[...ins].map(([pos, t]) => ({ pos, t, order: 1 })),
    ...comments.map((pos) => ({ pos, t: null, order: 0 })),
  ].sort((x, y) => y.pos - x.pos || x.order - y.order);
  for (const op of ops) {
    const $p = doc.resolve(op.pos);
    if (op.t === null) {
      // A comment is written under its own formatting marks (those it had
      // at load, or those it was added under), so a comment at the edge of
      // a link or emphasis run stays on its side of the closing syntax.
      const ms = formatting(doc.nodeAt(op.pos)?.marks ?? []);
      tr.replaceWith(op.pos, op.pos + 1, schema.text(a.note, ms));
    } else {
      const before = $p.nodeBefore;
      const after = $p.nodeAfter;
      // Inserted in reverse: opening tokens, the separator, then closing ones before them.
      if (op.t.open) tr.insert(op.pos, schema.text(op.t.open, [...(openFmt.get(op.pos) ?? [])]));
      if (op.t.sep) {
        const ms = before && after ? sharedMarks(before.marks, after.marks) : [];
        tr.insert(op.pos, schema.text(op.t.sep, ms));
      }
      if (op.t.close) {
        tr.insert(op.pos, schema.text(op.t.close, [...(closeFmt.get(op.pos) ?? [])]));
      }
    }
  }
  for (const name of Object.values(MARKS)) {
    tr.removeMark(0, tr.doc.content.size, schema.marks[name]);
  }
  return { doc: tr.doc, notes };
}

/** Whether any character of the alphabet is left in the document's strings. */
export function sentinelLeft(doc: Node, a: SentinelAlphabet): boolean {
  const sentinels = sentinelSet(a);
  const has = (v: unknown): boolean =>
    typeof v === 'string' && [...v].some((c) => sentinels.has(c));
  let found = false;
  doc.descendants((node) => {
    if (found) return false;
    if ((node.isText && has(node.text)) || Object.values(node.attrs).some(has)) found = true;
    for (const m of node.marks) if (Object.values(m.attrs).some(has)) found = true;
    return !found;
  });
  return found;
}

/** A loaded document and what the sentinel pass found. */
export interface LoadedDoc {
  doc: Node;
  attributeBlocks: Set<number>;
  consumed: Consumed[];
}

/**
 * Parses Markdown with CriticMarkup into a document, or null when it
 * cannot be shown faithfully: a note not placed, or a sentinel left
 * anywhere (the load-time safety net).
 */
export function loadDocument(
  md: string,
  parse: (s: string) => Node,
  schema: Schema,
  a: SentinelAlphabet
): LoadedDoc | null {
  // The tagged form: each note sentinel carries its comment's ordinal.
  const { text, notes } = criticToSentinelText(md, a, true);
  const tr = new Transform(parse(text));
  const r = applySentinels(tr, schema, notes, a);
  if (r.notesUsed !== notes.length || sentinelLeft(tr.doc, a)) return null;
  return {
    doc: tr.doc,
    attributeBlocks: r.attributeBlocks,
    consumed: r.consumed,
  };
}

/** loadDocument's document alone (tests and checks). */
export function loadDoc(
  md: string,
  parse: (s: string) => Node,
  schema: Schema,
  a: SentinelAlphabet
): Node | null {
  return loadDocument(md, parse, schema, a)?.doc ?? null;
}

/** Serializes a document to Markdown with CriticMarkup. */
export function saveDoc(
  doc: Node,
  serialize: (d: Node) => string,
  schema: Schema,
  a: SentinelAlphabet
): string {
  const s = docToSentinels(doc, schema, a);
  return sentinelTextToCritic(serialize(s.doc), s.notes, a);
}
