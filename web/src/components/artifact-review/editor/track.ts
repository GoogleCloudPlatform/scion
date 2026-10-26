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
 * Track changes: every edit inside one textblock is rewritten into
 * suggestion marks.
 *
 * appendTransaction sees each user transaction after it applied. For a
 * single replace step inside one textblock it undoes the step and redoes
 * it as a suggestion in the same history event, so one undo reverts both:
 *
 * - deleted plain or highlighted text stays, marked as a deletion (or as a
 *   replacement's old side when the step also inserts text);
 * - deleted text of the reviewer's own pending insertion (or a
 *   replacement's new side, or a comment, added in this session) really
 *   goes; an earlier reviewer's is never removed by typing (refused);
 * - already deleted text is left alone;
 * - inserted text goes after the deleted range, marked as an insertion, or
 *   as a replacement's new side when it replaces text or continues one.
 *   Equal adjacent marks merge, so typing inside one's own suggestion
 *   extends that suggestion; marks never nest.
 * - an edit inside an unmarked inline code span becomes a replacement of
 *   the whole span (old span, then new span), since a mark inside code
 *   would be read as literal text.
 *
 * policy.ts decides which transactions reach this plugin at all.
 */

import { Fragment } from '@milkdown/prose/model';
import type { Node, ResolvedPos, Schema } from '@milkdown/prose/model';
import { Plugin, PluginKey, TextSelection } from '@milkdown/prose/state';
import type { EditorState, Transaction } from '@milkdown/prose/state';
import { ReplaceStep, Transform } from '@milkdown/prose/transform';
import type { EditorView } from '@milkdown/prose/view';

import { COMMENT, CRITIC_MARK_NAMES, MARKS, isMine, markKeyOf } from './schema.js';

/** Meta of transactions the tracker must not rewrite (its own, and commands'). */
export const SKIP_TRACK = 'criticSkipTrack';
/** Meta of the transaction that loads a document. */
export const LOAD_META = 'criticLoad';

export const trackKey = new PluginKey('criticTrack');

/** The inline content of a slice that stays inside one textblock, or null. */
export function inlineContent(slice: {
  content: Fragment;
  openStart: number;
  openEnd: number;
}): Fragment | null {
  if (slice.openStart !== slice.openEnd) return null;
  let frag = slice.content;
  for (let d = 0; d < slice.openStart; d++) {
    if (frag.childCount !== 1) return null;
    frag = frag.firstChild!.content;
  }
  let ok = true;
  frag.forEach((n) => {
    if (!n.isInline) ok = false;
  });
  return ok ? frag : null;
}

function stripCritic(frag: Fragment, schema: Schema): Node[] {
  const out: Node[] = [];
  frag.forEach((n) => {
    let marks = n.marks;
    for (const name of CRITIC_MARK_NAMES) marks = schema.marks[name].removeFromSet(marks);
    out.push(n.mark(marks));
  });
  return out;
}

/** The result of tracking one step: the steps on top of the edited doc and the new cursor. */
export interface Tracked {
  transform: Transform;
  head: number;
}

/**
 * The unmarked inline code run around [from, to] of $from's textblock, or
 * null when the edit is not inside one.
 */
function codeRun(
  $from: ResolvedPos,
  from: number,
  to: number,
  schema: Schema
): { from: number; to: number } | null {
  const code = schema.marks.inlineCode;
  if (!code) return null;
  const start = $from.start();
  const runs: { from: number; to: number }[] = [];
  $from.parent.forEach((child, off) => {
    const a = start + off;
    const b = a + child.nodeSize;
    if (!(child.isText && code.isInSet(child.marks) && markKeyOf(child.marks) === null)) return;
    const last = runs[runs.length - 1];
    if (last && last.to === a) last.to = b;
    else runs.push({ from: a, to: b });
  });
  for (const r of runs) {
    if (r.from > from || to > r.to) continue;
    // Typing at the very edge of a span is outside it (inline code is not inclusive).
    if (from === to && (from === r.from || from === r.to)) continue;
    return r;
  }
  return null;
}

/**
 * Rewrites the replace step (from oldDoc to newDoc) as a suggestion.
 * Returns null when the step is not an edit inside one textblock.
 */
export function trackStep(
  step: ReplaceStep,
  oldDoc: Node,
  newDoc: Node,
  cursor: { empty: boolean; head: number }
): Tracked | null {
  const s = step as ReplaceStep & {
    from: number;
    to: number;
    structure: boolean;
  };
  if (s.structure) return null;
  const { from, to, slice } = s;
  const schema = oldDoc.type.schema;
  const insFrag = slice.size ? inlineContent(slice) : null;
  if (slice.size && !insFrag) return null;
  const $from = oldDoc.resolve(from);
  const $to = oldDoc.resolve(to);
  if (!$from.sameParent($to) || !$from.parent.isTextblock) return null;
  if (!insFrag && from === to) return null;

  const out = new Transform(newDoc);
  out.step(step.invert(oldDoc));
  const fwd = (): ReturnType<Transform['mapping']['slice']> => out.mapping.slice(1);

  const run = codeRun($from, from, to, schema);
  if (run) {
    // Old span becomes the replacement's old side; the edited span its new side.
    const oldSpan = oldDoc.slice(run.from, run.to).content;
    const edited = oldDoc
      .slice(run.from, from)
      .content.append(insFrag ?? Fragment.empty)
      .append(oldDoc.slice(to, run.to).content);
    out.addMark(run.from, run.to, schema.marks[MARKS.subOld].create({ mine: true }));
    const subNew = schema.marks[MARKS.subNew].create({ mine: true });
    const nodes: Node[] = [];
    edited.forEach((n) => {
      if (n.isText) nodes.push(n.mark(subNew.addToSet(n.marks)));
    });
    if (nodes.length === 0 && oldSpan.size === 0) return null;
    out.insert(run.to, nodes);
    const head = run.to + nodes.reduce((sum, n) => sum + n.nodeSize, 0);
    return { transform: out, head };
  }

  // Classify the deleted range. Only this session's own insertions and
  // comments really go; an earlier suggestion or comment is not removed by
  // typing (the edit is refused).
  const realDeletes: { from: number; to: number }[] = [];
  const toMark: { from: number; to: number }[] = [];
  let foreign = false;
  oldDoc.nodesBetween(from, to, (node, pos) => {
    if (!node.isInline) return true;
    const a = Math.max(pos, from);
    const b = Math.min(pos + node.nodeSize, to);
    if (b <= a) return false;
    if (node.type.name === COMMENT) {
      if (node.attrs.mine === true) realDeletes.push({ from: a, to: b });
      else foreign = true;
      return false;
    }
    const k = markKeyOf(node.marks);
    if (k === 'ins' || k === 'subNew') {
      if (isMine(node.marks)) realDeletes.push({ from: a, to: b });
      else foreign = true;
    } else if (k === null || k === 'hl') {
      toMark.push({ from: a, to: b });
    }
    return false;
  });
  if (foreign) return null;

  const replacing = !!insFrag && toMark.length > 0;
  const delType = schema.marks[replacing ? MARKS.subOld : MARKS.del];
  for (const r of toMark) {
    out.removeMark(r.from, r.to, schema.marks[MARKS.hl]);
    out.addMark(r.from, r.to, delType.create({ mine: true }));
  }

  let insEnd = -1;
  let afterIns = 0;
  if (insFrag) {
    let key: 'ins' | 'subNew' = 'ins';
    if (replacing) {
      key = 'subNew';
    } else {
      // Continue only this session's own replacement.
      const own = (n: Node | null | undefined): string | null =>
        n && isMine(n.marks) ? markKeyOf(n.marks) : null;
      const before = own($from.nodeBefore);
      const after = own($to.nodeAfter);
      if (before === 'subNew' || after === 'subNew' || before === 'subOld') key = 'subNew';
    }
    const mark = schema.marks[MARKS[key]].create({ mine: true });
    const nodes = stripCritic(insFrag, schema).map((n) => n.mark(mark.addToSet(n.marks)));
    const at = fwd().map(to);
    out.insert(at, nodes);
    insEnd = at + nodes.reduce((sum, n) => sum + n.nodeSize, 0);
    afterIns = out.steps.length;
  }
  for (const r of realDeletes.sort((x, y) => y.from - x.from)) {
    const m = fwd();
    out.delete(m.map(r.from, -1), m.map(r.to, -1));
  }

  let head: number;
  if (insEnd >= 0) {
    head = out.mapping.slice(afterIns).map(insEnd);
  } else {
    const forward = cursor.empty && cursor.head === from;
    head = fwd().map(forward ? to : from, forward ? 1 : -1);
  }
  return { transform: out, head: Math.min(head, out.doc.content.size) };
}

/** Copies a Tracked result onto a transaction of the state it was computed on. */
function applyTracked(tr: Transaction, t: Tracked): Transaction {
  for (const step of t.transform.steps) tr.step(step);
  tr.setSelection(TextSelection.create(tr.doc, t.head));
  tr.setMeta(SKIP_TRACK, true);
  return tr;
}

/** The single replace step of a user transaction, or null. */
export function singleReplace(tr: Transaction): ReplaceStep | null {
  if (tr.steps.length !== 1 || !(tr.steps[0] instanceof ReplaceStep)) return null;
  return tr.steps[0];
}

/** Whether a transaction is one the tracker leaves alone. */
export function isExempt(tr: Transaction): boolean {
  return (
    !tr.docChanged ||
    !!tr.getMeta(SKIP_TRACK) ||
    !!tr.getMeta(LOAD_META) ||
    tr.getMeta('history$') !== undefined ||
    tr.getMeta('composition') !== undefined
  );
}

/**
 * Tracked results the policy computed while deciding a transaction, so the
 * tracker does not compute them again when it applies.
 */
const decided = new WeakMap<Transaction, Tracked>();

/** Tracks a user transaction against the state it applies to, or null. */
export function trackTransaction(tr: Transaction, before: EditorState): Tracked | null {
  const known = decided.get(tr);
  if (known) return known;
  const step = singleReplace(tr);
  if (!step) return null;
  const sel = before.selection;
  const t = trackStep(step, before.doc, tr.doc, { empty: sel.empty, head: sel.head });
  if (t) decided.set(tr, t);
  return t;
}

/** The tracking plugin. */
export function trackPlugin(): Plugin {
  return new Plugin({
    key: trackKey,
    appendTransaction(trs, oldState, newState): Transaction | null {
      if (trs.length !== 1) return null;
      const tr = trs[0];
      if (isExempt(tr)) return null;
      const t = trackTransaction(tr, oldState);
      return t ? applyTracked(newState.tr, t) : null;
    },
  });
}

/**
 * Where two fragments differ: [start, endNow) in now replaces [start,
 * endOld) in old. findDiffEnd can report ends before findDiffStart when
 * the changed text repeats its neighbour ("item" to "ittem"); both ends
 * are then moved past start by the same amount. Null when equal.
 */
export function diffRange(
  now: Fragment,
  old: Fragment
): { start: number; endNow: number; endOld: number } | null {
  const start = now.findDiffStart(old);
  if (start === null) return null;
  const end = now.findDiffEnd(old);
  if (!end) return null;
  let endNow = end.a;
  let endOld = end.b;
  const overlap = start - Math.min(endNow, endOld);
  if (overlap > 0) {
    endNow += overlap;
    endOld += overlap;
  }
  return { start, endNow, endOld };
}

/** The state before a composition: the whole document and the textblock at the selection. */
export interface CompositionSnapshot {
  doc: Node;
  /** The textblock at the selection and where it starts, or null when the selection is not in one. */
  block: { pos: number; node: Node } | null;
}

/** Snapshots the state before a composition, to compare with after it. */
export function snapshotComposition(state: EditorState): CompositionSnapshot {
  const $from = state.selection.$from;
  return {
    doc: state.doc,
    block: $from.parent.isTextblock ? { pos: $from.before(), node: $from.parent } : null,
  };
}

/**
 * After a composition (IME, dictation, autocorrect on some keyboards),
 * which neither the policy nor the tracker touches while it runs, records
 * the composed text as an insertion. check vets the result (the block's
 * container, and that it would save); it returns a refusal reason or null.
 * Returns null when the composition was recorded (or changed nothing), or
 * the reason it was not; the document is then restored to its state
 * before the composition. The transaction carries the composition's id,
 * so history groups it with the composition: one undo reverts both.
 */
export function reconcileComposition(
  view: EditorView,
  snap: CompositionSnapshot,
  compositionId: unknown,
  check: (doc: Node, blockPos: number, at: number, before: Node) => string | null = () => null
): string | null {
  const state = view.state;
  const tag = (tr: Transaction): Transaction => {
    tr.setMeta(SKIP_TRACK, true);
    if (compositionId !== undefined && compositionId !== null)
      tr.setMeta('composition', compositionId);
    return tr;
  };
  const restore = (why: string): string => {
    const d = diffRange(state.doc.content, snap.doc.content);
    if (d)
      view.dispatch(tag(state.tr.replace(d.start, d.endNow, snap.doc.slice(d.start, d.endOld))));
    return why;
  };
  if (state.doc.eq(snap.doc)) return null;
  const block = snap.block;
  if (!block) return restore('generic');
  const now = state.doc.nodeAt(block.pos);
  if (!now || now.type !== block.node.type) return restore('generic');
  // Everything outside the snapshot block must be unchanged.
  const size = block.node.nodeSize;
  const delta = now.nodeSize - size;
  if (
    !state.doc.slice(0, block.pos).eq(snap.doc.slice(0, block.pos)) ||
    !state.doc.slice(block.pos + size + delta).eq(snap.doc.slice(block.pos + size))
  ) {
    return restore('generic');
  }
  const d = diffRange(now.content, block.node.content);
  if (!d) return null;
  const { start, endNow, endOld } = d;
  // Only insertions are recorded in this version: a composition that
  // removed text (or replaced a selection) is undone.
  if (endOld > start) return restore('generic');
  const base = block.pos + 1;
  // Restore the block, then apply the composed text as one insertion.
  const revert = state.tr.replaceWith(base, base + now.content.size, block.node.content);
  const oldDoc = revert.doc;
  const step = new ReplaceStep(base + start, base + start, now.slice(start, endNow));
  const applied = step.apply(oldDoc);
  if (!applied.doc) return restore('generic');
  const t = trackStep(step, oldDoc, applied.doc, { empty: true, head: base + start });
  if (!t) return restore('generic');
  const refused = check(t.transform.doc, block.pos, base + start, oldDoc);
  if (refused) return restore(refused);
  revert.step(step);
  for (const st of t.transform.steps) revert.step(st);
  revert.setSelection(TextSelection.create(revert.doc, t.head));
  view.dispatch(tag(revert));
  return null;
}
