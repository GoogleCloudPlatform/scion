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
 * CriticMarkup in the editor's schema: one flat mark per suggestion kind
 * and an inline atom node for comments. These never come from or go to the
 * Markdown tree directly: bridge.ts turns them into sentinel characters
 * before the serializer runs and back after the parser runs.
 */

import { InitReady, marksCtx, schemaTimerCtx } from '@milkdown/core';
import { createTimer } from '@milkdown/ctx';
import type { MilkdownPlugin } from '@milkdown/ctx';
import type { DOMOutputSpec, Mark, Node } from '@milkdown/prose/model';
import { $markSchema, $nodeSchema } from '@milkdown/utils';

export const MARKS = {
  ins: 'critic_ins',
  del: 'critic_del',
  subOld: 'critic_sub_old',
  subNew: 'critic_sub_new',
  hl: 'critic_hl',
} as const;
export type MarkKey = keyof typeof MARKS;
export const MARK_KEYS = Object.keys(MARKS) as MarkKey[];
export const CRITIC_MARK_NAMES: ReadonlySet<string> = new Set(Object.values(MARKS));
export const COMMENT = 'critic_comment';

const MARK_LABEL: Record<MarkKey, string> = {
  ins: 'Suggested insertion',
  del: 'Suggested deletion',
  subOld: 'Suggested replacement, old text',
  subNew: 'Suggested replacement, new text',
  hl: 'Highlighted text',
};

const MARK_TAG: Record<MarkKey, string> = {
  ins: 'ins',
  del: 'del',
  subOld: 'del',
  subNew: 'ins',
  hl: 'mark',
};

const never = { match: (): boolean => false, runner: (): void => {} };

function criticMark(key: MarkKey): ReturnType<typeof $markSchema> {
  const id = MARKS[key];
  const cls = id.replace(/_/g, '-');
  return $markSchema(id, () => ({
    group: 'critic',
    // Marks do not nest: a span of text carries at most one critic mark.
    excludes: 'critic',
    inclusive: false,
    // mine: added in this editing session (not saved; marks read from the
    // text are someone's earlier suggestions).
    attrs: { mine: { default: false, validate: 'boolean' } },
    parseDOM: [],
    toDOM: () => [MARK_TAG[key], { class: cls, title: MARK_LABEL[key] }, 0],
    parseMarkdown: never,
    // bridge.ts removes these marks before serializing.
    toMarkdown: { match: (m: Mark): boolean => m.type.name === id, runner: (): void => {} },
  }));
}

export const insMark = criticMark('ins');
export const delMark = criticMark('del');
export const subOldMark = criticMark('subOld');
export const subNewMark = criticMark('subNew');
export const hlMark = criticMark('hl');

/**
 * A comment: its note is plain text, shown in the badge's label and in the
 * comment card, never read as Markdown. mine marks a comment added in this
 * session (it is not saved).
 */
export const commentNode = $nodeSchema(COMMENT, () => ({
  group: 'inline',
  inline: true,
  atom: true,
  selectable: true,
  attrs: {
    note: { default: '', validate: 'string' },
    mine: { default: false, validate: 'boolean' },
  },
  parseDOM: [],
  toDOM: (node: Node): DOMOutputSpec => [
    'span',
    {
      class: 'critic-comment',
      role: 'note',
      tabindex: '0',
      'aria-label': `Comment: ${String(node.attrs.note)}`,
    },
    ['sl-icon', { name: 'chat-text', 'aria-hidden': 'true' }],
  ],
  parseMarkdown: never,
  toMarkdown: { match: (n: Node): boolean => n.type.name === COMMENT, runner: (): void => {} },
}));

/** The GFM strikethrough mark (preset-gfm). */
export const STRIKE = 'strike_through';

const CriticRanked = createTimer('CriticRanked');

/**
 * Places the critic marks just before strikethrough in the schema, so a
 * suggestion is the outer element of struck text it covers (and of text
 * struck inside it): a suggested deletion of struck text reads as a
 * deletion first. Every other mark keeps its place.
 */
export const criticRank: MilkdownPlugin = (ctx) => {
  ctx.record(CriticRanked);
  ctx.update(schemaTimerCtx, (timers) => [...timers, CriticRanked]);
  return async (): Promise<void> => {
    await ctx.wait(InitReady);
    ctx.update(marksCtx, (marks) => {
      const critic = marks.filter(([name]) => CRITIC_MARK_NAMES.has(name));
      const rest = marks.filter(([name]) => !CRITIC_MARK_NAMES.has(name));
      const at = rest.findIndex(([name]) => name === STRIKE);
      // No strikethrough mark (a renamed preset mark): nothing to order.
      // gfm-display.test.ts guards this by checking del.critic-del > s.gfm-strike.
      if (at < 0) return marks;
      return [...rest.slice(0, at), ...critic, ...rest.slice(at)];
    });
    ctx.done(CriticRanked);
  };
};

export const criticSchema = [
  insMark,
  delMark,
  subOldMark,
  subNewMark,
  hlMark,
  commentNode,
  criticRank,
].flat();

/** Whether the critic mark of a set of marks was added in this session. */
export function isMine(marks: readonly Mark[]): boolean {
  for (const m of marks) if (CRITIC_MARK_NAMES.has(m.type.name)) return m.attrs.mine === true;
  return false;
}

/**
 * The run identity of a node's critic mark: its key, plus whether it is
 * this session's for insertions, deletions and highlights, so that a new
 * suggestion next to an earlier one stays a separate mark. A replacement's
 * two sides are always one unit.
 */
export function runKeyOf(marks: readonly Mark[]): string | null {
  const k = markKeyOf(marks);
  if (k === null) return null;
  if (k === 'subOld' || k === 'subNew') return k;
  return isMine(marks) ? `${k}:mine` : k;
}

/** The critic mark key of a set of marks, or null. */
export function markKeyOf(marks: readonly Mark[]): MarkKey | null {
  for (const m of marks) {
    for (const k of MARK_KEYS) if (MARKS[k] === m.type.name) return k;
  }
  return null;
}
