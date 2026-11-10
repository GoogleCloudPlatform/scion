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
 * Opening a document reads each thing once and shares it between the load
 * checks. These tests pin that every shared reading gives each check the
 * same result as reading it on its own.
 */

import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { getCurrentTest } from 'vitest/suite';

import {
  chooseOpeningAlphabet,
  criticToSentinelText,
  probeOpeningAlphabet,
} from '../../../utils/critic-sentinels.js';
import { parseCritic } from '../../../utils/critic.js';
import { loadDocument } from './bridge.js';
import { definitionNodes } from './create.js';
import type { Core, MdRead } from './create.js';
import { TextMarks } from './guard.js';
import type { MarkRun } from './guard.js';
import {
  consumedAsInSource,
  docStrings,
  joinFlat,
  makeBaseline,
  savePreserving,
  sentinelInDefinition,
} from './preserve.js';
import type { Baseline } from './preserve.js';
import { ReviewSession, codeNeighbours } from './session.js';
import { corpus, destroyCores, makeCore, sweepDocs } from './__fixtures__/core.js';
import { largeDoc } from './__fixtures__/large-doc.js';
import { countTopLevelBlocks } from '../../../utils/markdown-blocks.js';

let core: Core;
beforeAll(async () => {
  core = await makeCore();
});
afterAll(destroyCores);

/**
 * The work sessions leave for when the browser is idle. It runs only when a
 * test says the browser is idle (runIdle), so the idle readings of earlier
 * tests' documents, 200 kB ones among them, do not all run at a later
 * test's first wait.
 */
const idleWork: (() => void)[] = [];
beforeEach(() => {
  idleWork.length = 0;
  vi.stubGlobal('requestIdleCallback', (f: () => void): number => idleWork.push(f));
});
afterAll(() => {
  vi.unstubAllGlobals();
});

/** Runs the idle work asked for so far, in order. */
function runIdle(): void {
  for (const f of idleWork.splice(0)) f();
}

const docs = [...corpus.map((c) => c.in), ...sweepDocs.map((d) => d.in)];

/** The alphabet md opens with: chosen as load chooses it, with the real parser. */
function loadAlphabet(md: string): ReturnType<typeof chooseOpeningAlphabet> {
  return chooseOpeningAlphabet(
    md,
    (text) => `${docStrings(core.parse(md))}\n${docStrings(core.parse(text))}`
  );
}

/** The baseline md opens with, or null. */
function baselineOf(md: string): Baseline | null {
  const a = loadAlphabet(md);
  if (!a) return null;
  const loaded = loadDocument(md, core.parse, core.schema, a);
  return loaded ? makeBaseline(md, loaded.doc, core.parseMd, a) : null;
}

describe('marks of a range, from one reading of the whole text', () => {
  /** The marks of [from, to): every segment of a fresh whole-text parse that starts there. */
  const fresh = (md: string, from: number, to: number, skip: Set<number>): MarkRun[] =>
    parseCritic(md)
      .filter((s) => s.kind !== 'text' && s.start >= from && s.start < to && !skip.has(s.start))
      .map(
        (s): MarkRun =>
          s.kind === 'comment'
            ? { kind: 'comment', text: s.text }
            : s.kind === 'substitution'
              ? { kind: 'substitution', text: core.plain(s.text), next: core.plain(s.next ?? '') }
              : { kind: s.kind as MarkRun['kind'], text: core.plain(s.text) }
      );

  it('gives every range the marks a fresh read of the whole text gives it', () => {
    let ranges = 0;
    for (const md of docs) {
      const marks = new TextMarks(md);
      const cuts = [0, ...parseCritic(md).flatMap((s) => [s.start, s.end]), md.length];
      const skip = new Set(
        parseCritic(md)
          .filter((_, i) => i % 3 === 0)
          .map((s) => s.start)
      );
      for (let i = 0; i < cuts.length; i++) {
        for (let j = i; j < Math.min(cuts.length, i + 4); j++) {
          for (const sk of [new Set<number>(), skip]) {
            expect(marks.runs(core.plain, cuts[i], cuts[j], sk), md).toEqual(
              fresh(md, cuts[i], cuts[j], sk)
            );
            ranges++;
          }
        }
      }
      expect(marks.runs(core.plain), md).toEqual(fresh(md, 0, Infinity, new Set()));
    }
    expect(ranges).toBeGreaterThan(1000);
  }, 30_000);
});

describe('code neighbours', () => {
  /** Per block: another block of its line run, or the text between blocks of the run, holds a backtick. */
  const reference = (base: Baseline): boolean[] => {
    const n = base.blocks.length;
    const run: number[] = [];
    let r = 0;
    for (let k = 0; k < n; k++) {
      run.push(r);
      if (/\n[ \t]*\r?\n/.test(base.blocks[k].after.replace(/\r\n/g, '\n'))) r++;
    }
    const gap = new Set<number>();
    if (n > 0 && base.lead.includes('`')) gap.add(run[0]);
    for (let k = 0; k < n; k++) {
      if (!base.blocks[k].after.includes('`')) continue;
      gap.add(run[k]);
      if (k + 1 < n) gap.add(run[k + 1]);
    }
    return base.blocks.map(
      (_, k) =>
        gap.has(run[k]) ||
        base.blocks.some((b, m) => m !== k && run[m] === run[k] && b.src.includes('`'))
    );
  };

  it('agrees with a block-by-block comparison', () => {
    const extra = [
      'a `x`\nb\n\nc `y`\n',
      '`a`\nb `c`\nd\n',
      '- a `x`\n- b\n\n`c`\n',
      'a\n[r]: /u "`"\nb\n\nc\n',
    ];
    let n = 0;
    for (const md of [...docs, ...extra]) {
      const base = baselineOf(md);
      if (!base) continue;
      expect(codeNeighbours(base), md).toEqual(reference(base));
      n++;
    }
    expect(n).toBeGreaterThan(50);
  });
});

describe('the text of marks', () => {
  it('reads each distinct mark text once per load, and again at the next load', () => {
    const md = 'a {++x++} b {++x++} c {--y--} d {++x++}\n\ne {~~x~>y~~}\n';
    const seen: string[] = [];
    const counting: Core = {
      ...core,
      plain: (t: string): string => {
        seen.push(t);
        return core.plain(t);
      },
    };
    const s = new ReviewSession(counting);
    expect(s.load(md)).toBe(true);
    expect([...seen].sort()).toEqual(['x', 'y']);
    seen.length = 0;
    expect(s.load(md)).toBe(true);
    expect([...seen].sort()).toEqual(['x', 'y']);
  });
});

describe('opening alphabet without the probe', () => {
  const shown =
    (md: string) =>
    (text: string): string =>
      `${docStrings(core.parse(md))}\n${docStrings(core.parse(text))}`;

  it('is the alphabet the probe chooses with the real parser', () => {
    const named = [
      'R&amp;D {++and++} &copy; &nbsp; &lt;b&gt; {>>&hellip;<<}\n',
      '[a](u "&quot;{++x++}&quot;") &emsp;&ensp; `&amp;`\n',
      '<span title="&mdash;">{==x==}</span>{>>&rarr;<<}\n',
    ];
    let n = 0;
    for (const md of [...docs, ...named]) {
      if (md.includes('&#')) continue;
      expect(
        chooseOpeningAlphabet(md, () => ''),
        md
      ).toEqual(probeOpeningAlphabet(md, shown(md)));
      n++;
    }
    expect(n).toBeGreaterThan(100);
  });
});

/** The tagged sentinel form md opens with (what the load checks read), or null. */
function taggedOf(md: string): string | null {
  const a = loadAlphabet(md);
  return a ? criticToSentinelText(md, a, true).text : null;
}

/** A deep copy, to compare a tree with what it was. */
const snapshot = <T>(x: T): T => structuredClone(x);

describe('one reading of Markdown for every load check', () => {
  const large = [largeDoc(false), largeDoc(true)];

  // Every document is read four ways: about 30 s alone, and up to 70 s on a
  // busy machine, hence the longer timeout.
  it('gives the document, the tree and the definitions the separate parses give', () => {
    let n = 0;
    for (const md of [...docs, ...large]) {
      for (const text of [md, taggedOf(md)]) {
        if (text === null) continue;
        const r = core.read(text);
        expect(r.doc.eq(core.parse(text)), text.slice(0, 80)).toBe(true);
        expect(r.tree).toEqual(core.parseMd(text));
        expect(r.definitions).toEqual(definitionNodes(core.parseMdRaw(text)));
        n++;
      }
    }
    expect(n).toBeGreaterThan(200);
  }, 240_000);

  it('building the document does not change the tree, and no check changes it', () => {
    const withDefinition = '[t][r] {++x++}\n\n> [s]: /q\n\n[r]: /p "title"\n';
    let n = 0;
    for (const md of [...docs, withDefinition, ...large]) {
      const a = loadAlphabet(md);
      if (!a) continue;
      const text = criticToSentinelText(md, a, true).text;
      const r = core.read(text);
      // A fresh parse is what the tree was before the document was built.
      const fresh = core.parseMd(text);
      expect(r.tree).toEqual(fresh);
      const tree = snapshot(r.tree);
      const defs = snapshot(r.definitions);
      const loaded = loadDocument(md, () => r.doc, core.schema, a);
      if (!loaded) continue;
      sentinelInDefinition(md, a, r.definitions);
      expect(r.definitions).toEqual(defs);
      consumedAsInSource(md, a, () => r.tree, loaded.doc.childCount, loaded.consumed);
      expect(r.tree).toEqual(tree);
      makeBaseline(md, loaded.doc, () => r.tree, a);
      expect(r.tree).toEqual(tree);
      expect(r.definitions).toEqual(defs);
      n++;
    }
    expect(n).toBeGreaterThan(100);
  }, 120_000);

  it('a whole load leaves every reading as it was read', () => {
    const reads: { r: MdRead; tree: unknown; defs: unknown }[] = [];
    const watching: Core = {
      ...core,
      read: (t: string): MdRead => {
        const r = core.read(t);
        reads.push({ r, tree: snapshot(r.tree), defs: snapshot(r.definitions) });
        return r;
      },
    };
    for (const md of [...docs, ...large]) {
      reads.length = 0;
      new ReviewSession(watching).load(md);
      expect(reads.length).toBeGreaterThan(0);
      for (const { r, tree, defs } of reads) {
        expect(r.tree).toEqual(tree);
        expect(r.definitions).toEqual(defs);
      }
    }
  }, 120_000);

  it('reads each distinct text once per load', () => {
    const texts: string[] = [];
    const counting: Core = {
      ...core,
      read: (t: string): MdRead => {
        texts.push(t);
        return core.read(t);
      },
    };
    // Without a numeric character reference: the sentinel form alone.
    const md = 'one {++two++} [t][r]\n\n[r]: /p\n';
    expect(new ReviewSession(counting).load(md)).toBe(true);
    expect(texts).toEqual([taggedOf(md)]);
    // With one, the probe reads the source and its own sentinel form too.
    texts.length = 0;
    expect(new ReviewSession(counting).load(`&#65; ${md}`)).toBe(true);
    expect(texts.length).toBe(3);
    expect(new Set(texts).size).toBe(3);
  });
});

describe('the baseline of a load', () => {
  it('is the one a fresh build from the opened document gives', () => {
    let n = 0;
    for (const md of [...docs, largeDoc(true)]) {
      const s = new ReviewSession(core);
      if (!s.load(md)) continue;
      const a = loadAlphabet(md)!;
      const fresh = makeBaseline(md, core.view.state.doc, core.parseMd, a);
      const base = (s as unknown as { baseline: Baseline | null }).baseline;
      expect(base).toEqual(fresh);
      if (base) {
        // Blocks are matched by identity with the opened document's.
        base.nodes.forEach((node, k) => expect(node).toBe(core.view.state.doc.child(k)));
        expect(base.written.size).toBe(0);
      }
      n++;
    }
    expect(n).toBeGreaterThan(100);
  }, 120_000);
});

describe('link reference definitions of the opening text', () => {
  // A reference link whose definition is in another block, and no numeric
  // character reference: the load does not read the source itself.
  const md = 'See [the plan][p] and {++more++} text.\n\nOther words here.\n\n[p]: /plan "Plan"\n';
  const defsOf = (s: ReviewSession): string | null =>
    (s as unknown as { definitions: string | null }).definitions;

  /** Types X after "See" as an insertion and saves. */
  const editAndSave = (s: ReviewSession): string | null => {
    const v = core.view;
    const ins = core.schema.marks.critic_ins.create({ mine: true });
    const at = 4;
    v.dispatch(v.state.tr.insertText('X', at).addMark(at, at + 1, ins));
    s.markDirty();
    return s.save();
  };

  it('are read when the browser is idle after the load', () => {
    const s = new ReviewSession(core);
    expect(s.load(md)).toBe(true);
    expect(defsOf(s)).toBeNull();
    expect(idleWork).toHaveLength(1);
    runIdle();
    expect(defsOf(s)).toBe('[p]: /plan "Plan"');
  });

  it('an edit before then saves what it saves after', () => {
    const early = new ReviewSession(core);
    expect(early.load(md)).toBe(true);
    expect(defsOf(early)).toBeNull();
    const before = editAndSave(early);
    expect(defsOf(early)).toBe('[p]: /plan "Plan"');

    const late = new ReviewSession(core);
    expect(late.load(md)).toBe(true);
    runIdle();
    expect(defsOf(late)).toBe('[p]: /plan "Plan"');
    const after = editAndSave(late);

    expect(before).not.toBeNull();
    expect(before).toBe(after);
    expect(before).toContain('{++X++}');
  });

  it("after a later load, they are the later text's", () => {
    const s = new ReviewSession(core);
    expect(s.load(md)).toBe(true);
    expect(s.load('No references here {++x++}.\n')).toBe(true);
    runIdle();
    expect(defsOf(s)).toBe('');
  });

  it('are read soon after the load where the browser cannot say when it is idle', async () => {
    vi.stubGlobal('requestIdleCallback', undefined);
    const timers = vi.spyOn(globalThis, 'setTimeout');
    try {
      const s = new ReviewSession(core);
      expect(s.load(md)).toBe(true);
      expect(defsOf(s)).toBeNull();
      // The reading waits for a timer instead, not for the idle queue, and
      // not for the next microtask either.
      expect(idleWork).toHaveLength(0);
      expect(timers).toHaveBeenCalledWith(expect.any(Function), 0);
      await Promise.resolve();
      expect(defsOf(s)).toBeNull();
      await new Promise((r) => setTimeout(r, 0));
      expect(defsOf(s)).toBe('[p]: /plan "Plan"');
    } finally {
      timers.mockRestore();
    }
  });

  it('come from the source reading when the load has one', () => {
    const s = new ReviewSession(core);
    expect(s.load(`&#65; ${md}`)).toBe(true);
    expect(defsOf(s)).toBe('[p]: /plan "Plan"');
  });
});

/** process.threadCpuUsage, where Node has it (23.9 and later). */
type ThreadCpu = { threadCpuUsage?: () => NodeJS.CpuUsage };
const threadCpuUsage = (process as NodeJS.Process & ThreadCpu).threadCpuUsage;

/**
 * The CPU time this thread has used, in ms. Load budgets are CPU time, not
 * wall time: time spent waiting for a CPU on a busy machine does not count
 * against them, while more work in the load still does. The thread's own
 * time where Node can read it (23.9 and later); otherwise the process's,
 * which is this file's own, since vitest runs each file in its own process
 * (the forks pool, its default).
 */
function cpuMs(): number {
  const u = threadCpuUsage ? threadCpuUsage.call(process) : process.cpuUsage();
  return (u.user + u.system) / 1000;
}

/**
 * The median CPU time of three loads of md with c; each load must open, and
 * passes check. The times are kept in the test's report (its meta), so a
 * run's report shows how close a load came to its budget.
 */
function medianLoadMs(c: Core, md: string, check: (s: ReviewSession) => void = () => {}): number {
  const times: number[] = [];
  for (let k = 0; k < 3; k++) {
    const s = new ReviewSession(c);
    const t0 = cpuMs();
    expect(s.load(md)).toBe(true);
    times.push(cpuMs() - t0);
    check(s);
  }
  times.sort((a, b) => a - b);
  const test = getCurrentTest();
  if (test) {
    const earlier = (test.meta as { loadCpuMs?: number[][] }).loadCpuMs ?? [];
    Object.assign(test.meta, {
      loadCpuMs: [...earlier, times.map(Math.round)],
      cpuClock: threadCpuUsage ? 'thread' : 'process',
    });
  }
  return times[1];
}

describe('opening a 200 kB document', () => {
  /**
   * The load budget, in CPU time in happy-dom: about 1.7 times its usual
   * time (about 3.5 s), so a slower machine does not fail it, and well
   * below what reading the whole text again for every block, or parsing
   * it several times, would take.
   */
  const BUDGET_MS = 6000;
  /**
   * With a numeric character reference the load also parses the source
   * and the alphabet's probe form (design E9): about 7.5 s here, so this
   * budget too is about 1.6 times its usual time.
   */
  const BUDGET_WITH_PROBE_MS = 12000;

  it.each([
    ['plain', false, false],
    ['with a literal opener', true, false],
    ['with a numeric reference and a definition', false, true],
  ])(
    'opens within the load budget (%s)',
    (_, hazard, refs) => {
      const md = largeDoc(hazard, refs);
      expect(md.length).toBeGreaterThan(200_000);
      expect(md.includes('&#')).toBe(refs);
      const ms = medianLoadMs(core, md, (s) => {
        expect(s.isHazardous).toBe(hazard);
        // With a numeric reference the load reads the source itself (the
        // alphabet's probe), and takes the definitions from that reading.
        const defs = (s as unknown as { definitions: string | null }).definitions;
        expect(defs).toBe(refs ? '[guide]: https://example.com/guide "Guide"' : null);
      });
      expect(core.view.state.doc.childCount).toBeGreaterThan(3500);
      expect(ms).toBeLessThan(refs ? BUDGET_WITH_PROBE_MS : BUDGET_MS);
    },
    180_000
  );

  describe('the budget check itself', () => {
    // A short document and a budget scaled to it, so the check is shown to
    // fail a slow load without spending a 200 kB budget to do so.
    const md = 'See {++this++} and [the plan][p].\n\n[p]: /plan\n';
    const SHORT_BUDGET_MS = 200;

    it('fails a load that parses its text again and again', () => {
      expect(medianLoadMs(core, md)).toBeLessThan(SHORT_BUDGET_MS);
      const slow: Core = {
        ...core,
        read: (t: string): MdRead => {
          const r = core.read(t);
          const until = cpuMs() + 2 * SHORT_BUDGET_MS;
          while (cpuMs() < until) core.read(t);
          return r;
        },
      };
      expect(medianLoadMs(slow, md)).toBeGreaterThan(SHORT_BUDGET_MS);
    }, 30_000);

    it('does not count time spent waiting', async () => {
      const t0 = cpuMs();
      await new Promise((r) => setTimeout(r, 300));
      expect(cpuMs() - t0).toBeLessThan(150);
    });
  });
});

describe('the text a save writes', () => {
  it('is its units and separators in order, each where the save says it is', () => {
    let n = 0;
    for (const md of [...docs, largeDoc(true)]) {
      const s = new ReviewSession(core);
      if (!s.load(md)) continue;
      const v = core.view;
      // An edit in the first text block, so one unit is written.
      let at = -1;
      v.state.doc.descendants((node, pos) => {
        if (at < 0 && node.isTextblock && node.type.allowsMarkType(core.schema.marks.critic_ins)) {
          at = pos + 1;
        }
        return at < 0;
      });
      if (at < 0) continue;
      const ins = core.schema.marks.critic_ins.create({ mine: true });
      v.dispatch(v.state.tr.insertText('X', at).addMark(at, at + 1, ins));
      const base = (s as unknown as { baseline: Baseline | null }).baseline;
      const r = savePreserving(v.state.doc, base, core.serialize, core.schema, core.parse);
      if (!r || !base) continue;
      let text = base.lead;
      for (const u of r.units) {
        expect(u.at).toBe(text.length);
        expect(r.md.slice(u.at, u.at + u.md.length)).toBe(u.md);
        text += u.md;
        if (u.i >= 0) text += base.blocks[u.j].after;
      }
      expect(r.md).toBe(text);
      n++;
    }
    expect(n).toBeGreaterThan(50);
  }, 120_000);

  it('joins its parts as concatenation does', () => {
    const parts = ['a', '', 'bc', '\n\n', 'd'.repeat(1000), '{++x++}'];
    expect(joinFlat(parts)).toBe(parts.reduce((x, y) => x + y, ''));
    expect(joinFlat([])).toBe('');
  });
});

describe('the page counts top-level blocks as the editor does', () => {
  /**
   * The page sizes a document with the app's Markdown lexer before the
   * editor loads (approximate by design). The documents where the two
   * counts differ are listed here, so a drift between them shows.
   */
  const KNOWN_DIFFERENCES: Record<string, [lexer: number, editor: number]> = {
    // A tab after a closing fence, and a code span the lexer reads across
    // a line of spaces: the lexer sees one block where the editor sees two.
    'corpus:closing fence may have a trailing tab': [1, 2],
    'corpus:code span stops at a line of spaces': [1, 2],
    // The editor shows an empty document as one empty paragraph.
    'corpus:empty': [0, 1],
    // The lexer counts link reference definitions; the editor has no block for them.
    'sweep:reference links': [5, 3],
  };

  it('agrees on the corpus, the sweep documents and the 200 kB documents', async () => {
    const named = [
      ...corpus.map((c) => [`corpus:${c.name}`, c.in] as const),
      ...sweepDocs.map((d) => [`sweep:${d.name}`, d.in] as const),
      ['large:plain', largeDoc(false)] as const,
      ['large:hazardous', largeDoc(true)] as const,
    ];
    const differences: Record<string, [number, number]> = {};
    let same = 0;
    for (const [name, md] of named) {
      const s = new ReviewSession(core);
      if (!s.load(md)) continue;
      const editor = core.view.state.doc.childCount;
      const lexer = await countTopLevelBlocks(md);
      if (lexer === editor) same++;
      else differences[name] = [lexer, editor];
    }
    expect(differences).toEqual(KNOWN_DIFFERENCES);
    expect(same).toBeGreaterThan(80);
  }, 120_000);
});
