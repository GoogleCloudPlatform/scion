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
 * Editing a comment from its badge (ptone/scion#6850; design 4.4 E5/E6 and
 * 4.5): a single click on the reviewer's own comment badge opens the note
 * dialog with the note filled in, under the same save rules as a new
 * comment; an earlier comment stays read-only and says so; a click in the
 * highlighted text only places the caret; a double-click opens one dialog.
 */

import { afterEach, describe, expect, it } from 'vitest';
import { TextSelection } from '@milkdown/prose/state';
import type { Node } from '@milkdown/prose/model';

import { ReviewController } from './controller.js';
import type { NoteRequest } from './controller.js';
import type { HintKind } from './policy.js';
import { renderContext } from './__fixtures__/core.js';

const made: ReviewController[] = [];
afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  document.body.replaceChildren();
});

interface Harness {
  c: ReviewController;
  hints: HintKind[];
  asked: NoteRequest[];
  /** The answer the note dialog gives next; a pending gate holds it open. */
  answer: { note: string | null; gate?: Promise<void> | undefined };
}

async function open(md: string): Promise<Harness> {
  const root = document.createElement('div');
  document.body.append(root);
  const hints: HintKind[] = [];
  const asked: NoteRequest[] = [];
  const answer: Harness['answer'] = { note: 'a note' };
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: async (req) => {
      asked.push(req);
      if (answer.gate) await answer.gate;
      return answer.note;
    },
    onHint: (h) => hints.push(h),
    onChange: () => {},
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return { c, hints, asked, answer };
}

const text = (c: ReviewController): string => c.flush()!;

function posOf(c: ReviewController, needle: string): number {
  let found = -1;
  c.view.state.doc.descendants((n, pos) => {
    if (found >= 0) return false;
    if (n.isText && n.text!.includes(needle)) found = pos + n.text!.indexOf(needle);
    return true;
  });
  if (found < 0) throw new Error(`no ${needle}`);
  return found;
}

function select(c: ReviewController, from: number, to = from): void {
  const { view } = c;
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

function comments(c: ReviewController): number[] {
  const out: number[] = [];
  c.view.state.doc.descendants((n, q) => {
    if (n.type.name === 'critic_comment') out.push(q);
    return true;
  });
  return out;
}

function click(button = 0, init: MouseEventInit = {}): MouseEvent {
  return new MouseEvent('click', { bubbles: true, cancelable: true, button, ...init });
}

/** Runs the editor's handleClickOn as ProseMirror does for a click on node at nodePos. */
function clickOn(c: ReviewController, nodePos: number, node: Node, e = click()): boolean {
  return !!c.view.someProp('handleClickOn', (f) => f(c.view, nodePos, node, nodePos, e, true));
}

function doubleClickOn(c: ReviewController, nodePos: number, node: Node): boolean {
  return !!c.view.someProp('handleDoubleClickOn', (f) =>
    f(c.view, nodePos, node, nodePos, click(), true)
  );
}

const settle = async (): Promise<void> => {
  for (let i = 0; i < 4; i++) await Promise.resolve();
};

/** One own comment on "two", noted "old". */
async function withOwnComment(md = 'one two three\n'): Promise<Harness & { pos: number }> {
  const h = await open(md);
  const p = posOf(h.c, 'two');
  select(h.c, p, p + 3);
  h.answer.note = 'old';
  await h.c.comment();
  expect(text(h.c)).toBe(md.replace('two', '{==two==}{>>old<<}'));
  return { ...h, pos: comments(h.c)[0] };
}

describe('editing a comment from its badge (ptone/scion#6850)', () => {
  it('a single click on an own badge opens the dialog with the note, and saving changes only that note', async () => {
    const { c, asked, answer, pos } = await withOwnComment();
    answer.note = 'new';
    const node = c.view.state.doc.nodeAt(pos)!;
    expect(clickOn(c, pos, node)).toBe(true);
    await settle();
    expect(asked).toHaveLength(2);
    expect(asked[1]).toMatchObject({ note: 'old', number: 1 });
    expect(text(c)).toBe('one {==two==}{>>new<<} three\n');
  });

  it('an empty save removes the comment and its highlight', async () => {
    const { c, answer, pos } = await withOwnComment();
    answer.note = '';
    clickOn(c, pos, c.view.state.doc.nodeAt(pos)!);
    await settle();
    expect(text(c)).toBe('one two three\n');
  });

  it('a click on an earlier badge opens nothing and says it is read-only; an own one beside it opens', async () => {
    const { c, asked, hints, answer, pos } = await withOwnComment('one two {>>earlier<<}\n');
    const earlier = comments(c).find((q) => q !== pos)!;
    expect(c.view.state.doc.nodeAt(earlier)?.attrs.mine).toBe(false);
    clickOn(c, earlier, c.view.state.doc.nodeAt(earlier)!);
    await settle();
    expect(asked).toHaveLength(1);
    expect(hints).toEqual(['earlier']);
    // Positive control in the same document: the own comment opens.
    answer.note = 'mine';
    clickOn(c, pos, c.view.state.doc.nodeAt(pos)!);
    await settle();
    expect(asked).toHaveLength(2);
  });

  it('after a switch to Source and back, a click on the formerly own badge opens nothing', async () => {
    const { c, asked, hints } = await withOwnComment();
    // Graphical -> Source -> Graphical: the text is opened again; every mark is earlier (E6).
    expect(c.load(c.flush()!)).toBe(true);
    const [pos] = comments(c);
    expect(c.view.state.doc.nodeAt(pos)?.attrs.mine).toBe(false);
    clickOn(c, pos, c.view.state.doc.nodeAt(pos)!);
    await settle();
    expect(asked).toHaveLength(1);
    expect(hints).toEqual(['earlier']);
  });

  it('a click in the highlighted text opens nothing (the caret goes there)', async () => {
    const { c, asked, pos } = await withOwnComment();
    const at = posOf(c, 'two');
    const textNode = c.view.state.doc.nodeAt(at)!;
    expect(textNode.isText).toBe(true);
    expect(clickOn(c, at, textNode)).toBe(false);
    await settle();
    expect(asked).toHaveLength(1);
    // Control: the badge of the same comment does open.
    clickOn(c, pos, c.view.state.doc.nodeAt(pos)!);
    await settle();
    expect(asked).toHaveLength(2);
  });

  it('a modified or non-main click on a badge does not open the dialog', async () => {
    const { c, asked, pos } = await withOwnComment();
    const node = c.view.state.doc.nodeAt(pos)!;
    expect(clickOn(c, pos, node, click(0, { ctrlKey: true }))).toBe(false);
    expect(clickOn(c, pos, node, click(0, { metaKey: true }))).toBe(false);
    expect(clickOn(c, pos, node, click(0, { shiftKey: true }))).toBe(false);
    expect(clickOn(c, pos, node, click(0, { altKey: true }))).toBe(false);
    expect(clickOn(c, pos, node, click(1))).toBe(false);
    await settle();
    expect(asked).toHaveLength(1);
    // Control: a plain main-button click on the same badge opens the dialog.
    expect(clickOn(c, pos, node)).toBe(true);
    await settle();
    expect(asked).toHaveLength(2);
  });

  it('a double-click opens one dialog, not two', async () => {
    const { c, asked, answer, pos } = await withOwnComment();
    let release!: () => void;
    answer.gate = new Promise<void>((r) => (release = r));
    answer.note = 'new';
    const node = c.view.state.doc.nodeAt(pos)!;
    // The first click opens the dialog; the double-click arrives while it is open.
    clickOn(c, pos, node);
    await settle();
    expect(doubleClickOn(c, pos, node)).toBe(true);
    await settle();
    expect(asked).toHaveLength(2);
    release();
    await settle();
    expect(text(c)).toBe('one {==two==}{>>new<<} three\n');
    // Once closed, a double-click opens the dialog again.
    answer.gate = undefined;
    doubleClickOn(c, comments(c)[0], c.view.state.doc.nodeAt(comments(c)[0])!);
    await settle();
    expect(asked).toHaveLength(3);
  });
});
