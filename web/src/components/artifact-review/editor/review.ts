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

/** Review commands: add a comment, and edit or delete one. */

import type { EditorState, Transaction } from '@milkdown/prose/state';
import { NodeSelection, TextSelection } from '@milkdown/prose/state';

import { containerClass } from './policy.js';
import type { HintKind } from './policy.js';
import { COMMENT, MARKS, markKeyOf, runKeyOf } from './schema.js';
import { SKIP_TRACK } from './track.js';
import { sharedMarks } from './bridge.js';

function hasCritic(state: EditorState, from: number, to: number): boolean {
  let found = false;
  state.doc.nodesBetween(from, to, (n) => {
    if (n.type.name === COMMENT || (n.isInline && markKeyOf(n.marks))) found = true;
    return !found;
  });
  return found;
}

/** Where a new comment goes, and whether it highlights the selection. */
export interface CommentPlan {
  /** Position of the comment node. */
  at: number;
  /** The highlighted range, when the comment wraps the selection. */
  highlight: { from: number; to: number } | null;
  /** The selected text, for the note dialog's quote. */
  quote: string;
}

/**
 * Plans a comment on the selection: an inline selection inside one
 * textblock that touches no mark and no code is highlighted and the note
 * follows it ({==sel==}{>>note<<}); otherwise the note goes on its own at
 * the end of the selection. Returns the hint to show where no comment can
 * go (code).
 */
export function planComment(state: EditorState): CommentPlan | { refuse: HintKind } {
  const { from, to, $from, $to } = state.selection;
  const $at = state.doc.resolve(to);
  if (containerClass($at) === 'code' || containerClass($from) === 'code') return { refuse: 'code' };
  if (!$at.parent.isTextblock) return { refuse: 'generic' };
  const code = state.schema.marks.inlineCode;
  const inCode = (pos: number): boolean => {
    const $p = state.doc.resolve(pos);
    const before = $p.nodeBefore;
    const after = $p.nodeAfter;
    return !!(code && before && after && code.isInSet(before.marks) && code.isInSet(after.marks));
  };
  if (inCode(to)) return { refuse: 'code' };
  const quote = state.doc.textBetween(from, to, ' ');
  const canWrap =
    from < to &&
    $from.sameParent($to) &&
    $from.parent.isTextblock &&
    !hasCritic(state, from, to) &&
    !inCode(from);
  return {
    at: canWrap ? to : afterRun(state, to),
    highlight: canWrap ? { from, to } : null,
    quote,
  };
}

/**
 * Where a note placed at pos goes so it does not split a mark: pos itself,
 * or the end of the critic run pos is strictly inside (a replacement's two
 * sides are one run), after the comment of a highlight run; at the seam
 * between a highlight and its comment, after that comment.
 */
export function afterRun(state: EditorState, pos: number): number {
  const $p = state.doc.resolve(pos);
  const parent = $p.parent;
  const before = $p.nodeBefore;
  const after = $p.nodeAfter;
  if (!before || !after) return pos;
  // At the seam between a highlight and its comment: after the comment.
  if (after.type.name === COMMENT && markKeyOf(before.marks) === 'hl') return pos + 1;
  const same = (a: string | null, b: string | null): boolean =>
    !!a && !!b && (a === b || (a === 'subOld' && b === 'subNew'));
  let key = runKeyOf(before.marks);
  if (!same(key, runKeyOf(after.marks))) return pos;
  // The node after pos (the one containing it when pos is inside a text node).
  let i = $p.index();
  let end = pos - $p.textOffset + parent.child(i).nodeSize;
  key = runKeyOf(parent.child(i).marks);
  while (i + 1 < parent.childCount && same(key, runKeyOf(parent.child(i + 1).marks))) {
    i++;
    key = runKeyOf(parent.child(i).marks);
    end += parent.child(i).nodeSize;
  }
  const next = i + 1 < parent.childCount ? parent.child(i + 1) : null;
  if (markKeyOf(parent.child(i).marks) === 'hl' && next?.type.name === COMMENT) end += 1;
  return end;
}

/** The transaction adding a comment with note as planned. */
export function addComment(state: EditorState, plan: CommentPlan, note: string): Transaction {
  const schema = state.schema;
  const tr = state.tr;
  if (plan.highlight) {
    tr.addMark(
      plan.highlight.from,
      plan.highlight.to,
      schema.marks[MARKS.hl].create({ mine: true })
    );
  }
  // The comment takes the formatting marks both its neighbours share (a
  // note inside link or emphasis text stays inside it).
  const $at = tr.doc.resolve(plan.at);
  const marks =
    $at.nodeBefore && $at.nodeAfter ? sharedMarks($at.nodeBefore.marks, $at.nodeAfter.marks) : [];
  tr.insert(plan.at, schema.nodes[COMMENT].create({ note, mine: true }, null, marks));
  tr.setSelection(TextSelection.create(tr.doc, plan.at + 1));
  tr.setMeta(SKIP_TRACK, true);
  return tr.scrollIntoView();
}

/** The comment selected as a node, with its position, or null. */
export function selectedComment(state: EditorState): { pos: number; note: string } | null {
  const sel = state.selection;
  if (sel instanceof NodeSelection && sel.node.type.name === COMMENT) {
    return { pos: sel.from, note: String(sel.node.attrs.note) };
  }
  return null;
}

/** The 1-based number of the comment at pos, in document order. */
export function commentNumber(state: EditorState, pos: number): number {
  let n = 0;
  let found = 0;
  state.doc.descendants((node, p) => {
    if (found) return false;
    if (node.type.name === COMMENT) {
      n++;
      if (p === pos) found = n;
    }
    return true;
  });
  return found;
}

/**
 * The transaction setting the note of the comment at pos. An empty note
 * deletes the comment, and the highlight right before it goes too.
 */
export function editComment(state: EditorState, pos: number, note: string): Transaction | null {
  const node = state.doc.nodeAt(pos);
  if (node?.type.name !== COMMENT) return null;
  const tr = state.tr;
  if (note.trim() === '') {
    tr.delete(pos, pos + 1);
    const $p = tr.doc.resolve(pos);
    const hl = state.schema.marks[MARKS.hl];
    let start = pos;
    const parentStart = $p.start();
    // Walk back over this session's own highlight run that ends at the
    // comment (an earlier reviewer's highlight stays).
    while (start > parentStart) {
      const before = tr.doc.resolve(start).nodeBefore;
      const m = before ? hl.isInSet(before.marks) : undefined;
      if (!m || m.attrs.mine !== true) break;
      start -= before!.nodeSize;
    }
    if (start < pos) tr.removeMark(start, pos, hl);
  } else {
    tr.setNodeMarkup(pos, undefined, { ...node.attrs, note });
  }
  tr.setMeta(SKIP_TRACK, true);
  return tr;
}
