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

import { onlyMarksChanged, parseCritic, projectCritic } from '../../../utils/critic.js';
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

  it('typing in a list item or a table cell is recorded', async () => {
    const { c, hints } = await open('- item\n\n| a |\n|---|\n| cell |\n');
    type(c, 'X', posOf(c, 'item'));
    type(c, 'X', posOf(c, 'cell'));
    expect(text(c)).toBe('- {++X++}item\n\n| a |\n|---|\n| {++X++}cell |\n');
    expect(hints).toEqual([]);
  });

  it('typing in a code block is refused', async () => {
    const { c, hints } = await open('```\ncode here\n```\n');
    type(c, 'X', posOf(c, 'code'));
    expect(text(c)).toBe('```\ncode here\n```\n');
    expect(hints).toEqual(['code']);
  });

  it('an edit keeps the source of text the serializer writes differently', async () => {
    // The serializer writes this paragraph without its backslash; the
    // edit lands in the source, backslash kept.
    const { c, hints } = await open('a \\# b c\n');
    type(c, 'X', posOf(c, 'b c'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('a \\# {++X++}b c\n');
  });

  it('refuses typing in a block the hub reads as code running on from it', async () => {
    // The critic parser reads a code span from the heading's backtick into
    // the next paragraph (Markdown does not): a token typed in the heading
    // would be code text to the hub.
    const md = '# The ` key\nPress {++x++} then `';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'key'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: the same heading without the run.
    const ok = await open('# The key\nPress {++x++} then\n');
    type(ok.c, 'X', posOf(ok.c, 'key'));
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe('# The {++X++}key\nPress {++x++} then\n');
  });

  it('refuses a mark whose closer a literal opener in an earlier block would take', async () => {
    // The critic parser reads from the literal {++ to the first ++} after
    // it: an insertion typed in a later block would end a mark there.
    const md = 'Note: {++ stays literal.\n\n- one\n- two\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'two'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: a deletion has another closer.
    backspace(c, posOf(c, 'two', 1));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('Note: {++ stays literal.\n\n- one\n- {--t--}wo\n');
    // Positive control: an insertion before the literal opener.
    const ok = await open('- one\n- two\n\nNote: {++ stays literal.\n');
    type(ok.c, 'X', posOf(ok.c, 'two'));
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe('- one\n- {++X++}two\n\nNote: {++ stays literal.\n');
  });

  it('refuses a separator a literal replacement opener in an earlier block would take', async () => {
    // A {~~ with a later ~~} but no ~> is literal; a ~> typed between them
    // would make the critic parser read one replacement from the {~~.
    const md = 'Use {~~ to start.\n\nSome text here.\n\nAnd ~~} to end.\n';
    const { c, hints } = await open(md);
    type(c, '~>', posOf(c, 'text'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: other text typed in the same block is taken.
    type(c, 'X', posOf(c, 'text'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('Use {~~ to start.\n\nSome {++X++}text here.\n\nAnd ~~} to end.\n');
  });

  it('refuses a backtick typed in a block whose line run holds one in another block', async () => {
    // The paragraph and the list have no blank line between them: the
    // critic parser would pair the paragraph's backtick with one typed in
    // the list, and the insertion's tokens would be code.
    const { c, hints } = await open('Press the ` key:\n- one\n- two\n');
    type(c, '`', posOf(c, 'two'));
    expect(hints).toEqual(['code-run']);
    expect(text(c)).toBe('Press the ` key:\n- one\n- two\n');
    // Positive control: plain text in the same run is taken.
    type(c, 'X', posOf(c, 'two'));
    expect(text(c)).toBe('Press the ` key:\n- one\n- {++X++}two\n');
    // Positive control: with a blank line, the blocks are separate runs.
    const ok = await open('Press the ` key:\n\n- one\n- two\n');
    type(ok.c, '`', posOf(ok.c, 'two'));
    expect(ok.hints).toEqual([]);
    expect(
      onlyMarksChanged(text(ok.c), projectCritic('Press the ` key:\n\n- one\n- two\n', 'clean'))
    ).toBe(true);
  });

  it('refuses a separator typed after a literal replacement opener in the same block', async () => {
    // The {~~ in block 0 has a ~~} in block 2: a ~> after the {~~ in block 0
    // would make one replacement from block 0 to block 2.
    const md = 'Use {~~ to start.\n\nSome text here.\n\nAnd ~~} to end.\n';
    const { c, hints } = await open(md);
    type(c, '~>', posOf(c, 'start'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: other text in that block is taken.
    type(c, 'X', posOf(c, 'start'));
    expect(text(c)).toBe('Use {~~ to {++X++}start.\n\nSome text here.\n\nAnd ~~} to end.\n');
    // The same with the {~~ in a list item and the ~> typed in the next one.
    const list = '- Use {~~ to start\n- some text\n\nAnd ~~} to end.\n';
    const l = await open(list);
    type(l.c, '~>', posOf(l.c, 'text'));
    expect(l.hints).toEqual(['generic']);
    expect(text(l.c)).toBe(list);
  });

  it('refuses breaking a closer that a literal replacement opener does not reach yet', async () => {
    // {~~ has a ~~} in block 1 with no ~> between them; typing inside that
    // ~~} lets the {~~ reach the ~> and ~~} of block 2.
    const md = 'Use {~~ to start.\n\nMid ~~} here.\n\nThen ~> and ~~} end.\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, '~~}', 2));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // The same in the opener's own block.
    const own = 'Use {~~ to ~~} start.\n\nThen ~> and ~~} end.\n';
    const o = await open(own);
    type(o.c, 'X', posOf(o.c, '~~}', 2));
    expect(o.hints).toEqual(['generic']);
    expect(text(o.c)).toBe(own);
    // Positive control: other text in block 1 is taken.
    type(c, 'Y', posOf(c, 'here'));
    expect(text(c)).toBe('Use {~~ to start.\n\nMid ~~} {++Y++}here.\n\nThen ~> and ~~} end.\n');
  });

  it('takes a separator typed in a block wholly before a literal replacement opener', async () => {
    const md = 'Some text here.\n\nUse {~~ to start.\n\nAnd ~~} to end.\n';
    const { c, hints } = await open(md);
    type(c, '~>', posOf(c, 'text'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('Some {++\\~>++}text here.\n\nUse {~~ to start.\n\nAnd ~~} to end.\n');
  });

  it('refuses an edit that turns a line into something that looks like a fence', async () => {
    // The first line looks like a fence opener (the backtick in its info
    // string makes it text): an edit before it changes the fence-line count.
    const md = '``` a`b\n{++x++}\n';
    const { c, hints } = await open(md);
    type(c, 'X', 1);
    expect(hints).toEqual(['code-run']);
    expect(text(c)).toBe(md);
  });

  it('refuses a backtick typed next to a link definition whose title holds one', async () => {
    // The definition's title is in the same lines as the paragraph: the
    // critic parser would pair its backtick with a typed one.
    const md = '[k]: /keys "the ` key"\nPress a key.\n';
    const { c, hints } = await open(md);
    type(c, '`', posOf(c, 'a key'));
    expect(hints).toEqual(['code-run']);
    expect(text(c)).toBe(md);
    // Positive control: the same definition without a backtick.
    const plain = '[k]: /keys "the key"\nPress a key.\n';
    const ok = await open(plain);
    type(ok.c, '`', posOf(ok.c, 'a key'));
    expect(ok.hints).toEqual([]);
    expect(onlyMarksChanged(text(ok.c), projectCritic(plain, 'clean'))).toBe(true);
    // The same with the definition between two blocks of one line run.
    const between = '# Keys\n[k]: /keys "the ` key"\n- one\n- two\n';
    const b = await open(between);
    type(b.c, '`', posOf(b.c, 'two'));
    expect(b.hints).toEqual(['code-run']);
    expect(text(b.c)).toBe(between);
  });

  it('keeps backticks typed in two blocks of one line run from pairing', async () => {
    // Nothing in the run holds a backtick at load, so neither edit is
    // refused; typed backticks are written so that they never open code.
    const md = 'Press a key:\n- one\n- two\n';
    const { c, hints } = await open(md);
    type(c, '`', posOf(c, 'a key'));
    type(c, '`', posOf(c, 'two'));
    expect(hints).toEqual([]);
    const out = text(c);
    expect(onlyMarksChanged(out, projectCritic(md, 'clean'))).toBe(true);
    expect(parseCritic(out).filter((x) => x.kind === 'insertion')).toHaveLength(2);
    // Two backticks in one block and one in the next, in both orders.
    for (const [first, second] of [
      ['``', '`'],
      ['`', '``'],
    ]) {
      const mixed = await open(md);
      type(mixed.c, first, posOf(mixed.c, 'a key'));
      type(mixed.c, second, posOf(mixed.c, 'two'));
      expect(mixed.hints).toEqual([]);
      const o = text(mixed.c);
      expect(onlyMarksChanged(o, projectCritic(md, 'clean'))).toBe(true);
      expect(parseCritic(o).filter((x) => x.kind === 'insertion')).toHaveLength(2);
    }
    // Two backticks at a time, too.
    const two = await open(md);
    type(two.c, '``', posOf(two.c, 'a key'));
    type(two.c, '``', posOf(two.c, 'two'));
    expect(two.hints).toEqual([]);
    const out2 = text(two.c);
    expect(onlyMarksChanged(out2, projectCritic(md, 'clean'))).toBe(true);
    expect(parseCritic(out2).filter((x) => x.kind === 'insertion')).toHaveLength(2);
  });

  it('refuses a backtick typed above a table row holding a lone backtick', async () => {
    // The critic parser pairs backticks across the lines of a table; a
    // backtick typed in one row, with another row holding one, is refused.
    const md = '| a | b |\n|---|---|\n| x | y |\n| ` | z |\n';
    const { c, hints } = await open(md);
    type(c, '`', posOf(c, 'x', 1));
    expect(hints).toEqual(['code-run']);
    expect(text(c)).toBe(md);
    // Positive control: other text in the same row is taken.
    type(c, 'W', posOf(c, 'x', 1));
    expect(text(c)).toBe('| a | b |\n|---|---|\n| x{++W++} | y |\n| ` | z |\n');
  });

  it('edits a block with a reference link, keeping the reference', async () => {
    const md =
      'See [the guide][g] for {++more++} detail.\n\n- item with [a ref][g]\n\n[g]: https://example.com/guide\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'detail'));
    type(c, 'Y', posOf(c, 'item'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe(
      'See [the guide][g] for {++more++} {++X++}detail.\n\n- {++Y++}item with [a ref][g]\n\n[g]: https://example.com/guide\n'
    );
  });

  it('an edit with no exact place in the source is refused', async () => {
    // The source writes & as a character reference and the serializer as
    // the character: typing right after it has no place in the source
    // (inside the reference would change the text), so the editor falls
    // back to the serializer's text, which changes the text outside
    // marks, and refuses.
    const md = 'a &amp; b\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, '& b', 1));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(md);
    // Positive control: an edit away from the reference lands in the source.
    type(c, 'X', posOf(c, 'b'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('a &amp; {++X++}b\n');
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
    const { c, hints } = await open('a &amp; b\n');
    select(c, posOf(c, '& b', 1));
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('a &amp; b\n');
    // Positive control: a comment in a paragraph the serializer writes
    // differently lands in the source.
    const ok = await open('a \\# b c\n');
    select(ok.c, posOf(ok.c, 'b c'));
    await ok.c.comment();
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe('a \\# {>>a note<<}b c\n');
  });

  it('refuses a note that would end the comment early', async () => {
    const { c, hints, answer } = await open('one two\n');
    answer.note = 'x <<} y';
    select(c, posOf(c, 'two'));
    await c.comment();
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe('one two\n');
  });

  it('comments in a list item and in a table cell', async () => {
    const { c, hints } = await open('* item one\n\n| a | b |\n|:-|-:|\n| cell | x |\n');
    const p = posOf(c, 'one');
    select(c, p, p + 3);
    await c.comment();
    select(c, posOf(c, 'cell', 4));
    await c.comment();
    expect(hints).toEqual([]);
    expect(text(c)).toBe(
      '* item {==one==}{>>a note<<}\n\n| a | b |\n|:-|-:|\n| cell{>>a note<<} | x |\n'
    );
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

  it('a refused composition in an unedited document leaves it unedited', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    compose(c, p, (tr) => tr.insertText('犬', p, p + 3));
    expect(hints).toEqual(['generic']);
    expect(c.dirty).toBe(false);
    expect(text(c)).toBe('a dog\n');
  });

  it('a refused composition after an edit keeps the document edited', async () => {
    const { c, hints } = await open('a dog\n');
    type(c, 'big ', posOf(c, 'dog'));
    const p = posOf(c, 'dog');
    compose(c, p, (tr) => tr.insertText('犬', p, p + 3));
    expect(hints).toEqual(['generic']);
    expect(c.dirty).toBe(true);
    expect(text(c)).toBe('a {++big ++}dog\n');
  });

  it('records a composition in a list item and a table cell, and undoes one in a code block', async () => {
    const { c, hints } = await open('- item\n\n| a |\n|---|\n| cell |\n\n```\ncode\n```\n');
    const before = c.view.state.doc;
    const li = posOf(c, 'item');
    compose(c, li, (tr) => tr.insertText('X', li), Date.now() - 2000);
    const cell = posOf(c, 'cell');
    compose(c, cell, (tr) => tr.insertText('Z', cell), Date.now() - 2000);
    const code = posOf(c, 'code');
    compose(c, code, (tr) => tr.insertText('Y', code));
    expect(hints).toEqual(['code']);
    expect(text(c)).toBe('- {++X++}item\n\n| a |\n|---|\n| {++Z++}cell |\n\n```\ncode\n```\n');
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
    // The settle step waits a tick (ProseMirror may still apply the last
    // change); a flush before then (Save, a switch to Source) settles first.
    expect(c.view.state.doc.textContent).toBe('a 犬dog');
    expect(c.flush()).toBe('a {++犬++}dog\n');
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
    // Not settled yet: the composed text is not marked, so it cannot be
    // saved as a review.
    expect(c.view.state.doc.textContent).toBe('a 犬dog');
    expect(c.isComposing).toBe(true);
    expect(c.flush()).toBeNull();
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

  it('records a composed letter that repeats its neighbour in a list, and restores it exactly in code', async () => {
    const md = '- item\n\n```\ncoode\n```\n';
    const { c, hints } = await open(md);
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
    expect(hints).toEqual(['code']);
    expect(text(c)).toBe('- it{++t++}em\n\n```\ncoode\n```\n');
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

describe('whole-text check for hazardous documents (design E7)', () => {
  /** Switches off the fast first-line checks, so only the whole-text check is left. */
  function firstLineOff(c: ReviewController): void {
    const session = (c as unknown as { session: Record<string, unknown> }).session;
    session.addsCloser = (): boolean => false;
    session.changesCode = (): boolean => false;
  }

  /** Forces the hazard flag off, so the per-edit whole-text check does not run. */
  function hazardOff(c: ReviewController): void {
    (c as unknown as { session: Record<string, unknown> }).session.hazardous = false;
  }

  // Each case: a document and its edits ([text, offset in it, typed]); every
  // edit but the last changes no pairing on its own.
  const cases: [string, string, [string, number, string][]][] = [
    [
      'a closer after a literal opener',
      'Note: {++ stays literal.\n\n- one\n- two\n',
      [['two', 0, 'X']],
    ],
    [
      'a separator after a literal replacement opener',
      'Use {~~ to start.\n\nSome text here.\n\nAnd ~~} to end.\n',
      [['text', 0, '~>']],
    ],
    ['a backtick in a line run with one', 'Press the ` key:\n- one\n- two\n', [['two', 0, '`']]],
    [
      "a separator in the opener's own block",
      'Use {~~ to start.\n\nSome text here.\n\nAnd ~~} to end.\n',
      [['start', 0, '~>']],
    ],
    [
      'a backtick next to a definition with one',
      '[k]: /keys "the ` key"\nPress a key.\n',
      [['a key', 0, '`']],
    ],
    [
      'a broken closer the opener does not reach yet',
      'Use {~~ to start.\n\nMid ~~} here.\n\nThen ~> and ~~} end.\n',
      [['~~}', 2, 'X']],
    ],
    [
      "a broken closer in the opener's own block",
      'Use {~~ to ~~} start.\n\nThen ~> and ~~} end.\n',
      [['~~}', 2, 'X']],
    ],
    [
      'a broken closer in a list item',
      '- Use {~~ a\n- b ~~} c\n\nThen ~> and ~~} end.\n',
      [['~~}', 2, 'X']],
    ],
    // A pairing that needs two edits in two blocks: the check must read the
    // block edited earlier as save writes it.
    [
      'a broken closer, then a separator in a later block',
      'Use {~~ to start.\n\nMid ~~} here.\n\nThen and ~~} end.\n',
      [
        ['~~}', 2, 'X'],
        ['and', 0, '~>'],
      ],
    ],
    [
      'a broken separator, then a new one before the closer',
      'Use {~~ to start.\n\nSay hi ~~} then ~> bye.\n',
      [
        ['~>', 1, 'X'],
        ['hi', 0, '~>'],
      ],
    ],
  ];
  for (const [name, md, edits] of cases) {
    it(`refuses ${name} with the first-line checks off`, async () => {
      const { c, hints } = await open(md);
      expect(c.isHazardous).toBe(true);
      firstLineOff(c);
      // The text after the edits that were taken: the refused one leaves it.
      let before = md;
      for (const [k, [at, offset, typed]] of edits.entries()) {
        type(c, typed, posOf(c, at, offset));
        expect(hints).toEqual(k < edits.length - 1 ? [] : ['generic']);
        if (k < edits.length - 1) {
          before = text(c);
          expect(before).not.toBe(md);
          expect(onlyMarksChanged(before, projectCritic(md, 'clean'))).toBe(true);
        }
      }
      expect(text(c)).toBe(before);
    });

    it(`takes ${name} with no check left, and the save then refuses it`, async () => {
      // Control: without the first line and without the whole-text check,
      // every edit is taken and only save's own whole-text check stops it.
      const { c, hints } = await open(md);
      firstLineOff(c);
      hazardOff(c);
      for (const [at, offset, typed] of edits) type(c, typed, posOf(c, at, offset));
      expect(hints).toEqual([]);
      expect(c.flush()).toBeNull();
    });
  }

  it('takes edits that change no pairing in a hazardous document', async () => {
    // A literal {++: a deletion after it adds no ++}.
    const lit = 'Note: {++ stays literal.\n\n- one\n- two\n';
    const a = await open(lit);
    expect(a.c.isHazardous).toBe(true);
    backspace(a.c, posOf(a.c, 'two', 1));
    expect(a.hints).toEqual([]);
    expect(text(a.c)).toBe('Note: {++ stays literal.\n\n- one\n- {--t--}wo\n');
    // A backtick shared in a line run: plain text typed there is taken.
    const run = 'Press the ` key:\n- one\n- two\n';
    const b = await open(run);
    expect(b.c.isHazardous).toBe(true);
    type(b.c, 'Z', posOf(b.c, 'two'));
    expect(b.hints).toEqual([]);
    expect(text(b.c)).toBe('Press the ` key:\n- one\n- {++Z++}two\n');
  });

  it('keeps a document that is not hazardous at load so through a sequence of edits', async () => {
    const md = 'Plain text here.\n\n- one\n- two\n\n| a | b |\n|---|---|\n| c | d |\n';
    const { c, hints } = await open(md);
    expect(c.isHazardous).toBe(false);
    type(c, '{++', posOf(c, 'text'));
    type(c, '`', posOf(c, 'two'));
    type(c, '~>', posOf(c, 'one'));
    backspace(c, posOf(c, 'd', 1));
    expect(hints).toEqual([]);
    const out = text(c);
    expect(onlyMarksChanged(out, projectCritic(md, 'clean'))).toBe(true);
    expect(parseCritic(out).filter((x) => x.kind !== 'text')).toHaveLength(4);
    // The saved text, opened again, is still not hazardous: the edits
    // created no literal opener and no shared backtick.
    const again = await open(out);
    expect(again.c.isHazardous).toBe(false);
  });
});

describe('lists and tables keep their source', () => {
  it('an edit in one item leaves the other items and the markers as written', async () => {
    const md = '* one\n* two words\n* three\n\n+ plus\n';
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'words'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('* one\n* two {++X++}words\n* three\n\n+ plus\n');
  });

  it('patches one item of a long list the editor writes differently', async () => {
    // The editor writes every * bullet as - and every &amp; as &: across
    // the whole list that is more difference than the patch works
    // through, so only the item is patched.
    const items = Array.from({ length: 300 }, (_, i) => `* &amp; &amp; &amp; &amp; item ${i}`);
    const md = `${items.join('\n')}\n`;
    const { c, hints } = await open(md);
    type(c, 'X', posOf(c, 'item 200'));
    expect(hints).toEqual([]);
    items[200] = '* &amp; &amp; &amp; &amp; {++X++}item 200';
    expect(text(c)).toBe(`${items.join('\n')}\n`);
  });

  it('refuses structure edits in lists and tables with the list and table hint', async () => {
    const md = '- one\n- two\n\n| a | b |\n|---|---|\n| c | d |\n';
    const { c, hints } = await open(md);
    // A deletion across two items, and across two cells.
    deleteRange(c, posOf(c, 'one', 1), posOf(c, 'two', 1));
    deleteRange(c, posOf(c, 'c'), posOf(c, 'd', 1));
    // A split in an item (Enter).
    select(c, posOf(c, 'one', 1));
    splitBlock(c.view.state, c.view.dispatch);
    expect(hints).toEqual(['list-table', 'list-table', 'list-table']);
    expect(c.dirty).toBe(false);
    expect(text(c)).toBe(md);
  });

  it('keeps the numbers of an ordered list and of a nested one', async () => {
    const md = '3. three\n7. seven\n10. ten\n    1. nested one\n    1. nested two\n';
    const { c } = await open(md);
    const p = posOf(c, 'seven');
    type(c, 'Y', p, p + 1);
    type(c, 'Z', posOf(c, 'two'));
    expect(text(c)).toBe(
      '3. three\n7. {~~s~>Y~~}even\n10. ten\n    1. nested one\n    1. nested {++Z++}two\n'
    );
  });

  it('keeps a loose list loose and a quoted list quoted', async () => {
    const md = '- loose one\n\n- loose two\n\n> - quoted item\n> - another\n';
    const { c } = await open(md);
    type(c, 'X', posOf(c, 'two'));
    type(c, 'Y', posOf(c, 'another'));
    expect(text(c)).toBe(
      '- loose one\n\n- loose {++X++}two\n\n> - quoted item\n> - {++Y++}another\n'
    );
  });

  it('an edit in one row leaves the other rows, the alignment and the padding as written', async () => {
    const md = '| Left | Center | Right |\n|:-----|:------:|------:|\n| a | c | 1 |\n|d|ee|2|\n';
    const { c, hints } = await open(md);
    type(c, 'long text', posOf(c, 'ee'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe(
      '| Left | Center | Right |\n|:-----|:------:|------:|\n| a | c | 1 |\n|d|{++long text++}ee|2|\n'
    );
  });

  it('edits the header row in place', async () => {
    const md = '| Name   | Value |\n| ------ | ----- |\n| x      | 1     |\n';
    const { c } = await open(md);
    type(c, 'Full ', posOf(c, 'Name'));
    expect(text(c)).toBe('| {++Full ++}Name   | Value |\n| ------ | ----- |\n| x      | 1     |\n');
  });

  it('keeps a backslashed pipe in a cell, beside the edit and inside it', async () => {
    const md = '| a | b |\n|---|---|\n| x \\| y | z |\n';
    const { c, hints } = await open(md);
    type(c, 'W', posOf(c, 'z'));
    expect(text(c)).toBe('| a | b |\n|---|---|\n| x \\| y | {++W++}z |\n');
    // A replacement that takes in the backslashed pipe.
    const p = posOf(c, '| y');
    type(c, 'V', p - 1, p + 3);
    expect(hints).toEqual([]);
    expect(text(c)).toBe('| a | b |\n|---|---|\n| x{~~ \\| y~>V~~} | {++W++}z |\n');
  });

  it('writes a list with a mark across two items as one unit', async () => {
    const md = '- a {++x\n- y++} b\n- c\n';
    const { c, hints } = await open(md);
    type(c, 'Z', posOf(c, 'c'));
    expect(hints).toEqual([]);
    expect(text(c)).toBe('- a {++x\n- y++} b\n- {++Z++}c\n');
  });

  it('refuses an edit in a block the serializer rewrites in too many places to patch', async () => {
    // Every &amp; is written as &, which is more difference than the patch
    // works through: the serializer's text is used, it changes the text
    // outside marks, and the edit is refused.
    const big = `${'&amp; '.repeat(1500)}end\n`;
    const { c, hints } = await open(big);
    type(c, 'X', posOf(c, 'end'));
    expect(hints).toEqual(['generic']);
    expect(text(c)).toBe(big);
    // Positive control: the same edit with a few of them.
    const small = `${'&amp; '.repeat(5)}end\n`;
    const ok = await open(small);
    type(ok.c, 'X', posOf(ok.c, 'end'));
    expect(ok.hints).toEqual([]);
    expect(text(ok.c)).toBe(`${'&amp; '.repeat(5)}{++X++}end\n`);
  });
});

describe('review keymap', () => {
  function press(c: ReviewController, key: string, mods: Partial<KeyboardEventInit> = {}): boolean {
    // keyCode: ProseMirror reads it for shifted and Alt shortcuts.
    const keyCode = key.length === 1 ? key.toUpperCase().charCodeAt(0) : 0;
    const e = new KeyboardEvent('keydown', {
      key,
      keyCode,
      bubbles: true,
      cancelable: true,
      ...mods,
    });
    c.view.dom.dispatchEvent(e);
    return e.defaultPrevented;
  }

  it('formatting and block shortcuts change nothing and show the formatting hint', async () => {
    const { c, hints } = await open('one two\n');
    select(c, posOf(c, 'two'), posOf(c, 'two', 3));
    const shortcuts: [string, Partial<KeyboardEventInit>][] = [
      ['b', { ctrlKey: true }],
      ['i', { ctrlKey: true }],
      ['e', { ctrlKey: true }],
      ['x', { ctrlKey: true, altKey: true }],
      ['1', { ctrlKey: true, altKey: true }],
      ['0', { ctrlKey: true, altKey: true }],
      ['7', { ctrlKey: true, altKey: true }],
      ['8', { ctrlKey: true, altKey: true }],
      ['c', { ctrlKey: true, altKey: true }],
      ['B', { ctrlKey: true, shiftKey: true }],
    ];
    for (const [key, mods] of shortcuts) expect(press(c, key, mods)).toBe(true);
    expect(hints).toEqual(shortcuts.map(() => 'format'));
    expect(text(c)).toBe('one two\n');
    expect(c.dirty).toBe(false);
  });

  it('Shift-Enter is refused like a paragraph break', async () => {
    const { c, hints } = await open('one two\n');
    select(c, posOf(c, 'two'));
    expect(press(c, 'Enter', { shiftKey: true })).toBe(true);
    expect(hints).toEqual(['break']);
    expect(c.dirty).toBe(false);
  });

  it('Tab and Shift-Tab move between table cells without changing the text', async () => {
    const md = '| a | b |\n| - | - |\n| c | d |\n';
    const { c, hints } = await open(md);
    select(c, posOf(c, 'a'));
    expect(press(c, 'Tab')).toBe(true);
    expect(c.view.state.selection.$from.parent.textContent).toBe('b');
    expect(press(c, 'Tab')).toBe(true);
    expect(c.view.state.selection.$from.parent.textContent).toBe('c');
    expect(press(c, 'Tab', { shiftKey: true })).toBe(true);
    expect(c.view.state.selection.$from.parent.textContent).toBe('b');
    expect(hints).toEqual([]);
    expect(text(c)).toBe(md);
  });

  it('Enter in a table and Tab in a list are refused with the list and table hint', async () => {
    const { c, hints } = await open('| a | b |\n| - | - |\n| c | d |\n\n- one\n- two\n');
    select(c, posOf(c, 'c', 1));
    expect(press(c, 'Enter')).toBe(true);
    select(c, posOf(c, 'two'));
    expect(press(c, 'Tab')).toBe(true);
    expect(press(c, 'Tab', { shiftKey: true })).toBe(true);
    expect(hints).toEqual(['list-table', 'list-table', 'list-table']);
    expect(c.dirty).toBe(false);
  });

  it('Tab outside lists and tables is left to the browser', async () => {
    const { c, hints } = await open('one two\n');
    select(c, posOf(c, 'two'));
    expect(press(c, 'Tab')).toBe(false);
    expect(hints).toEqual([]);
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
