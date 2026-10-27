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
 * Earlier marks in this version of the editor (design E5 and E6): a mark
 * is the reviewer's own only if it was made in this editor since the text
 * was last opened here (opening a text, including every return from the
 * Source view, makes every mark earlier); earlier marks are immutable, and
 * no edit may move one (its place in the source with the marks removed).
 */

import { afterEach, describe, expect, it } from 'vitest';
import { undo } from '@milkdown/prose/history';
import { TextSelection } from '@milkdown/prose/state';

import { ReviewController } from './controller.js';
import type { HintKind } from './policy.js';
import { LOAD_META } from './track.js';
import type { Node } from '@milkdown/prose/model';
import { Transform } from '@milkdown/prose/transform';

import { chooseAlphabet } from '../../../utils/critic-sentinels.js';
import { saveDoc } from './bridge.js';
import { docMarkRanges, docMarkRuns } from './guard.js';
import { docStrings } from './preserve.js';
import { corpus, renderContext } from './__fixtures__/core.js';

interface Harness {
  c: ReviewController;
  hints: HintKind[];
  changes: { n: number };
  answer: { note: string };
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
  const changes = { n: 0 };
  const answer = { note: 'ok' };
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: () => Promise.resolve(answer.note),
    onHint: (h) => hints.push(h),
    onChange: () => changes.n++,
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return { c, hints, changes, answer };
}

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

function type(c: ReviewController, text: string, at: number): void {
  const v = c.view;
  v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, at)));
  v.dispatch(v.state.tr.insertText(text, at));
}

function comments(c: ReviewController): { note: string; mine: boolean; pos: number }[] {
  const out: { note: string; mine: boolean; pos: number }[] = [];
  c.view.state.doc.descendants((n, pos) => {
    if (n.type.name === 'critic_comment') {
      out.push({ note: String(n.attrs.note), mine: n.attrs.mine === true, pos });
    }
    return true;
  });
  return out;
}

/** Whether any mark or comment of the document is the reviewer's. */
function ownCount(c: ReviewController): number {
  let n = 0;
  let prevMine = false;
  c.view.state.doc.descendants((node) => {
    if (node.type.name === 'critic_comment') {
      if (node.attrs.mine) n++;
      prevMine = false;
      return true;
    }
    if (!node.isText) {
      prevMine = false;
      return true;
    }
    const m = node.marks.find((x) => x.type.name.startsWith('critic_'));
    const mine = m?.attrs.mine === true;
    if (mine && !prevMine) n++;
    prevMine = mine;
    return true;
  });
  return n;
}

// Earlier comments at the start and the end edge of the four inline runs.
const EDGES: [string, string, string][] = [
  ['link end', 'See [the docs{>>check this<<}](https://e.example) for more.\n', 'more.'],
  ['link start', 'See [{>>check this<<}the docs](https://e.example) for more.\n', 'more.'],
  ['emphasis end', 'Make it *clear{>>why?<<}* now.\n', 'now.'],
  ['emphasis start', 'Make it *{>>why?<<}clear* now.\n', 'now.'],
  ['strong end', 'Make it **clear{>>why?<<}** now.\n', 'now.'],
  ['strong start', 'Make it **{>>why?<<}clear** now.\n', 'now.'],
  ['strikethrough end', 'Make it ~~clear{>>why?<<}~~ now.\n', 'now.'],
  ['strikethrough start', 'Make it ~~{>>why?<<}clear~~ now.\n', 'now.'],
];

describe('earlier comments at the edge of an inline run', () => {
  for (const [name, version, after] of EDGES) {
    it(`${name}: an edit elsewhere keeps the comment in its place`, async () => {
      const { c, hints } = await open(version);
      type(c, '!', posOf(c, after, after.length));
      expect(hints).toEqual([]);
      expect(c.flush()).toBe(version.replace(`${after}\n`, `${after}{++!++}\n`));
    });

    it(`${name}: after a switch to Source and back, the comment is still earlier and read-only`, async () => {
      const { c, hints, answer } = await open(version);
      type(c, '!', posOf(c, after, after.length));
      expect(c.load(c.flush()!)).toBe(true);
      const [comment] = comments(c);
      expect(comment.mine).toBe(false);
      answer.note = 'rewritten';
      await c.editNote(comment.pos);
      // Deleting it is refused too.
      const v = c.view;
      v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, comment.pos + 1)));
      v.dispatch(v.state.tr.delete(comment.pos, comment.pos + 1));
      expect(hints).toEqual(['earlier', 'earlier']);
      expect(c.flush()).toBe(version.replace(`${after}\n`, `${after}{++!++}\n`));
    });

    it(`${name}: an edit that would move the comment is refused, with its own hint`, async () => {
      const { c, hints } = await open(version);
      // The comment loses the formatting it was written under (as if the
      // writer placed it by its neighbours): it would be written outside
      // the run, in another place.
      const [comment] = comments(c);
      const v = c.view;
      const node = v.state.doc.nodeAt(comment.pos)!;
      v.dispatch(
        v.state.tr.setNodeMarkup(comment.pos, undefined, node.attrs, []).setMeta(LOAD_META, true)
      );
      type(c, '!', posOf(c, after, after.length));
      expect(hints).toEqual(['moved']);
      expect(c.view.state.doc.textContent.includes('!')).toBe(false);
      // Save refuses to write the comment in another place, too.
      expect(c.flush()).toBeNull();
    });
  }

  it('positive control: a comment in the middle of link text', async () => {
    const version = 'See [a{>>n<<}b](https://e.example) for more.\n';
    const { c, hints } = await open(version);
    type(c, '!', posOf(c, 'more.', 5));
    expect(hints).toEqual([]);
    expect(c.flush()).toBe('See [a{>>n<<}b](https://e.example) for more.{++!++}\n');
  });

  it('an earlier comment inside emphasis: edit accepted, comment kept, still earlier', async () => {
    const version = 'Plain *word{>>earlier note<<}* end.\n';
    const { c, hints } = await open(version);
    type(c, 'X', posOf(c, 'end', 3));
    expect(c.flush()).toBe('Plain *word{>>earlier note<<}* end{++X++}.\n');
    expect(c.load(c.flush()!)).toBe(true);
    const [comment] = comments(c);
    expect(comment.mine).toBe(false);
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, comment.pos + 1)));
    v.dispatch(v.state.tr.delete(comment.pos, comment.pos + 1));
    expect(hints).toEqual(['earlier']);
  });
});

describe('own marks are set only when made here (identical marks at the same place)', () => {
  /** Each comment and critic run in order: kind:text:own. */
  const owners = (c: ReviewController): string[] => {
    const out: string[] = [];
    c.view.state.doc.descendants((n) => {
      if (n.type.name === 'critic_comment') out.push(`comment:${n.attrs.note}:${n.attrs.mine}`);
      else if (n.isText) {
        const m = n.marks.find((x) => x.type.name.startsWith('critic_'));
        if (m) out.push(`${m.type.name}:${n.text}:${m.attrs.mine}`);
      }
      return true;
    });
    return out;
  };

  it('an own note equal to an earlier one at the seam after its highlight', async () => {
    for (const [version, note] of [
      ['Some {==x==}{>>ok<<} text\n', 'ok'],
      ['See {==x==}{>>+1<<} here.\n', '+1'],
    ] as const) {
      const { c, hints, answer } = await open(version);
      const p = posOf(c, 'x', 1);
      c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, p)));
      answer.note = note;
      await c.comment();
      // Before the switch, the reviewer's note (the last) is theirs.
      expect(owners(c)).toEqual([
        'critic_hl:x:false',
        `comment:${note}:false`,
        `comment:${note}:true`,
      ]);
      // After the switch to Source and back, every mark is earlier.
      expect(c.load(c.flush()!)).toBe(true);
      expect(owners(c)).toEqual([
        'critic_hl:x:false',
        `comment:${note}:false`,
        `comment:${note}:false`,
      ]);
      const [first] = comments(c);
      answer.note = 'changed';
      await c.editNote(first.pos);
      expect(hints).toEqual(['earlier']);
      expect(c.flush()).not.toContain('changed');
    }
  });

  it('an own insertion equal to an earlier one next to it', async () => {
    const { c, hints } = await open('A {++Z++}{>>n<<} B\n');
    const [comment] = comments(c);
    type(c, 'Z', comment.pos + 1);
    expect(owners(c)).toEqual(['critic_ins:Z:false', 'comment:n:false', 'critic_ins:Z:true']);
    expect(c.load(c.flush()!)).toBe(true);
    expect(owners(c)).toEqual(['critic_ins:Z:false', 'comment:n:false', 'critic_ins:Z:false']);
    // Withdrawing the first Z is refused (it is earlier).
    const p = posOf(c, 'Z', 1);
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, p)));
    c.view.dispatch(c.view.state.tr.delete(p - 1, p));
    expect(hints).toEqual(['earlier']);
    expect(c.flush()).toBe('A {++Z++}{>>n<<}{++Z++} B\n');
  });

  it('a mark typed in the Source view before an identical earlier one stays earlier, as does the earlier one', async () => {
    for (const add of ['{>>n<<}', '{++,++}']) {
      const version = `A ${add} B\n`;
      const { c } = await open(version);
      expect(c.load(c.flush()!.replace('A ', `A ${add} `))).toBe(true);
      expect(owners(c).every((x) => x.endsWith(':false'))).toBe(true);
      expect(owners(c)).toHaveLength(2);
    }
  });

  it('a clean-text change in the Source view makes no mark own', async () => {
    const { c, hints } = await open('one {>>n<<} two {++x++} three\n');
    type(c, 'Q', posOf(c, 'three', 5));
    // In the Source view the reviewer shifts the text, then returns.
    const shifted = c.flush()!.replace('one ', 'one more ');
    expect(c.load(shifted)).toBe(true);
    expect(owners(c).every((x) => x.endsWith(':false'))).toBe(true);
    const [comment] = comments(c);
    await c.editNote(comment.pos);
    expect(hints).toEqual(['earlier']);
  });
});

describe('typing right after a comment at the edge of formatting', () => {
  const cases: [string, string, string][] = [
    ['Make it *clear{>>why?<<}* now.\n', 'why?', 'Make it *clear{>>why?<<}{++X++}* now.\n'],
    [
      'See [the docs{>>n<<}](https://e.example) for more.\n',
      'n',
      'See [the docs{>>n<<}{++X++}](https://e.example) for more.\n',
    ],
  ];
  for (const [version, note, want] of cases) {
    it(`after the comment in ${JSON.stringify(version).slice(0, 30)}: no refusal, formatting as the run`, async () => {
      const { c, hints } = await open(version);
      const comment = comments(c).find((x) => x.note === note)!;
      type(c, 'X', comment.pos + 1);
      expect(hints).toEqual([]);
      expect(c.flush()).toBe(want);
    });
  }
});

describe('ownership across reloads', () => {
  // The reload property: after any accepted edit, opening the saved text
  // again (a switch to Source and back, or a new review) shows every mark
  // in the same place with the same kind, text and note, and none of them
  // is the reviewer's own (own is set only when a mark is made here).
  const docs = [
    ...EDGES.map(([, md]) => md),
    'a {++x++} b {==h==}{>>n<<} c {--d--} e {~~f~>g~~} h\n',
    'Some {==x==}{>>ok<<} text\n',
    'A {++Z++}{>>n<<} B\n',
    ...corpus.map((x) => x.in),
  ];
  const marks = (c: ReviewController): string[] => {
    const runs = docMarkRuns(c.view.state.doc);
    const ranges = docMarkRanges(c.view.state.doc);
    return runs.map(
      (r, n) =>
        `${r.kind}|${r.text}|${r.next ?? ''}@${ranges[n].map((x) => `${x.from}-${x.to}`).join(',')}`
    );
  };

  it('opening the saved text again keeps every mark in place, and none is own', async () => {
    let edited = 0;
    for (const md of docs) {
      const root = document.createElement('div');
      document.body.append(root);
      const c = await ReviewController.create({
        root,
        render: () => renderContext,
        askNote: () => Promise.resolve(null),
        onHint: () => {},
        onChange: () => {},
      });
      made.push(c);
      if (!c.load(md)) continue;
      // Type at the end of the last textblock.
      let end = -1;
      c.view.state.doc.descendants((n, q) => {
        if (n.isTextblock) end = q + n.nodeSize - 1;
        return true;
      });
      if (end < 0) continue;
      type(c, 'Q', end);
      const out = c.flush();
      if (out === null || out === md) continue;
      edited++;
      expect(ownCount(c), md).toBe(1);
      const before = marks(c);
      expect(c.load(out), md).toBe(true);
      expect(marks(c), md).toEqual(before);
      expect(ownCount(c), md).toBe(0);
      expect(
        docMarkRuns(c.view.state.doc).every((r) => r.mine === false),
        md
      ).toBe(true);
    }
    expect(edited).toBeGreaterThan(20);
  });
});

describe('own marks in this view', () => {
  it('hasOwnMarks: none at load, some after a comment, none after the text is opened again', async () => {
    const { c, answer } = await open('one two {>>earlier<<}\n');
    expect(c.hasOwnMarks()).toBe(false);
    const t = posOf(c, 'two');
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, t)));
    answer.note = 'mine';
    await c.comment();
    expect(c.hasOwnMarks()).toBe(true);
    expect(c.load(c.flush()!)).toBe(true);
    expect(c.hasOwnMarks()).toBe(false);
  });
});

describe('own is never saved', () => {
  it('the same document saves to the same bytes with and without own attributes', async () => {
    const version = 'one {++x++} two {==h==}{>>n<<} three four\n';
    const { c, answer } = await open(version);
    // Own marks of every kind: an insertion, a deletion, a replacement and
    // a highlighted comment.
    type(c, 'A', posOf(c, 'one', 0));
    const p = posOf(c, 'three');
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, p, p + 5)));
    c.view.dispatch(c.view.state.tr.delete(p, p + 5));
    const q = posOf(c, 'four');
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, q, q + 4)));
    c.view.dispatch(c.view.state.tr.insertText('FOUR', q, q + 4));
    const t = posOf(c, 'two');
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, t, t + 3)));
    answer.note = 'mine';
    await c.comment();
    const withOwn = c.flush()!;
    expect(ownCount(c)).toBeGreaterThanOrEqual(4);
    // The same document with every own attribute cleared.
    const doc = c.view.state.doc;
    const tr = new Transform(doc);
    doc.descendants((n, pos) => {
      if (n.type.name === 'critic_comment') {
        tr.setNodeMarkup(pos, undefined, { ...n.attrs, mine: false }, n.marks);
        return false;
      }
      if (!n.isText) return true;
      for (const m of n.marks) {
        if (!m.type.name.startsWith('critic_')) continue;
        tr.removeMark(pos, pos + n.nodeSize, m);
        tr.addMark(pos, pos + n.nodeSize, m.type.create({ ...m.attrs, mine: false }));
      }
      return false;
    });
    expect(ownCount(c)).toBeGreaterThan(0);
    expect(docMarkRuns(tr.doc).length).toBeGreaterThan(0);
    expect(docMarkRuns(tr.doc).every((r) => r.mine === false)).toBe(true);
    const a1 = chooseAlphabet(docStrings(doc))!;
    const a2 = chooseAlphabet(docStrings(tr.doc))!;
    const serialize = (d: Node): string =>
      (c as unknown as { core: { serialize(x: Node): string } }).core.serialize(d);
    const bytesWith = saveDoc(doc, serialize, doc.type.schema, a1);
    const bytesWithout = saveDoc(tr.doc, serialize, doc.type.schema, a2);
    expect(bytesWithout).toBe(bytesWith);
    expect(withOwn).toBe(bytesWith);
  });
});

describe('own is set only when a mark is made here', () => {
  it('undo brings back an own comment as own, and loading or pasting never makes a mark own', async () => {
    const { c, hints, answer } = await open('one two {>>earlier<<}\n');
    const t = posOf(c, 'two');
    c.view.dispatch(c.view.state.tr.setSelection(TextSelection.create(c.view.state.doc, t)));
    answer.note = 'mine';
    await c.comment();
    const [mine] = comments(c);
    expect(mine.mine).toBe(true);
    // Deleting an own comment really removes it; undo restores it, still own.
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, mine.pos + 1)));
    // (A later edit, so undo does not group it with adding the comment.)
    v.dispatch(v.state.tr.delete(mine.pos, mine.pos + 1).setTime(Date.now() + 5000));
    expect(comments(c).map((x) => x.note)).toEqual(['earlier']);
    expect(undo(v.state, v.dispatch)).toBe(true);
    expect(comments(c).map((x) => [x.note, x.mine])).toEqual([
      ['mine', true],
      ['earlier', false],
    ]);
    // Pasting review marks as text is refused (they never become marks).
    const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(event, 'clipboardData', {
      value: {
        getData: (k: string) => (k === 'text/plain' ? '{>>pasted<<}' : ''),
        items: [],
        files: [],
        types: ['text/plain'],
      },
    });
    v.dom.dispatchEvent(event);
    expect(hints).toContain('generic');
    expect(comments(c).map((x) => x.note)).toEqual(['mine', 'earlier']);
  });
});

describe('composition and history', () => {
  it('a composition reports no change until it settles', async () => {
    const { c, changes } = await open('a dog\n');
    const p = posOf(c, 'dog');
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, p)));
    c.beginComposition();
    const tr = v.state.tr.insertText('犬', p);
    tr.setMeta('composition', 1);
    v.dispatch(tr);
    expect(changes.n).toBe(0);
    c.endComposition();
    c.settleComposition();
    expect(changes.n).toBe(1);
    expect(c.flush()).toBe('a {++犬++}dog\n');
  });

  it('a composition that changes nothing leaves the editor as it was, and reports it', async () => {
    const { c, changes } = await open('a dog\n');
    expect(c.dirty).toBe(false);
    const p = posOf(c, 'dog');
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, p)));
    c.beginComposition();
    const tr1 = v.state.tr.insertText('犬', p);
    tr1.setMeta('composition', 2);
    v.dispatch(tr1);
    const tr2 = v.state.tr.delete(p, p + 1);
    tr2.setMeta('composition', 2);
    v.dispatch(tr2);
    expect(changes.n).toBe(0);
    c.endComposition();
    c.settleComposition();
    expect(c.dirty).toBe(false);
    expect(changes.n).toBe(1);
    expect(c.flush()).toBe('a dog\n');
  });

  it('opening a text drops a composition still pending from the previous one', async () => {
    const { c, hints } = await open('a dog\n');
    const p = posOf(c, 'dog');
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, p)));
    c.beginComposition();
    const tr = v.state.tr.insertText('犬', p);
    tr.setMeta('composition', 3);
    v.dispatch(tr);
    c.endComposition();
    // The text is opened again before the deferred settle step runs.
    expect(c.load('other text\n')).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    c.settleComposition();
    expect(hints).toEqual([]);
    expect(c.dirty).toBe(false);
    expect(c.flush()).toBe('other text\n');
  });

  it('opening another text drops the undo history', async () => {
    const { c } = await open('one two\n');
    type(c, 'X', posOf(c, 'two'));
    expect(c.load('other text\n')).toBe(true);
    const before = c.view.state.doc;
    expect(undo(c.view.state, c.view.dispatch)).toBe(false);
    expect(c.view.state.doc.eq(before)).toBe(true);
  });
});
