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
 * Which edits the review editor records as suggestions, and which it
 * refuses with a hint pointing to the source view.
 *
 * Every transaction that changes the document is put in a class, and
 * POLICY maps the class to an action. Adding a kind of suggestion later
 * (paragraph breaks, formatting as replacements) is a new table entry and
 * a tracker, not a new set of checks.
 */

import type { Node, ResolvedPos } from '@milkdown/prose/model';
import { Plugin, PluginKey } from '@milkdown/prose/state';
import type { EditorState, Transaction } from '@milkdown/prose/state';
import {
  AddMarkStep,
  AttrStep,
  RemoveMarkStep,
  ReplaceAroundStep,
  ReplaceStep,
} from '@milkdown/prose/transform';

import { cleanUnchanged } from './guard.js';
import { COMMENT, isMine, markKeyOf, runKeyOf } from './schema.js';
import { inlineContent, isExempt, trackTransaction } from './track.js';

/** The hint shown when an edit is refused. */
export type HintKind =
  | 'format'
  | 'code'
  | 'image'
  | 'list-table'
  | 'break'
  | 'paste-multi'
  | 'earlier'
  | 'moved'
  | 'code-run'
  | 'generic';

/**
 * Classes of document-changing transactions. 'list' and 'table' are
 * changes to the structure of a list or table (items, rows, cells, their
 * nesting); text edits inside an item or a cell are 'inline'.
 */
export type EditClass =
  | 'inline'
  | 'split'
  | 'join'
  | 'cross-block'
  | 'format-inline'
  | 'format-block'
  | 'list'
  | 'table'
  | 'code'
  | 'html'
  | 'other';

export type PolicyAction = { action: 'track' } | { action: 'refuse'; hint: HintKind };

/** What suggest mode does with each class of edit. */
export const POLICY: Record<EditClass, PolicyAction> = {
  inline: { action: 'track' },
  split: { action: 'refuse', hint: 'break' },
  join: { action: 'refuse', hint: 'break' },
  'cross-block': { action: 'refuse', hint: 'break' },
  'format-inline': { action: 'refuse', hint: 'format' },
  'format-block': { action: 'refuse', hint: 'format' },
  list: { action: 'refuse', hint: 'list-table' },
  table: { action: 'refuse', hint: 'list-table' },
  code: { action: 'refuse', hint: 'code' },
  html: { action: 'refuse', hint: 'generic' },
  other: { action: 'refuse', hint: 'generic' },
};

const LIST_NODES = new Set(['bullet_list', 'ordered_list', 'list_item']);
const TABLE_NODES = new Set([
  'table',
  'table_row',
  'table_header_row',
  'table_cell',
  'table_header',
]);

/** The container class of a position: list, table, code, or null. */
export function containerClass($pos: ResolvedPos): EditClass | null {
  if ($pos.parent.type.spec.code) return 'code';
  for (let d = $pos.depth; d > 0; d--) {
    const name = $pos.node(d).type.name;
    if (TABLE_NODES.has(name)) return 'table';
    if (LIST_NODES.has(name)) return 'list';
  }
  return null;
}

function touchesHtml(doc: Node, from: number, to: number, inserted: Node[]): boolean {
  let found = inserted.some((n) => n.type.name === 'html');
  if (!found && to > from) {
    doc.nodesBetween(from, to, (n) => {
      if (n.type.name === 'html') found = true;
      return !found;
    });
  }
  return found;
}

/** Puts a document-changing transaction in a class. */
export function classify(tr: Transaction, before: EditorState): EditClass {
  const doc = before.doc;
  let markOnly = tr.steps.length > 0;
  for (const s of tr.steps) {
    if (!(s instanceof AddMarkStep || s instanceof RemoveMarkStep)) markOnly = false;
  }
  if (markOnly) return 'format-inline';
  if (tr.steps.some((s) => s instanceof ReplaceAroundStep || s instanceof AttrStep)) {
    const $from = doc.resolve(tr.selection.from);
    return (
      containerClass(doc.resolve(before.selection.from)) ?? containerClass($from) ?? 'format-block'
    );
  }
  if (tr.steps.length !== 1 || !(tr.steps[0] instanceof ReplaceStep)) {
    if (tr.steps.some((s) => s instanceof AddMarkStep || s instanceof RemoveMarkStep)) {
      return 'format-inline';
    }
    return 'other';
  }
  const step = tr.steps[0] as ReplaceStep & { from: number; to: number; structure: boolean };
  const $from = doc.resolve(step.from);
  const $to = doc.resolve(step.to);
  if ($from.parent.type.spec.code || $to.parent.type.spec.code) return 'code';
  // Text edits inside one list item's or table cell's paragraph are inline
  // edits like any other; changing the structure of a list or table is not.
  const container = containerClass($from) ?? containerClass($to);
  if (!$from.sameParent($to) || !$from.parent.isTextblock) {
    if (container) return container;
    return $from.parent.isTextblock && $to.parent.isTextblock ? 'join' : 'cross-block';
  }
  const frag = step.slice.size ? inlineContent(step.slice) : null;
  if (step.slice.size && !frag) return container ?? 'split';
  const inserted: Node[] = [];
  frag?.forEach((n) => inserted.push(n));
  if (touchesHtml(doc, step.from, step.to, inserted)) return 'html';
  return 'inline';
}

/**
 * Checks that the top-level blocks first..last of doc would save as text
 * the hub accepts (same clean text as their source, marks read back,
 * earlier marks kept in their place): null, or the hint for the refusal.
 */
export type UnitCheck = (doc: Node, first: number, last: number) => HintKind | null;

/**
 * Whether a replace from..to (in doc) would change a mark or comment of an
 * earlier review: it deletes an earlier insertion, replacement new side,
 * highlighted text or comment, or it inserts inside an earlier mark's run
 * (between the two sides of an earlier replacement included, and between
 * an earlier highlight and the comment after it). Deleting
 * text an earlier review already marked as deleted changes nothing.
 */
export function touchesEarlier(doc: Node, from: number, to: number): boolean {
  let hit = false;
  if (to > from) {
    doc.nodesBetween(from, to, (n) => {
      if (hit || !n.isInline) return !hit;
      if (n.type.name === COMMENT) {
        if (n.attrs.mine !== true) hit = true;
        return false;
      }
      const k = markKeyOf(n.marks);
      if ((k === 'ins' || k === 'subNew' || k === 'hl') && !isMine(n.marks)) hit = true;
      return false;
    });
    if (hit) return true;
  }
  const $a = doc.resolve(from);
  const $b = doc.resolve(to);
  const before = $a.nodeBefore;
  const after = $b.nodeAfter;
  if (!before || !after || !$a.parent.isTextblock) return false;
  // An earlier highlight and its comment are one unit.
  if (after.type.name === COMMENT && markKeyOf(before.marks) === 'hl' && !isMine(before.marks)) {
    return true;
  }
  const kb = runKeyOf(before.marks);
  const ka = runKeyOf(after.marks);
  if (!kb || !ka || isMine(before.marks) || isMine(after.marks)) return false;
  return kb === ka || (kb === 'subOld' && ka === 'subNew');
}

/**
 * The hint for an edit refused because it would change a mark the
 * reviewer did not add, by top-level block: 'earlier', or 'generic' where
 * the editor could not tell the reviewer's own marks from earlier ones.
 */
export type EarlierHint = (block: number) => HintKind;

/** Decides a transaction: allow it, or refuse it with a hint. */
export function decide(
  tr: Transaction,
  before: EditorState,
  unitCheck: UnitCheck = () => null,
  earlierHint: EarlierHint = () => 'earlier'
): HintKind | null {
  if (isExempt(tr)) return null;
  const cls = classify(tr, before);
  const rule = POLICY[cls];
  if (rule.action === 'refuse') return rule.hint;
  // A tracked class is a single ReplaceStep (classify).
  const step = tr.steps[0] as ReplaceStep & { from: number; to: number };
  if (touchesEarlier(before.doc, step.from, step.to)) {
    return earlierHint(before.doc.resolve(step.from).index(0));
  }
  // The edit will be tracked: check that the tracked result keeps the clean reading.
  const tracked = trackTransaction(tr, before);
  if (!tracked) return 'generic';
  const after = tracked.transform.doc;
  if (!cleanUnchanged(before.doc, after, step.from, step.to)) return 'generic';
  const first = before.doc.resolve(step.from).index(0);
  const last = before.doc.resolve(step.to).index(0);
  return unitCheck(after, first, last);
}

export const policyKey = new PluginKey('criticPolicy');

/** The suggest-mode filter. onRefuse is told the hint of each refused edit. */
export function policyPlugin(
  onRefuse: (hint: HintKind) => void,
  unitCheck?: UnitCheck,
  earlierHint?: EarlierHint
): Plugin {
  return new Plugin({
    key: policyKey,
    filterTransaction(tr, state): boolean {
      const hint = decide(tr, state, unitCheck, earlierHint);
      if (hint === null) return true;
      onRefuse(hint);
      return false;
    },
  });
}
