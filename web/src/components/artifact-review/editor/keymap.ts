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
 * The editor's own shortcuts for what the Markdown presets' keymaps did.
 * Formatting, heading, list and code block shortcuts would change the
 * document in a way suggest mode does not record, so each one shows the
 * hint the policy gives for its class of edit and changes nothing (a line
 * break with Shift-Enter too). Tab in a table moves between cells, and
 * every other key keeps ProseMirror's base behaviour.
 */

import type { Command, EditorState } from '@milkdown/prose/state';
import { Selection, TextSelection } from '@milkdown/prose/state';

import { POLICY, containerClass } from './policy.js';
import type { EditClass, HintKind } from './policy.js';

/** The formatting and block shortcuts of the presets, by the class of edit they make. */
export const REFUSED_SHORTCUTS: Record<string, EditClass> = {
  'Mod-b': 'format-inline',
  'Mod-i': 'format-inline',
  'Mod-e': 'format-inline',
  'Mod-Alt-x': 'format-inline',
  'Mod-Alt-0': 'format-block',
  'Mod-Alt-1': 'format-block',
  'Mod-Alt-2': 'format-block',
  'Mod-Alt-3': 'format-block',
  'Mod-Alt-4': 'format-block',
  'Mod-Alt-5': 'format-block',
  'Mod-Alt-6': 'format-block',
  'Mod-Alt-7': 'format-block',
  'Mod-Alt-8': 'format-block',
  'Mod-Alt-c': 'format-block',
  'Mod-Shift-b': 'format-block',
};

function inTable(state: EditorState): boolean {
  return containerClass(state.selection.$from) === 'table';
}

function inList(state: EditorState): boolean {
  return containerClass(state.selection.$from) === 'list';
}

/** The text position at the start of the next (dir 1) or previous (dir -1) table cell, or null. */
function cellStart(state: EditorState, dir: 1 | -1): number | null {
  const $from = state.selection.$from;
  let d = $from.depth;
  while (d > 0 && !['table_cell', 'table_header'].includes($from.node(d).type.name)) d--;
  if (d === 0) return null;
  const edge = dir > 0 ? $from.after(d) : $from.before(d);
  const found = Selection.findFrom(state.doc.resolve(edge), dir, true);
  if (!found || containerClass(found.$from) !== 'table') return null;
  if (dir > 0) return found.from;
  // Moving back lands at the end of the previous cell: go to its start.
  return found.$from.start();
}

/** The review editor's keymap; refuse is told the hint of a refused shortcut. */
export function reviewKeymap(refuse: (hint: HintKind) => void): Record<string, Command> {
  const refuseAs =
    (cls: EditClass): Command =>
    () => {
      const rule = POLICY[cls];
      if (rule.action === 'refuse') refuse(rule.hint);
      return true;
    };
  const keys: Record<string, Command> = {};
  for (const [key, cls] of Object.entries(REFUSED_SHORTCUTS)) keys[key] = refuseAs(cls);
  const tab =
    (dir: 1 | -1): Command =>
    (state, dispatch) => {
      if (inList(state)) return refuseAs('list')(state);
      if (!inTable(state)) return false;
      const at = cellStart(state, dir);
      if (at !== null) dispatch?.(state.tr.setSelection(TextSelection.create(state.doc, at)));
      return true;
    };
  keys.Tab = tab(1);
  keys['Mod-]'] = tab(1);
  keys['Shift-Tab'] = tab(-1);
  keys['Mod-['] = tab(-1);
  const tableBreak: Command = (state) => (inTable(state) ? refuseAs('table')(state) : false);
  keys.Enter = tableBreak;
  keys['Mod-Enter'] = tableBreak;
  // A line break inside a paragraph is a structural suggestion, refused
  // like a paragraph break.
  keys['Shift-Enter'] = (state): boolean => refuseAs(inTable(state) ? 'table' : 'split')(state);
  return keys;
}
