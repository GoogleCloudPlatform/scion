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

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { chooseAlphabet } from '../../../utils/critic-sentinels.js';
import type { Node } from '@milkdown/prose/model';

import { loadDoc, loadDocument, saveDoc, sentinelLeft } from './bridge.js';
import type { Consumed } from './bridge.js';
import { consumedAsInSource, makeBaseline } from './preserve.js';
import { MARKS } from './schema.js';
import type { Core, MdNode } from './create.js';
import { docMarkKinds, earlierKept, keyedMarks, marksReadBack, textMarkKinds } from './guard.js';
import type { MarkRun } from './guard.js';
import { parseCritic, projectCritic } from '../../../utils/critic.js';
import { ReviewSession, backtickRuns, fenceLines } from './session.js';
import { corpus, destroyCores, makeCore } from './__fixtures__/core.js';

let core: Core;
beforeAll(async () => {
  core = await makeCore();
});
afterAll(destroyCores);

describe('corpus round trip', () => {
  it('saves every corpus document unedited as the same bytes', () => {
    const s = new ReviewSession(core);
    for (const c of corpus) {
      expect(s.load(c.in), c.name).toBe(true);
      expect(s.save(), c.name).toBe(c.in);
    }
  });

  it('keeps every block byte for byte when a forced save edits nothing', () => {
    const bad: string[] = [];
    for (const c of corpus) {
      const s = new ReviewSession(core);
      s.load(c.in);
      s.markDirty();
      if (s.save() !== c.in)
        bad.push(`${c.name}: ${JSON.stringify(c.in)} -> ${JSON.stringify(s.save())}`);
    }
    expect(bad).toEqual([]);
  });

  // Marks the editor keeps as literal text: empty marks (no text to carry
  // a mark), marks around a whole fenced block, and the cases where
  // CommonMark sees code that the critic parser does not.
  const LITERAL = [
    'empty insertion',
    'empty deletion',
    'substitution both empty',
    'four plus empty insertion',
    'fence inside a mark',
    'tilde fence in a block quote stays marks (container blocks are not recognised)',
    'tilde fence nested in a list item four spaces deep stays marks',
    'indented code block stays marks',
  ];

  it('reads the same marks as parseCritic, except the pinned literal cases', () => {
    const bad: string[] = [];
    for (const c of corpus) {
      const a = chooseAlphabet(c.in)!;
      const doc = loadDoc(c.in, core.parse, core.schema, a)!;
      const d = docMarkKinds(doc).join(',');
      const t = textMarkKinds(c.in).join(',');
      if (d !== t) bad.push(c.name);
    }
    expect(bad).toEqual(LITERAL);
  });

  it('keeps formatting inside a substitution new side (room for formatting suggestions)', () => {
    const md = 'make {~~a~>**a**~~} bold\n';
    const s = new ReviewSession(core);
    expect(s.load(md)).toBe(true);
    expect(s.save()).toBe(md);
    s.markDirty();
    expect(s.save()).toBe(md);
    const dom = core.view.dom;
    expect(dom.querySelector('del.critic-sub-old strong')).toBeNull();
    expect(
      dom.querySelector('ins.critic-sub-new strong, strong ins.critic-sub-new')
    ).not.toBeNull();
  });

  it('reads the same mark texts back after saving, except the pinned literal cases', () => {
    const bad: string[] = [];
    for (const c of corpus) {
      const a = chooseAlphabet(c.in)!;
      const doc = loadDoc(c.in, core.parse, core.schema, a)!;
      if (marksReadBack(doc, c.in, core.plain) !== null) bad.push(c.name);
    }
    expect(bad).toEqual(LITERAL);
  });

  // A forced full re-serialization (no source passthrough) keeps the clean
  // and accept projections of every other corpus document, compared after
  // both sides go through the same serializer normalization. Where it does
  // not: an empty substitution (its tokens stay literal and get
  // backslashes), and the cases where CommonMark sees code that the
  // critic parser does not (the serializer rewrites the code block).
  // Phase 1 refuses edits in such blocks; phase 2 patches the source.
  const RESERIALIZE_DIFFERS = [
    'substitution both empty',
    'tilde fence in a block quote stays marks (container blocks are not recognised)',
    'tilde fence nested in a list item four spaces deep stays marks',
    'indented code block stays marks',
    'a heading does not end a run of lines',
  ];

  it('keeps the projections after a forced full re-serialization, except pinned cases', () => {
    const norm = (t: string): string => core.serialize(core.parse(t)).replace(/\s+$/, '');
    const bad: string[] = [];
    for (const c of corpus) {
      const a = chooseAlphabet(c.in)!;
      const out = saveDoc(
        loadDoc(c.in, core.parse, core.schema, a)!,
        core.serialize,
        core.schema,
        a
      );
      const same = (['clean', 'accept'] as const).every(
        (m) => norm(projectCritic(out, m)) === norm(projectCritic(c.in, m))
      );
      if (!same) bad.push(c.name);
    }
    expect(bad).toEqual(RESERIALIZE_DIFFERS);
  });
});

describe('load-time safety net', () => {
  it('finds an alphabet character left in text, a node attribute or a mark attribute', () => {
    const a = chooseAlphabet('x')!;
    const { schema } = core.view.state;
    const doc = (
      inline: ReturnType<typeof schema.text>[]
    ): ReturnType<typeof schema.topNodeType.create> =>
      schema.topNodeType.create(null, [schema.nodes.paragraph.create(null, inline)]);
    expect(sentinelLeft(doc([schema.text('plain')]), a)).toBe(false);
    expect(sentinelLeft(doc([schema.text(`a${a.note}b`)]), a)).toBe(true);
    const link = schema.marks.link.create({ href: `https://x.example/${a.insOpen}` });
    expect(sentinelLeft(doc([schema.text('l', [link])]), a)).toBe(true);
    const img = schema.nodes.image.create({ src: 'a.png', alt: `x${a.hlClose}` });
    expect(
      sentinelLeft(schema.topNodeType.create(null, [schema.nodes.paragraph.create(null, [img])]), a)
    ).toBe(true);
  });
});

// The two invariants are tested against deliberately broken parts (a
// parser that moves text, a serializer that rewrites an earlier mark), not
// only against the constructs known today.
/**
 * core with a broken Markdown parser: every document it reads is built by
 * parse (the trees and definitions are the real parser's).
 */
const brokenParse = (parse: (t: string) => Node): Core => ({
  ...core,
  parse,
  read: (t: string) => ({ ...core.read(t), doc: parse(t) }),
});

/** core whose readings find no link reference definitions (the definition check off). */
const noDefinitions = (): Core => ({
  ...core,
  parseMdRaw: (): MdNode => ({ type: 'root', children: [] }),
  read: (t: string) => ({ ...core.read(t), definitions: [] }),
});

describe('load invariant (I1)', () => {
  const md = 'one {==a==}{>>A<<}\n\ntwo {==b==}{>>B<<}\n';

  it('opens the document with the real parser', () => {
    expect(new ReviewSession(core).load(md)).toBe(true);
  });

  it('refuses when a broken parser moves blocks (marks shown on other text)', () => {
    const swap = (t: string): Node => {
      const parts = t.split('\n\n');
      [parts[0], parts[1]] = [parts[1], parts[0]];
      return core.parse(parts.join('\n\n'));
    };
    expect(new ReviewSession(brokenParse(swap)).load(md)).toBe(false);
  });

  it('refuses when a broken parser moves a mark token into another block', () => {
    // Moves the last character before the first break (a comment sentinel)
    // to the end of the text.
    const move = (t: string): Node => {
      const i = t.indexOf('\n\n');
      if (i < 1) return core.parse(t);
      return core.parse(t.slice(0, i - 1) + t.slice(i) + t[i - 1]);
    };
    expect(new ReviewSession(brokenParse(move)).load(md)).toBe(false);
  });

  it('refuses when what the pass consumed differs from the source in any way', () => {
    const a = chooseAlphabet(md)!;
    const loaded = loadDocument(md, core.parse, core.schema, a)!;
    const ok = (c: Consumed[]): boolean =>
      consumedAsInSource(md, a, core.parseMd, loaded.doc.childCount, c) !== null;
    expect(ok(loaded.consumed)).toBe(true);
    // Each note must be the one at its own source place (its ordinal).
    const swapped = loaded.consumed.map((x) => ({ ...x }));
    const withNotes = swapped.filter((x) => x.ordinal !== undefined);
    const first = withNotes[0].ordinal!;
    withNotes[0].ordinal = withNotes[1].ordinal!;
    withNotes[1].ordinal = first;
    expect(ok(swapped)).toBe(false);
    expect(ok(loaded.consumed.map((x, i) => (i === 0 ? { ...x, block: 1 } : x)))).toBe(false);
    expect(ok(loaded.consumed.slice(1))).toBe(false);
    expect(ok([...loaded.consumed, loaded.consumed[0]])).toBe(false);
  });
});

describe('load invariant (I1), read-only units', () => {
  // A block with a mark token inside a link title is read-only; its other
  // marks must still be the critic parser's.
  const md = '[l](u "{++x++}") and {++real++}\n';

  it('opens the read-only unit with the real parser', () => {
    const s = new ReviewSession(core);
    expect(s.load(md)).toBe(true);
    expect(s.readOnlyBlocks()).toEqual([0]);
  });

  it('refuses when a broken parser changes the text of a mark in an editable unit', () => {
    const src = 'one {++real++} two\n';
    expect(new ReviewSession(core).load(src)).toBe(true);
    const broken = brokenParse((t: string): Node => core.parse(t.replace('real', 'fake')));
    expect(new ReviewSession(broken).load(src)).toBe(false);
  });

  it('refuses when a broken parser changes another mark of that unit', () => {
    const broken = brokenParse((t: string): Node => core.parse(t.replace('real', 'fake')));
    expect(new ReviewSession(broken).load(md)).toBe(false);
  });
});

describe('load invariant (I1), notes by place', () => {
  // A link reference definition inside the same block quote or list item
  // as its reference: the parser copies its title (with a note) onto the
  // link, before later text of the same block.
  const nested = [
    '> [t][r] {>>B<<}\n>\n> [r]: /p "{>>A<<}"\n',
    '- [t][r] {>>B<<}\n\n  [r]: /p "{>>A<<}"\n',
  ];

  it('refuses a definition holding a note at any depth', () => {
    for (const md of nested) expect(new ReviewSession(core).load(md), md).toBe(false);
  });

  it('refuses a mark token between blocks with the definition check off', () => {
    // An unused definition holding an insertion's opening token.
    const md = '[u]: /x "{++"\n\nhello ++} world\n';
    expect(new ReviewSession(core).load(md)).toBe(false);
    const s = new ReviewSession(noDefinitions());
    expect(s.load(md)).toBe(false);
  });

  it('refuses the same documents by the note ordinals alone (definition check off)', () => {
    for (const md of nested) {
      const s = new ReviewSession(noDefinitions());
      expect(s.load(md), md).toBe(false);
    }
  });

  it('takes every note from its own place: a tag that says otherwise refuses', () => {
    const md = 'one {>>A<<} two {>>B<<}\n';
    const a = chooseAlphabet(md)!;
    // A parser that swaps the two ordinal tags (the notes would swap).
    const swapTags = (t: string): Node => {
      const i = t.indexOf(a.note);
      const j = t.indexOf(a.note, i + 1);
      const chars = [...t];
      [chars[i + 1], chars[j + 1]] = [chars[j + 1], chars[i + 1]];
      return core.parse(chars.join(''));
    };
    const loaded = loadDocument(md, swapTags, core.schema, a)!;
    expect(loaded).not.toBeNull();
    const labels: string[] = [];
    loaded.doc.descendants((n) => {
      if (n.type.name === 'critic_comment') labels.push(String(n.attrs.note));
      return true;
    });
    expect(labels).toEqual(['B', 'A']);
    expect(
      consumedAsInSource(md, a, core.parseMd, loaded.doc.childCount, loaded.consumed)
    ).toBeNull();
    // Positive control: the real parser.
    const real = loadDocument(md, core.parse, core.schema, a)!;
    expect(
      consumedAsInSource(md, a, core.parseMd, real.doc.childCount, real.consumed)
    ).not.toBeNull();
  });
});

describe('save invariant (I2)', () => {
  // The document's runs for out, as the read-back aligns them: only the
  // count and which runs are the reviewer's own matter here.
  const runs = (out: string, mine: boolean[]): MarkRun[] =>
    parseCritic(out)
      .filter((x) => x.kind !== 'text')
      .map((x, i) => ({ kind: x.kind as MarkRun['kind'], text: '', mine: mine[i] }));
  const keys = (md: string): string[] => keyedMarks(md).map((m) => m.key);
  const kept = (before: string, out: string, mine: boolean[]): boolean =>
    earlierKept(keys(before), out, runs(out, mine));

  it('earlierKept: the earlier marks exactly, once the own ones are dropped', () => {
    const src = 'a {++x++} b {==h==}{>>n<<}';
    expect(kept(src, src, [false, false, false])).toBe(true);
    expect(
      kept(src, 'a {++x++} {--b--} {==h==}{>>n<<}{>>mine<<}', [false, true, false, false, true])
    ).toBe(true);
    expect(kept(src, 'a {++y++} b {==h==}{>>n<<}', [false, false, false])).toBe(false);
    expect(kept(src, 'a {++x++} b {==h==}{>>m<<}', [false, false, false])).toBe(false);
    expect(kept(src, 'a b {==h==}{>>n<<}{++x++}', [false, false, false])).toBe(false);
    expect(kept(src, 'a {++x++} b {==h==}{>>n<<}{++z++}', [false, false, false, false])).toBe(
      false
    );
    expect(kept(src, 'a {++x++} b', [false])).toBe(false);
    // Not aligned with the document's runs.
    expect(earlierKept(keys(src), src, runs(src, [false, false, false]).slice(1))).toBe(false);
  });

  it('earlierKept: a highlight and its comment are one mark', () => {
    // A note placed between an earlier highlight and its comment.
    expect(kept('{==x==}{>>n<<}', '{==x==}{>>m<<}{>>n<<}', [false, true, false])).toBe(false);
    // Positive control: the own note after the pair.
    expect(kept('{==x==}{>>n<<}', '{==x==}{>>n<<}{>>m<<}', [false, false, true])).toBe(true);
    // An own note after an earlier lone highlight is not folded into it
    // (the halves have different owners), so the earlier mark is kept.
    expect(kept('{==x==} y', '{==x==}{>>m<<} y', [false, true])).toBe(true);
    // Folded, the same pair from an earlier review differs from the lone highlight.
    expect(kept('{==x==} y', '{==x==}{>>m<<} y', [false, false])).toBe(false);
  });

  it("earlierKept: an own mark with an earlier mark's key does not stand in for it", () => {
    expect(kept('a {++x++}', 'a {++y++} {++x++}', [false, true])).toBe(false);
    // Positive control: an own mark beside an unchanged identical earlier one.
    expect(kept('a {++x++}', 'a {++x++} {++x++}', [false, true])).toBe(true);
  });

  // An edit beside an earlier insertion whose text a broken serializer
  // rewrites in a way the plain reading does not see.
  const md = 'a {++*x*++} b\n';
  const edit = (s: ReviewSession, c: Core): void => {
    const v = c.view;
    const end = v.state.doc.child(0).nodeSize - 1;
    const mark = c.schema.marks[MARKS.ins].create({ mine: true });
    v.dispatch(v.state.tr.insert(end, c.schema.text(' Z', [mark])));
    s.markDirty();
  };

  it('refuses the save when the whole text changes outside marks', () => {
    // A serializer that adds plain text to the edited paragraph: its marks
    // still read back, so only the whole-text clean check sees it.
    const src = 'a {++x++} b\n';
    const broken = {
      ...core,
      serialize: (d: Node): string => {
        const t = core.serialize(d);
        return t.includes('Z') ? t.replace(' b', ' b more') : t;
      },
    };
    const s = new ReviewSession(broken);
    expect(s.load(src)).toBe(true);
    edit(s, broken);
    expect(s.save()).toBeNull();
    // Positive control: the real serializer.
    const ok = new ReviewSession(core);
    expect(ok.load(src)).toBe(true);
    edit(ok, core);
    expect(ok.save()).toBe('a {++x++} b{++ Z++}\n');
  });

  it("refuses the save when a block written from its source no longer holds the document's marks", () => {
    // Block 0 holds a mark token inside a link title, so it is read-only
    // and always written from its source. A mark put there anyway (no
    // policy here) would vanish from the saved text: only the whole-text
    // read-back sees it.
    const src = '[l](u "{++x++}") and {++real++}\n\nsecond para\n';
    const s = new ReviewSession(core);
    expect(s.load(src)).toBe(true);
    expect(s.readOnlyBlocks()).toEqual([0]);
    const v = core.view;
    const mark = core.schema.marks[MARKS.ins].create({ mine: true });
    v.dispatch(v.state.tr.insert(v.state.doc.child(0).nodeSize - 1, core.schema.text('Z', [mark])));
    s.markDirty();
    expect(s.save()).toBeNull();
    // Positive control: the same mark in block 1 saves, with block 0 (and
    // its token in the title) written from its source.
    const ok = new ReviewSession(core);
    expect(ok.load(src)).toBe(true);
    const end = v.state.doc.child(0).nodeSize + v.state.doc.child(1).nodeSize - 1;
    v.dispatch(v.state.tr.insert(end, core.schema.text('Z', [mark])));
    ok.markDirty();
    expect(ok.save()).toBe('[l](u "{++x++}") and {++real++}\n\nsecond para{++Z++}\n');
  });

  it('refuses the save when a literal opener in an earlier block would take the new closer', () => {
    const src = 'Note: {++ stays literal.\n\nlater text\n';
    const s = new ReviewSession(core);
    expect(s.load(src)).toBe(true);
    const v = core.view;
    const mark = core.schema.marks[MARKS.ins].create({ mine: true });
    const end = v.state.doc.child(0).nodeSize + v.state.doc.child(1).nodeSize - 1;
    v.dispatch(v.state.tr.insert(end, core.schema.text(' Z', [mark])));
    s.markDirty();
    expect(s.unitCheck(v.state.doc, 1, 1)).toBe('generic');
    expect(s.save()).toBeNull();
    // Positive control: the same insertion in a block before the literal
    // opener.
    const ok = new ReviewSession(core);
    const src2 = 'earlier text\n\nNote: {++ stays literal.\n';
    expect(ok.load(src2)).toBe(true);
    v.dispatch(
      v.state.tr.insert(v.state.doc.child(0).nodeSize - 1, core.schema.text(' Z', [mark]))
    );
    ok.markDirty();
    expect(ok.save()).toBe('earlier text{++ Z++}\n\nNote: {++ stays literal.\n');
  });

  it('refuses when the serializer rewrites the earlier mark and writes the own one like it', () => {
    // The own insertion is an emphasised x, as the earlier one is; the
    // broken serializer rewrites only the first, and only in the edited
    // text (a rewrite in the text before the edit too would cancel out in
    // the three-way patch).
    const src = 'a {++*x*++} b\n';
    const broken = {
      ...core,
      serialize: (d: Node): string => {
        const t = core.serialize(d);
        return t.split('*x*').length > 2 ? t.replace('*x*', '_x_') : t;
      },
    };
    const s = new ReviewSession(broken);
    expect(s.load(src)).toBe(true);
    const v = core.view;
    const end = v.state.doc.child(0).nodeSize - 1;
    const ins = core.schema.marks[MARKS.ins].create({ mine: true });
    const em = core.schema.marks.emphasis.create();
    v.dispatch(v.state.tr.insert(end, core.schema.text('x', [em, ins])));
    s.markDirty();
    expect(s.unitOk(v.state.doc, 0, 0)).toBe(false);
    expect(s.save()).toBeNull();
    // Positive control: the real serializer.
    const ok = new ReviewSession(core);
    expect(ok.load(src)).toBe(true);
    v.dispatch(
      v.state.tr.insert(v.state.doc.child(0).nodeSize - 1, core.schema.text('x', [em, ins]))
    );
    ok.markDirty();
    expect(ok.unitOk(v.state.doc, 0, 0)).toBe(true);
    expect(ok.save()).toBe('a {++*x*++} b{++*x*++}\n');
  });

  it('refuses the save when a written unit does not read back as the document', () => {
    // A serializer that changes the reviewer's own inserted text: the
    // earlier marks are untouched, but the text no longer says what the
    // document shows.
    const src = 'a {++x++} b\n';
    const broken = {
      ...core,
      serialize: (d: Node): string => core.serialize(d).replace('Q', 'R'),
    };
    const s = new ReviewSession(broken);
    expect(s.load(src)).toBe(true);
    const v = core.view;
    const ins = core.schema.marks[MARKS.ins].create({ mine: true });
    v.dispatch(v.state.tr.insert(v.state.doc.child(0).nodeSize - 1, core.schema.text('Q', [ins])));
    s.markDirty();
    expect(s.save()).toBeNull();
  });

  it('saves the edit with the real serializer', () => {
    const s = new ReviewSession(core);
    expect(s.load(md)).toBe(true);
    edit(s, core);
    expect(s.unitOk(core.view.state.doc, 0, 0)).toBe(true);
    expect(s.save()).toBe('a {++*x*++} b{++ Z++}\n');
  });

  it('refuses the edit and the save when the serializer rewrites the earlier mark', () => {
    // Only in the edited text: a rewrite of the text before the edit too
    // would cancel out in the three-way patch.
    const broken = {
      ...core,
      serialize: (d: Node): string => {
        const t = core.serialize(d);
        return t.includes('Z') ? t.replace(/\*/g, '_') : t;
      },
    };
    const s = new ReviewSession(broken);
    expect(s.load(md)).toBe(true);
    edit(s, broken);
    expect(s.unitOk(core.view.state.doc, 0, 0)).toBe(false);
    expect(s.save()).toBeNull();
  });
});

describe('baseline units', () => {
  const baseline = (md: string): ReturnType<typeof makeBaseline> => {
    const a = chooseAlphabet(md)!;
    const loaded = loadDocument(md, core.parse, core.schema, a)!;
    return makeBaseline(md, loaded.doc, core.parseMd, a);
  };

  it('splits a list into items and a table into rows', () => {
    const list = baseline('* one\n* two\n')!;
    expect(list.blocks[0].subs).toEqual([
      { from: 0, to: 5 },
      { from: 6, to: 11 },
    ]);
    const table = baseline('| a | b |\n|---|---|\n| c | d |\n')!;
    expect(table.blocks[0].subs?.length).toBe(2);
  });

  it('does not split a list with a mark across two of its items', () => {
    const b = baseline('- a {++x\n- y++} b\n- c\n')!;
    expect(b.blocks).toHaveLength(1);
    expect(b.blocks[0].subs).toBeUndefined();
  });
});

describe('code signature', () => {
  it('counts the lines that would open or close a fence', () => {
    expect(fenceLines('text\n```js\ncode\n```\nmore')).toBe(2);
    expect(fenceLines('   ~~~\n    ```')).toBe(1); // four spaces is indented code
    expect(fenceLines('a ``` b\n``x')).toBe(0);
    expect(fenceLines('{++```++}')).toBe(0); // a typed run after a token is not at a line start
  });

  it('lists backtick runs with their lengths and backslashes', () => {
    expect(backtickRuns('a `b` ``c``')).toBe('1,1,2,2');
    expect(backtickRuns('a \\` b \\\\` c')).toBe('1e,1');
    expect(backtickRuns('no ticks')).toBe('');
  });
});
