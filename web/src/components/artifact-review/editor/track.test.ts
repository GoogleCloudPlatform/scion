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

import { afterEach, describe, expect, it } from 'vitest';
import { undo } from '@milkdown/prose/history';
import { splitBlock, toggleMark } from '@milkdown/prose/commands';
import { TextSelection } from '@milkdown/prose/state';
import type { Transaction } from '@milkdown/prose/state';

import { onlyMarksChanged, projectCritic } from '../../../utils/critic.js';
import { ReviewController, scrollToHeading } from './controller.js';
import type { NoteRequest } from './controller.js';
import { decide } from './policy.js';
import type { HintKind } from './policy.js';
import { corpus, renderContext } from './__fixtures__/core.js';

interface Harness {
  c: ReviewController;
  hints: HintKind[];
  asked: NoteRequest[];
  answer: { note: string | null };
}

const made: ReviewController[] = [];
afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  document.body.replaceChildren();
});

async function open(md: string): Promise<Harness> {
  const root = document.createElement('div');
  document.body.append(root);
  const hints: HintKind[] = [];
  const asked: NoteRequest[] = [];
  const answer = { note: 'a note' as string | null };
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: (req) => {
      asked.push(req);
      return Promise.resolve(answer.note);
    },
    onHint: (h) => hints.push(h),
    onChange: () => {},
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return { c, hints, asked, answer };
}

/** Document position of the n-th character of the text of the first textblock containing it. */
function posOf(c: ReviewController, needle: string, offset = 0): number {
  let found = -1;
  c.view.state.doc.descendants((n, pos) => {
    if (found >= 0) return false;
    if (n.isText && n.text!.includes(needle)) found = pos + n.text!.indexOf(needle) + offset;
    return true;
  });
  if (found < 0) throw new Error(`no ${needle}`);
  return found;
}

function select(c: ReviewController, from: number, to = from): void {
  const v = c.view;
  v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, from, to)));
}

function type(c: ReviewController, text: string, from: number, to = from): void {
  select(c, from, to);
  const v = c.view;
  v.dispatch(v.state.tr.insertText(text, from, to));
}

function backspace(c: ReviewController, at: number): void {
  select(c, at);
  const v = c.view;
  v.dispatch(v.state.tr.delete(at - 1, at));
}

function deleteRange(c: ReviewController, from: number, to: number): void {
  select(c, from, to);
  const v = c.view;
  v.dispatch(v.state.tr.delete(from, to));
}

const text = (c: ReviewController): string => c.flush()!;

describe('track changes (inline)', () => {
  it('typing inserts', async () => {
    const { c } = await open('a big dog\n');
    type(c, 'very ', posOf(c, 'big'));
    expect(text(c)).toBe('a {++very ++}big dog\n');
  });

  it('typing over a selection substitutes', async () => {
    const { c } = await open('a big dog\n');
    const p = posOf(c, 'big');
    type(c, 'small', p, p + 3);
    expect(text(c)).toBe('a {~~big~>small~~} dog\n');
  });

  it('backspace deletes, and a second one merges', async () => {
    const { c } = await open('a big dog\n');
    const p = posOf(c, 'big');
    backspace(c, p + 3);
    backspace(c, p + 2);
    expect(text(c)).toBe('a b{--ig--} dog\n');
  });

  it('typing inside an own insertion extends it, without nesting', async () => {
    const { c } = await open('a dog\n');
    type(c, 'big ', posOf(c, 'dog'));
    type(c, 'X', posOf(c, 'big', 2));
    expect(text(c)).toBe('a {++biXg ++}dog\n');
  });

  it('backspace in an own insertion removes the character', async () => {
    const { c } = await open('a dog\n');
    type(c, 'big ', posOf(c, 'dog'));
    backspace(c, posOf(c, 'big', 2));
    expect(text(c)).toBe('a {++bg ++}dog\n');
  });

  it('typing in an own replacement new side extends it', async () => {
    const { c } = await open('a cat!\n');
    const p = posOf(c, 'cat');
    type(c, 'dog', p, p + 3);
    type(c, 'o', posOf(c, 'dog', 1));
    expect(text(c)).toBe('a {~~cat~>doog~~}!\n');
  });

  it('retyping over an own replacement keeps one substitution', async () => {
    const { c } = await open('a big dog\n');
    const p = posOf(c, 'big');
    type(c, 'small', p, p + 3);
    const q = posOf(c, 'small');
    type(c, 'tiny', q, q + 5);
    expect(text(c)).toBe('a {~~big~>tiny~~} dog\n');
  });

  it('an earlier insertion or comment is not removed by typing (refused)', async () => {
    const { c, hints } = await open('a {++big ++}dog{>>note<<}\n');
    backspace(c, posOf(c, 'big', 2));
    const p = posOf(c, 'dog', 3);
    deleteRange(c, p, p + 1);
    expect(hints).toEqual(['earlier', 'earlier']);
    expect(text(c)).toBe('a {++big ++}dog{>>note<<}\n');
  });

  it('typing next to an earlier deletion or highlight makes a separate insertion', async () => {
    const { c, hints } = await open('a {++big++} {--old--} {==hl==} z\n');
    type(c, 'Y', posOf(c, 'old', 3));
    type(c, 'W', posOf(c, 'hl', 2));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++big++} {--old--}{++Y++} {==hl==}{++W++} z\n');
  });

  it('an own suggestion never joins an earlier one of the same kind (refused)', async () => {
    const md = 'a {++big++} {--old--}de z\n';
    const { c, hints } = await open(md);
    // Directly after and directly before an earlier insertion.
    type(c, 'X', posOf(c, 'big', 3));
    type(c, 'X', posOf(c, 'big', 0));
    // Deleting the character right after an earlier deletion.
    const p = posOf(c, 'de');
    deleteRange(c, p, p + 1);
    expect(hints).toEqual(['generic', 'generic', 'generic']);
    expect(text(c)).toBe(md);
    // Positive control: one character away, the insertion is recorded.
    type(c, 'X', posOf(c, 'de', 1));
    expect(text(c)).toBe('a {++big++} {--old--}d{++X++}e z\n');
  });

  it('typing inside an earlier replacement or highlight is refused, with the earlier hint', async () => {
    const md = 'a {~~old~>new~~} {==hl==}{>>n<<} z\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'old', 1));
    type(c, 'Y', posOf(c, 'new', 1));
    type(c, 'W', posOf(c, 'hl', 1));
    expect(hints).toEqual(['earlier', 'earlier', 'earlier']);
    expect(text(c)).toBe(md);
  });

  it('typing between an earlier highlight and its comment is refused (they are one unit)', async () => {
    const md = 'a {==x==}{>>n<<} z\n';
    const { c, hints } = await open(md);
    type(c, 'Y', posOf(c, 'x', 1));
    expect(hints).toEqual(['earlier']);
    expect(text(c)).toBe(md);
    // Positive control: after the comment, typing is recorded.
    type(c, 'W', posOf(c, ' z'));
    expect(text(c)).toBe('a {==x==}{>>n<<}{++W++} z\n');
  });

  it('a unit the editor does not read as the critic parser does is read-only', async () => {
    // An empty insertion: the critic parser reads a mark, the editor shows
    // the tokens as text (there is no text to carry a mark).
    const md = 'a {++++} b\n\nnext para\n';
    const { c, hints } = await open(md);
    expect(c.readOnlyBlocks()).toEqual([0]);
    type(c, 'Q', posOf(c, ' b'));
    expect(hints).toEqual(['generic']);
    // Positive control: the next block takes edits; the first is written as it was.
    type(c, 'Z', posOf(c, 'next'));
    expect(text(c)).toBe('a {++++} b\n\n{++Z++}next para\n');
  });

  it('a refusal in a read-only unit uses the generic hint, even beside a mark', async () => {
    const md = 'a {++++} b {++x++} c\n';
    const { c, hints } = await open(md);
    expect(c.readOnlyBlocks()).toEqual([0]);
    backspace(c, posOf(c, 'x', 1));
    type(c, 'Q', posOf(c, ' c'));
    expect(hints).toEqual(['generic', 'generic']);
    expect(text(c)).toBe(md);
  });

  it('the policy takes the earlier-mark hint from the session, by block', async () => {
    const { c } = await open('a {++big ++}dog\n');
    const v = c.view;
    const p = posOf(c, 'big', 1);
    const tr = v.state.tr.insertText('X', p);
    const blocks: number[] = [];
    const hint = decide(tr, v.state, undefined, (block) => {
      blocks.push(block);
      return 'generic';
    });
    expect(hint).toBe('generic');
    expect(blocks).toEqual([0]);
    expect(decide(v.state.tr.insertText('X', p), v.state)).toBe('earlier');
  });

  it('replacing a whole emphasised word is refused with a hint in this version', async () => {
    // The replacement's separator would be written outside the emphasis
    // ({~~*clear~>x*~~}), which changes the text outside the marks.
    const md = 'Make it *clear* now\n';
    const { c, hints } = await open(md);
    const p = posOf(c, 'clear');
    type(c, 'x', p, p + 5);
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
  });

  it('one undo reverts a deletion and a replacement', async () => {
    const { c } = await open('a big dog\n');
    const p = posOf(c, 'big');
    deleteRange(c, p, p + 3);
    expect(text(c)).toBe('a {--big--} dog\n');
    expect(undo(c.view.state, c.view.dispatch)).toBe(true);
    expect(text(c)).toBe('a big dog\n');
    type(c, 'red', posOf(c, 'dog'), posOf(c, 'dog') + 3);
    expect(text(c)).toBe('a big {~~dog~>red~~}\n');
    expect(undo(c.view.state, c.view.dispatch)).toBe(true);
    expect(text(c)).toBe('a big dog\n');
  });

  it('deleting bold text keeps the formatting inside the deletion', async () => {
    const { c } = await open('a **big** dog\n');
    const p = posOf(c, 'big');
    deleteRange(c, p, p + 3);
    expect(text(c)).toBe('a {--**big**--} dog\n');
  });

  it('an edit inside inline code replaces the whole span', async () => {
    const { c } = await open('run `make test` now\n');
    type(c, 'X', posOf(c, 'make', 2));
    expect(text(c)).toBe('run {~~`make test`~>`maXke test`~~} now\n');
  });

  it('one undo reverts one tracked edit', async () => {
    const { c } = await open('a big dog\n');
    type(c, 'very ', posOf(c, 'big'));
    expect(undo(c.view.state, c.view.dispatch)).toBe(true);
    expect(text(c)).toBe('a big dog\n');
  });

  it('keeps every other block byte for byte', async () => {
    const md = '# Title\n\n* one\n* two\n\n| a | b |\n|-|-|\n| 1 | 2 |\n\nedit me\n';
    const { c } = await open(md);
    type(c, 'X', posOf(c, 'edit'));
    const out = text(c);
    expect(out).toBe('# Title\n\n* one\n* two\n\n| a | b |\n|-|-|\n| 1 | 2 |\n\n{++X++}edit me\n');
    expect(onlyMarksChanged(out, projectCritic(md, 'clean'))).toBe(true);
  });
});

describe('refused edits in suggest mode', () => {
  it('Enter (a paragraph break) changes nothing and gives the break hint', async () => {
    const { c, hints } = await open('one two\n');
    select(c, posOf(c, 'two'));
    splitBlock(c.view.state, c.view.dispatch);
    expect(text(c)).toBe('one two\n');
    expect(hints).toEqual(['break']);
  });

  it('formatting changes nothing and gives the formatting hint', async () => {
    const { c, hints } = await open('one two\n');
    const p = posOf(c, 'two');
    select(c, p, p + 3);
    toggleMark(c.view.state.schema.marks.strong)(c.view.state, c.view.dispatch);
    expect(text(c)).toBe('one two\n');
    expect(hints).toEqual(['format']);
  });

  it('typing in a list item or a table is refused in this version', async () => {
    const { c, hints } = await open('- item\n\n| a |\n|---|\n| cell |\n');
    type(c, 'X', posOf(c, 'item'));
    type(c, 'X', posOf(c, 'cell'));
    expect(text(c)).toBe('- item\n\n| a |\n|---|\n| cell |\n');
    expect(hints).toEqual(['list-table', 'list-table']);
  });

  it('typing in a code block is refused', async () => {
    const { c, hints } = await open('```\ncode here\n```\n');
    type(c, 'X', posOf(c, 'code'));
    expect(text(c)).toBe('```\ncode here\n```\n');
    expect(hints).toEqual(['code']);
  });

  it('an edit whose save would change text outside marks is refused', async () => {
    // The serializer writes this paragraph without its backslash, which
    // would change the text outside marks, so the edit is refused.
    const md = 'a \\# b c\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'b c'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: the same edit in a paragraph the serializer keeps.
    const ok = await open('a b c\n');
    type(ok.c, 'X', posOf(ok.c, 'b c'));
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe('a {++X++}b c\n');
  });
});

describe('comments', () => {
  it('wraps an inline selection as a highlight with a note', async () => {
    const { c, asked } = await open('one two three\n');
    const p = posOf(c, 'two');
    select(c, p, p + 3);
    await c.comment();
    expect(asked[0].quote).toBe('two');
    expect(text(c)).toBe('one {==two==}{>>a note<<} three\n');
  });

  it('puts a note at the cursor without a selection', async () => {
    const { c } = await open('one two\n');
    select(c, posOf(c, 'two'));
    await c.comment();
    expect(text(c)).toBe('one {>>a note<<}two\n');
  });

  it('a cancelled dialog adds nothing', async () => {
    const { c, answer } = await open('one two\n');
    answer.note = null;
    select(c, posOf(c, 'two'));
    await c.comment();
    expect(text(c)).toBe('one two\n');
  });

  it('edits an own note, and an empty note deletes the comment and its highlight', async () => {
    const { c, answer, asked } = await open('one two three\n');
    const p = posOf(c, 'two');
    select(c, p, p + 3);
    answer.note = 'old';
    await c.comment();
    expect(text(c)).toBe('one {==two==}{>>old<<} three\n');
    let pos = -1;
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment') pos = q;
      return true;
    });
    answer.note = 'new';
    await c.editNote(pos);
    expect(asked[1]).toMatchObject({ note: 'old', number: 1 });
    expect(text(c)).toBe('one {==two==}{>>new<<} three\n');
    answer.note = '';
    await c.editNote(pos);
    expect(text(c)).toBe('one two three\n');
  });

  it('deleting an own note near an earlier highlight keeps the earlier one', async () => {
    const { c, answer } = await open('a {==x==} y z\n');
    const p = posOf(c, 'y');
    select(c, p, p + 1);
    answer.note = 'mine';
    await c.comment();
    expect(text(c)).toBe('a {==x==} {==y==}{>>mine<<} z\n');
    let pos = -1;
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment') pos = q;
      return true;
    });
    answer.note = '';
    await c.editNote(pos);
    expect(text(c)).toBe('a {==x==} y z\n');
  });

  it('an own highlight directly after an earlier one is refused (they would join)', async () => {
    const md = 'a {==x==}y z\n';
    const { c, hints } = await open(md);
    const p = posOf(c, 'y');
    select(c, p, p + 1);
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
  });
  it("after a switch to Source and back, the reviewer's comment is earlier too (read-only)", async () => {
    const { c, asked, answer, hints } = await open('one two {>>earlier<<}\n');
    select(c, posOf(c, 'two'));
    answer.note = 'mine';
    await c.comment();
    const comments = (): number[] => {
      const out: number[] = [];
      c.view.state.doc.descendants((n, q) => {
        if (n.type.name === 'critic_comment') out.push(q);
        return true;
      });
      return out;
    };
    // Positive control: before the switch, the reviewer's own note can be edited.
    answer.note = 'edited';
    await c.editNote(comments()[0]);
    expect(text(c)).toBe('one {>>edited<<}two {>>earlier<<}\n');
    expect(asked).toHaveLength(2);
    // Graphical -> Source -> Graphical: the text is opened again, and every
    // mark in it is earlier (own is set only when a mark is made here).
    expect(c.load(c.flush()!)).toBe(true);
    const [mine, earlier] = comments();
    expect(c.view.state.doc.nodeAt(mine)?.attrs.mine).toBe(false);
    expect(c.view.state.doc.nodeAt(earlier)?.attrs.mine).toBe(false);
    await c.editNote(mine);
    await c.editNote(earlier);
    expect(hints).toEqual(['earlier', 'earlier']);
    expect(asked).toHaveLength(2);
  });

  it('a key acts on the selection the browser shows, even before the editor saw the move', async () => {
    const { c, hints } = await open('one two\n');
    select(c, 1);
    const textNode = c.view.dom.querySelector('p')!.firstChild!;
    document.getSelection()!.setBaseAndExtent(textNode, 4, textNode, 7);
    // Bold on the selected word: refused with the formatting hint. On the
    // editor's stale empty selection it would do nothing.
    c.view.dom.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'b',
        code: 'KeyB',
        ctrlKey: true,
        bubbles: true,
        cancelable: true,
      })
    );
    expect(c.view.state.selection.from).toBe(5);
    expect(c.view.state.selection.to).toBe(8);
    expect(hints).toEqual(['format']);
    expect(text(c)).toBe('one two\n');
  });

  it('Ctrl+Alt+M also comments by the key name (a layout with M elsewhere)', async () => {
    const { c, asked } = await open('one two\n');
    select(c, posOf(c, 'two'));
    c.view.dom.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'm',
        code: 'Semicolon',
        ctrlKey: true,
        altKey: true,
        bubbles: true,
        cancelable: true,
      })
    );
    await Promise.resolve();
    await Promise.resolve();
    expect(asked).toHaveLength(1);
    expect(text(c)).toBe('one {>>a note<<}two\n');
  });

  it('Ctrl+Alt+M comments by the key position, at the caret the browser shows', async () => {
    const { c, asked } = await open('one two\n');
    select(c, 1);
    // The browser moved the caret before 'two' without the editor seeing it.
    const textNode = c.view.dom.querySelector('p')!.firstChild!;
    document.getSelection()!.collapse(textNode, 4);
    // A layout where Ctrl+Alt+M types another character: key differs, code is KeyM.
    const e = new KeyboardEvent('keydown', {
      key: 'µ',
      code: 'KeyM',
      ctrlKey: true,
      altKey: true,
      bubbles: true,
      cancelable: true,
    });
    c.view.dom.dispatchEvent(e);
    await Promise.resolve();
    await Promise.resolve();
    expect(e.defaultPrevented).toBe(true);
    expect(asked).toHaveLength(1);
    expect(text(c)).toBe('one {>>a note<<}two\n');
    // Another key with the same modifiers does nothing.
    const other = new KeyboardEvent('keydown', {
      key: 'n',
      code: 'KeyN',
      ctrlKey: true,
      altKey: true,
      bubbles: true,
      cancelable: true,
    });
    c.view.dom.dispatchEvent(other);
    await Promise.resolve();
    expect(other.defaultPrevented).toBe(false);
    expect(asked).toHaveLength(1);
  });

  it('shows a hint and changes nothing when the comment moved while the dialog was open', async () => {
    const root = document.createElement('div');
    document.body.append(root);
    const hints: HintKind[] = [];
    let onAsk: () => void = () => {};
    let answer = 'first';
    const c = await ReviewController.create({
      root,
      render: () => renderContext,
      askNote: () => {
        onAsk();
        return Promise.resolve(answer);
      },
      onHint: (h) => hints.push(h),
      onChange: () => {},
    });
    made.push(c);
    expect(c.load('one two\n')).toBe(true);
    select(c, posOf(c, 'two'));
    await c.comment();
    let pos = -1;
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment') pos = q;
      return true;
    });
    // While the dialog is open, text typed before the comment moves it.
    answer = 'changed';
    onAsk = (): void => type(c, 'X', 1);
    await c.editNote(pos);
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('{++X++}one {>>first<<}two\n');
    // Positive control: without the move, the note changes.
    onAsk = (): void => {};
    let now = -1;
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment') now = q;
      return true;
    });
    await c.editNote(now);
    expect(text(c)).toBe('{++X++}one {>>changed<<}two\n');
  });

  it('puts a caret comment inside a mark after the mark, not inside it', async () => {
    const cases: [string, string, number, string][] = [
      ['a {++ab++} z\n', 'ab', 1, 'a {++ab++}{>>a note<<} z\n'],
      ['a {~~old~>new~~} z\n', 'new', 2, 'a {~~old~>new~~}{>>a note<<} z\n'],
      ['a {~~old~>new~~} z\n', 'old', 1, 'a {~~old~>new~~}{>>a note<<} z\n'],
      ['a {==ab==}{>>theirs<<} z\n', 'ab', 1, 'a {==ab==}{>>theirs<<}{>>a note<<} z\n'],
      // At the seam between a highlight and its comment, earlier or own.
      ['a {==ab==}{>>theirs<<} z\n', 'ab', 2, 'a {==ab==}{>>theirs<<}{>>a note<<} z\n'],
    ];
    for (const [md, needle, offset, want] of cases) {
      const { c } = await open(md);
      select(c, posOf(c, needle, offset));
      await c.comment();
      expect(text(c), md).toBe(want);
    }
    // A selection inside an earlier highlight that ends at the seam.
    const sel = await open('a {==ab==}{>>theirs<<} z\n');
    select(sel.c, posOf(sel.c, 'ab', 1), posOf(sel.c, 'ab', 2));
    await sel.c.comment();
    expect(text(sel.c)).toBe('a {==ab==}{>>theirs<<}{>>a note<<} z\n');
    // An own highlight and its comment are kept together too.
    const own = await open('one two z\n');
    select(own.c, posOf(own.c, 'two'), posOf(own.c, 'two', 3));
    await own.c.comment();
    select(own.c, posOf(own.c, 'two', 3));
    own.answer.note = 'second';
    await own.c.comment();
    expect(text(own.c)).toBe('one {==two==}{>>a note<<}{>>second<<} z\n');
    // Positive control: at a plain caret the note goes at the caret.
    const { c } = await open('one two\n');
    select(c, posOf(c, 'two', 1));
    await c.comment();
    expect(text(c)).toBe('one t{>>a note<<}wo\n');
  });

  it('shows a hint and adds nothing when the document changed while the comment dialog was open', async () => {
    const root = document.createElement('div');
    document.body.append(root);
    const hints: HintKind[] = [];
    let onAsk: () => void = () => {};
    const c = await ReviewController.create({
      root,
      render: () => renderContext,
      askNote: () => {
        onAsk();
        return Promise.resolve('note');
      },
      onHint: (h) => hints.push(h),
      onChange: () => {},
    });
    made.push(c);
    expect(c.load('one two\n')).toBe(true);
    select(c, posOf(c, 'two'));
    onAsk = (): void => type(c, 'X', 1);
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('{++X++}one two\n');
    // Positive control: with no change, the comment is added where planned.
    onAsk = (): void => {};
    select(c, posOf(c, 'two'));
    await c.comment();
    expect(text(c)).toBe('{++X++}one {>>note<<}two\n');
  });

  it('Enter on a comment badge reached with Tab edits that comment', async () => {
    const { c, asked, answer } = await open('one two\n');
    select(c, posOf(c, 'two'));
    answer.note = 'first';
    await c.comment();
    const badge = c.view.dom.querySelector('.critic-comment') as HTMLElement;
    select(c, 1);
    badge.focus();
    answer.note = 'second';
    const e = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    badge.dispatchEvent(e);
    await Promise.resolve();
    await Promise.resolve();
    expect(e.defaultPrevented).toBe(true);
    expect(asked[1]).toMatchObject({ note: 'first', number: 1 });
    expect(text(c)).toBe('one {>>second<<}two\n');
  });

  it('a mark identical to an earlier one: own until the switch, then both earlier', async () => {
    const version = 'the {==the==}{>>theirs<<} end\n';
    const { c, answer } = await open(version);
    select(c, posOf(c, 'the'), posOf(c, 'the', 3));
    answer.note = 'mine';
    await c.comment();
    expect(c.flush()).toBe('{==the==}{>>mine<<} {==the==}{>>theirs<<} end\n');
    // Each highlight and each comment, in order: whether it is the reviewer's.
    const owners = (): boolean[] => {
      const out: boolean[] = [];
      c.view.state.doc.descendants((n) => {
        if (n.type.name === 'critic_comment') out.push(n.attrs.mine === true);
        const hl = n.marks.find((m) => m.type.name === 'critic_hl');
        if (n.isText && hl) out.push(hl.attrs.mine === true);
        return true;
      });
      return out;
    };
    expect(owners()).toEqual([true, true, false, false]);
    expect(c.load(c.flush()!)).toBe(true);
    expect(owners()).toEqual([false, false, false, false]);
  });

  it('marks typed as tokens in the Source view are earlier in the graphical view', async () => {
    const { c, hints } = await open('one two {>>earlier<<}\n');
    // The reviewer adds a comment in the Source view.
    expect(c.load(c.flush()!.replace('one ', 'one {>>from source<<}'))).toBe(true);
    const mine: [string, boolean, number][] = [];
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment')
        mine.push([String(n.attrs.note), n.attrs.mine === true, q]);
      return true;
    });
    expect(mine.map(([note, own]) => [note, own])).toEqual([
      ['from source', false],
      ['earlier', false],
    ]);
    await c.editNote(mine[0][2]);
    expect(hints).toEqual(['earlier']);
  });

  it('an earlier insertion inside link text stays inside it on edits elsewhere', async () => {
    const md = '[a {++b++}](https://e.example) z {>>n<<}\n';
    const { c, hints, answer } = await open(md);
    type(c, 'b', posOf(c, ' z', 2));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('[a {++b++}](https://e.example) z{++b++} {>>n<<}\n');
    // A comment inside the link text, before the earlier insertion.
    select(c, posOf(c, 'a ', 2));
    answer.note = 'mine';
    await c.comment();
    expect(hints).toEqual([]);
    expect(text(c)).toBe('[a {>>mine<<}{++b++}](https://e.example) z{++b++} {>>n<<}\n');
  });

  it('does not edit a comment already in the text', async () => {
    const { c, hints, asked } = await open('one {==two==}{>>old<<} three\n');
    let pos = -1;
    c.view.state.doc.descendants((n, q) => {
      if (n.type.name === 'critic_comment') pos = q;
      return true;
    });
    await c.editNote(pos);
    expect(asked).toEqual([]);
    expect(hints).toEqual(['earlier']);
    expect(text(c)).toBe('one {==two==}{>>old<<} three\n');
  });

  it('refuses a comment whose save would change text outside marks', async () => {
    const { c, hints } = await open('a \\# b c\n');
    select(c, posOf(c, 'b c'));
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('a \\# b c\n');
    // Positive control: the same comment in a paragraph the serializer keeps.
    const ok = await open('a b c\n');
    select(ok.c, posOf(ok.c, 'b c'));
    await ok.c.comment();
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe('a {>>a note<<}b c\n');
  });

  it('refuses a note that would end the comment early', async () => {
    const { c, hints, answer } = await open('one two\n');
    answer.note = 'x <<} y';
    select(c, posOf(c, 'two'));
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('one two\n');
  });

  it('refuses a comment inside a list in this version', async () => {
    const { c, hints } = await open('- item\n');
    select(c, posOf(c, 'item'));
    await c.comment();
    expect(hints).toEqual(['list-table']);
    expect(text(c)).toBe('- item\n');
  });
});

describe('composition (IME) input', () => {
  /**
   * What ProseMirror does while composing (untracked transactions tagged
   * with the composition id), then the controller's end-of-composition
   * step. time sets the composition transaction's time.
   */
  function compose(
    c: ReviewController,
    at: number,
    edit: (tr: Transaction) => Transaction,
    time?: number
  ): void {
    select(c, at);
    c.beginComposition();
    const v = c.view;
    const tr = edit(v.state.tr);
    tr.setMeta('composition', 7);
    if (time !== undefined) tr.setTime(time);
    v.dispatch(tr);
    c.endComposition();
    c.settleComposition();
  }

  it('records a composed word as an insertion', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    compose(c, p, (tr) => tr.insertText('大きい', p));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++大きい++}dog\n');
  });

  it('one undo after a long pause returns the document from before the composition', async () => {
    const { c } = await open('a dog\n');
    const before = c.view.state.doc;
    const p = posOf(c, 'dog');
    compose(c, p, (tr) => tr.insertText('犬', p), Date.now() - 2000);
    expect(text(c)).toBe('a {++犬++}dog\n');
    expect(undo(c.view.state, c.view.dispatch)).toBe(true);
    expect(c.view.state.doc.eq(before)).toBe(true);
  });

  it('undoes a composition that removed text, with the generic hint', async () => {
    const { c, hints } = await open('a dog\n');
    const before = c.view.state.doc;
    const p = posOf(c, 'dog');
    compose(c, p, (tr) => tr.insertText('犬', p, p + 3), Date.now() - 2000);
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('a dog\n');
    undo(c.view.state, c.view.dispatch);
    expect(c.view.state.doc.eq(before)).toBe(true);
  });

  it('undoes a composition in a list item or a code block, with their hints', async () => {
    const { c, hints } = await open('- item\n\n```\ncode\n```\n');
    const before = c.view.state.doc;
    const li = posOf(c, 'item');
    compose(c, li, (tr) => tr.insertText('X', li), Date.now() - 2000);
    const code = posOf(c, 'code');
    compose(c, code, (tr) => tr.insertText('Y', code));
    expect(hints).toEqual(['list-table', 'code']);
    expect(text(c)).toBe('- item\n\n```\ncode\n```\n');
    while (undo(c.view.state, c.view.dispatch)) {
      // back to the start
    }
    expect(c.view.state.doc.eq(before)).toBe(true);
  });

  it('undoes a composition that landed outside the block it started in', async () => {
    const { c, hints } = await open('one\n\ntwo\n');
    select(c, posOf(c, 'one'));
    c.beginComposition();
    const q = posOf(c, 'two');
    const tr = c.view.state.tr.insertText('Z', q);
    tr.setMeta('composition', 9);
    c.view.dispatch(tr);
    c.endComposition();
    c.settleComposition();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('one\n\ntwo\n');
  });
});

describe('composition timing', () => {
  const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

  function composeStep(c: ReviewController, at: number, t: string, id: number): void {
    const v = c.view;
    const tr = v.state.tr.insertText(t, at);
    tr.setMeta('composition', id);
    v.dispatch(tr);
  }

  it('settles one tick after compositionend, through the DOM events', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    select(c, p);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionstart'));
    composeStep(c, p, '犬', 3);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionend'));
    // ProseMirror may still apply the last change before the next tick.
    expect(c.flush()).toBe('a 犬dog\n');
    await tick();
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++犬++}dog\n');
  });

  it('records back-to-back compositions, the second starting before the first settles', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    select(c, p);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionstart'));
    composeStep(c, p, '大', 4);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionend'));
    c.view.dom.dispatchEvent(new CompositionEvent('compositionstart'));
    // The first composition was settled before the second snapshot.
    expect(c.flush()).toBe('a {++大++}dog\n');
    const q = posOf(c, 'dog');
    composeStep(c, q, 'きい', 5);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionend'));
    await tick();
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++大きい++}dog\n');
  });

  it('does not settle while the view is still composing', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    select(c, p);
    c.beginComposition();
    composeStep(c, p, '犬', 6);
    let composing = true;
    Object.defineProperty(c.view, 'composing', { get: () => composing, configurable: true });
    c.endComposition();
    await tick();
    expect(c.flush()).toBe('a 犬dog\n');
    // A new compositionstart keeps the older snapshot, which covers both.
    c.beginComposition();
    composeStep(c, posOf(c, 'dog'), '猫', 7);
    composing = false;
    c.endComposition();
    await tick();
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++犬猫++}dog\n');
  });

  it('records a composition whose start the editor did not see', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    select(c, p);
    // No compositionstart: the first composition transaction is the start.
    composeStep(c, p, '犬', 13);
    c.view.dom.dispatchEvent(new CompositionEvent('compositionend'));
    await tick();
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a {++犬++}dog\n');
  });

  it('restores exactly when the composed letter repeats its neighbour, in a list and in code', async () => {
    const md = '- item\n\n```\ncoode\n```\n';
    const { c, hints } = await open(md);
    const before = c.view.state.doc;
    const li = posOf(c, 'item', 2);
    select(c, li);
    c.beginComposition();
    composeStep(c, li, 't', 8);
    c.endComposition();
    c.settleComposition();
    const code = posOf(c, 'coode', 2);
    select(c, code);
    c.beginComposition();
    composeStep(c, code, 'o', 9);
    c.endComposition();
    c.settleComposition();
    expect(hints).toEqual(['list-table', 'code']);
    expect(c.view.state.doc.eq(before)).toBe(true);
    expect(text(c)).toBe(md);
  });

  it('restores a composition inside an earlier insertion, with the earlier-review hint', async () => {
    const md = 'a {++big++} dog\n';
    const { c, hints } = await open(md);
    const before = c.view.state.doc;
    const p = posOf(c, 'big', 1);
    select(c, p);
    c.beginComposition();
    composeStep(c, p, '犬', 12);
    c.endComposition();
    c.settleComposition();
    expect(hints).toEqual(['earlier']);
    expect(c.view.state.doc.eq(before)).toBe(true);
    expect(text(c)).toBe(md);
  });

  it('restores exactly when a composition removes one of a doubled letter', async () => {
    const { c, hints } = await open('hello world\n');
    const before = c.view.state.doc;
    const p = posOf(c, 'hello', 3);
    select(c, p);
    c.beginComposition();
    const tr = c.view.state.tr.delete(p, p + 1);
    tr.setMeta('composition', 10);
    c.view.dispatch(tr);
    c.endComposition();
    c.settleComposition();
    expect(hints).toEqual(['generic']);
    expect(c.view.state.doc.eq(before)).toBe(true);
  });

  it('records a composed letter that repeats its neighbour as one insertion', async () => {
    const { c } = await open('helo\n');
    const p = posOf(c, 'helo', 3);
    select(c, p);
    c.beginComposition();
    composeStep(c, p, 'l', 11);
    c.endComposition();
    c.settleComposition();
    const out = text(c);
    expect(out.match(/\{\+\+l\+\+\}/g)?.length).toBe(1);
    expect(projectCritic(out, 'accept')).toBe('hello\n');
    expect(projectCritic(out, 'clean')).toBe('helo\n');
  });
});

describe('preset plugins', () => {
  // Every plugin in the editor state. A Milkdown upgrade that adds a
  // plugin fails here, so it is audited before it can change a review's
  // document on its own (create.ts DROPPED_PLUGIN).
  const KEPT_PLUGINS = [
    'criticPaste',
    'criticPolicy',
    'criticTrack',
    'criticUi',
    'plugin', // the review keymap (controller.ts)
    'MILKDOWN_VIEW_CLEAR',
    'MILKDOWN_HARDBREAK_FILTER',
    'safari-ime-span',
    'history',
    'MILKDOWN_STATE_TRACKER',
    'plugin', // Milkdown's keymap
  ];

  it('keeps only the audited plugins', async () => {
    const { c } = await open('x\n');
    // Keys end in $ and an instance number.
    const keys = c.view.state.plugins.map((p) =>
      (p as unknown as { key: string }).key.replace(/\$\d*$/, '')
    );
    expect(keys).toEqual(KEPT_PLUGINS);
  });

  it('shows no hint when a document with a heading opens, nor after an edit', async () => {
    const { c, hints } = await open('# Title\n\npara\n');
    expect(hints).toEqual([]);
    type(c, 'X', posOf(c, 'para'));
    type(c, 'Y', posOf(c, 'Title'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('# {++Y++}Title\n\n{++X++}para\n');
  });

  it('shows no hint when any corpus document opens', async () => {
    const noisy: string[] = [];
    for (const doc of corpus) {
      const root = document.createElement('div');
      document.body.append(root);
      const hints: HintKind[] = [];
      const c = await ReviewController.create({
        root,
        render: () => renderContext,
        askNote: () => Promise.resolve(null),
        onHint: (h) => hints.push(h),
        onChange: () => {},
      });
      made.push(c);
      c.load(doc.in);
      await new Promise((r) => setTimeout(r, 0));
      if (hints.length) noisy.push(`${doc.name}: ${hints.join(',')}`);
    }
    expect(noisy).toEqual([]);
  });
});

describe('fragment links', () => {
  it('Mod-click on a #fragment link moves to the heading', async () => {
    const { c } = await open('[go](#second-part)\n\n# First\n\n## Second part\n');
    expect(scrollToHeading(c.view, 'second-part')).toBe(true);
    expect(c.view.state.selection.$from.parent.textContent).toBe('Second part');
    expect(scrollToHeading(c.view, 'nowhere')).toBe(false);
  });
});
