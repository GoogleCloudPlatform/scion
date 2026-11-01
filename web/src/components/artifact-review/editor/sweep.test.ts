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
 * The edit sweep: for every document of the shared CriticMarkup corpus and
 * of the sweep's own fixtures (lists, tables, syntax the serializer writes
 * differently), insertions, deletions and replacements at positions spread
 * over the document, made through the editor with its suggest-mode plugins
 * live. Every edit the editor takes must save as text the hub accepts
 * (onlyMarksChanged against the opening text), that opens again in the
 * editor, and that differs from the opening text only inside the edited
 * block (inside the edited item or row, for a list or a table). Every edit
 * it refuses must leave the document as it was and show a hint; an edit
 * that would change nothing (deleting text already suggested for deletion)
 * needs no hint.
 */

import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { writeFileSync } from 'node:fs';
import type { Node } from '@milkdown/prose/model';
import { TextSelection } from '@milkdown/prose/state';
import type { Transaction } from '@milkdown/prose/state';

import { normalizeCritic, onlyMarksChanged, projectCritic } from '../../../utils/critic.js';
import { ReviewController } from './controller.js';
import type { Core } from './create.js';
import type { HintKind } from './policy.js';
import { markKeyOf } from './schema.js';
import { corpus, destroyCores, makeCore, renderContext, sweepDocs } from './__fixtures__/core.js';

/** Each sweep makes about 2,000 edits; this leaves room for slow machines. */
const SWEEP_TIMEOUT_MS = 120_000;

/** Positions per document, spread over its text. */
const SPOTS = 6;

type EditKind = 'insert' | 'delete' | 'replace';
const EDITS: EditKind[] = ['insert', 'delete', 'replace'];

const docs = [
  ...corpus.map((c) => ({ name: `corpus: ${c.name}`, in: c.in })),
  ...sweepDocs.map((c) => ({ name: `sweep: ${c.name}`, in: c.in })),
];

let core: Core;
const made: ReviewController[] = [];
let hints: HintKind[] = [];

beforeAll(async () => {
  core = await makeCore();
});

afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  await destroyCores();
  core = await makeCore();
});

async function controller(): Promise<ReviewController> {
  const root = document.createElement('div');
  document.body.append(root);
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: () => Promise.resolve(null),
    onHint: (h) => hints.push(h),
    onChange: () => {},
  });
  made.push(c);
  return c;
}

/** Every cursor position inside a textblock, in order. */
function textPositions(doc: Node): number[] {
  const out: number[] = [];
  doc.descendants((n, pos) => {
    if (!n.isTextblock) return true;
    for (let p = pos + 1; p <= pos + 1 + n.content.size; p++) out.push(p);
    return false;
  });
  return out;
}

/** The position at fraction f of the text positions. */
function spot(doc: Node, k: number): number | null {
  const all = textPositions(doc);
  if (all.length === 0) return null;
  return all[Math.min(all.length - 1, Math.floor(((k + 0.5) * all.length) / SPOTS))];
}

/** The edit as the browser's input would make it, or null where it does not apply. */
function edit(c: ReviewController, kind: EditKind, p: number): Transaction | null {
  const v = c.view;
  const $p = v.state.doc.resolve(p);
  const end = $p.end();
  let from = p;
  let to = p;
  if (kind === 'delete') to = p + 1;
  if (kind === 'replace') to = Math.min(p + 2, end);
  if (to > end || (kind !== 'insert' && to === from)) return null;
  v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, from, to)));
  if (kind === 'delete') return v.state.tr.delete(from, to);
  from = v.state.selection.from;
  return v.state.tr.insertText(kind === 'insert' ? 'X' : 'Y', from, to);
}

interface Span {
  from: number;
  to: number;
}

/**
 * The source range of the edited unit at document position p: its
 * top-level block, or the item or row of a top-level list or table, read
 * from the Markdown parser's tree of md (mark tokens as text). Null when
 * the tree's blocks do not line up with the document's.
 */
function editedSpan(md: string, doc: Node, p: number): Span | null {
  const kids = (
    core.parseMd(md) as unknown as {
      children: {
        type: string;
        position: { start: { offset: number }; end: { offset: number } };
        children?: { position: { start: { offset: number }; end: { offset: number } } }[];
      }[];
    }
  ).children;
  if (kids.length !== doc.childCount) return null;
  const $p = doc.resolve(p);
  const i = $p.index(0);
  const block = kids[i];
  const span = { from: block.position.start.offset, to: block.position.end.offset };
  if ((block.type === 'list' || block.type === 'table') && $p.depth > 1) {
    const sub = block.children?.[$p.index(1)];
    if (block.children?.length === doc.child(i).childCount && sub) {
      return { from: sub.position.start.offset, to: sub.position.end.offset };
    }
  }
  return span;
}

/** The range of md that out changed (common prefix and suffix taken off). */
function changed(md: string, out: string): Span {
  let pre = 0;
  while (pre < md.length && pre < out.length && md[pre] === out[pre]) pre++;
  let suf = 0;
  while (
    suf < md.length - pre &&
    suf < out.length - pre &&
    md[md.length - 1 - suf] === out[out.length - 1 - suf]
  ) {
    suf++;
  }
  return { from: pre, to: md.length - suf };
}

/** The text of a node with every suggestion accepted. */
function acceptReading(n: Node): string {
  let out = '';
  n.descendants((x) => {
    if (x.isText) {
      const k = markKeyOf(x.marks);
      if (k !== 'del' && k !== 'subOld') out += x.text;
    }
    return true;
  });
  return out;
}

/**
 * Whether the edit, made as plain typing, would leave the accepted text as
 * it is: deleting text already suggested for deletion, or typing over a
 * pending suggestion's new text the same text. The editor records nothing,
 * and needs no hint.
 */
function noOp(doc: Node, tr: Transaction): boolean {
  return tr.doc.childCount === doc.childCount && acceptReading(tr.doc) === acceptReading(doc);
}

interface Tally {
  opened: number;
  notOpened: string[];
  applied: number;
  refused: Partial<Record<HintKind, number>>;
  noOps: number;
  refusals: string[];
  noLocality: number;
  failures: string[];
  outputs: { name: string; base: string; review: string }[];
}

function newTally(): Tally {
  return {
    opened: 0,
    notOpened: [],
    applied: 0,
    refused: {},
    noOps: 0,
    refusals: [],
    noLocality: 0,
    failures: [],
    outputs: [],
  };
}

/**
 * Checks one edit made in c on the text md: taken edits by their saved
 * text, refused ones by the document and the hint. Returns the saved text
 * of a taken edit.
 */
async function check(
  t: Tally,
  c: ReviewController,
  reopen: ReviewController,
  name: string,
  md: string,
  p: number,
  kind: EditKind,
  tr: Transaction,
  localTo: string | null
): Promise<string | null> {
  const where = `${name} @${p} ${kind}`;
  const before = c.view.state.doc;
  const prior = c.flush();
  const hintCount = hints.length;
  c.view.dispatch(tr);
  if (c.view.state.doc.eq(before)) {
    const hint = hints[hintCount];
    if (hint !== undefined) {
      t.refused[hint] = (t.refused[hint] ?? 0) + 1;
      if (process.env.SWEEP_REPORT) t.refusals.push(`${where}: ${hint}`);
    } else if (noOp(before, tr)) t.noOps++;
    else t.failures.push(`${where}: refused without a hint`);
    if (c.flush() !== prior) t.failures.push(`${where}: refused, but the text changed`);
    return null;
  }
  t.applied++;
  const out = c.flush();
  if (out === null) {
    t.failures.push(`${where}: taken but cannot be saved`);
    return null;
  }
  if (!onlyMarksChanged(out, projectCritic(md, 'clean'))) {
    t.failures.push(`${where}: changes text outside marks: ${JSON.stringify(out)}`);
  }
  t.outputs.push({ name: where, base: md, review: out });
  if (!reopen.load(out)) t.failures.push(`${where}: saved text does not open again`);
  if (localTo !== null) {
    const span = editedSpan(localTo, before, p);
    if (!span) t.noLocality++;
    else {
      const ch = changed(localTo, out);
      if (ch.from < span.from || ch.to > span.to) {
        t.failures.push(
          `${where}: changed ${ch.from}..${ch.to} outside the edited unit ${span.from}..${span.to}`
        );
      }
    }
  }
  await Promise.resolve();
  return out;
}

/**
 * With SWEEP_REPORT set, prints a summary; with SWEEP_OUT=<prefix> too,
 * writes <prefix>.<label>.summary.txt (the summary), .refusals.txt (every
 * refused edit and its hint) and .json (every taken edit's opening and
 * saved text, for a check with the hub's own parser).
 */
function report(label: string, t: Tally): void {
  if (!process.env.SWEEP_REPORT) return;
  const summary = `${label}: ${t.opened} opened, ${t.notOpened.length} not opened, ${t.applied} taken, refused ${JSON.stringify(t.refused)}, ${t.noOps} edits that change nothing, ${t.noLocality} without a locality check`;
  console.info(summary);
  const out = process.env.SWEEP_OUT;
  if (!out) return;
  writeFileSync(`${out}.${label}.summary.txt`, `${summary}\n`);
  writeFileSync(`${out}.${label}.refusals.txt`, t.refusals.join('\n'));
  writeFileSync(`${out}.${label}.json`, JSON.stringify(t.outputs));
}

describe('edit sweep', () => {
  it(
    'every taken edit saves as text the hub accepts, local to its unit; every refused edit changes nothing',
    async () => {
      const t = newTally();
      const c = await controller();
      const reopen = await controller();
      for (const d of docs) {
        hints = [];
        if (!c.load(d.in)) {
          t.notOpened.push(d.name);
          continue;
        }
        t.opened++;
        for (let k = 0; k < SPOTS; k++) {
          for (const kind of EDITS) {
            expect(c.load(d.in)).toBe(true);
            const p = spot(c.view.state.doc, k);
            if (p === null) continue;
            const tr = edit(c, kind, p);
            if (!tr) continue;
            await check(t, c, reopen, d.name, d.in, p, kind, tr, d.in);
          }
        }
      }
      report('independent', t);
      expect(t.failures).toEqual([]);
      // Every taken edit had its locality checked, and no text edit in a
      // list or a table was refused as a change to its structure.
      expect(t.noLocality).toBe(0);
      expect(t.refused['list-table'] ?? 0).toBe(0);
      // The sweep exercised edits of every kind on every fixture.
      expect(t.applied).toBeGreaterThan(docs.length);
      for (const d of sweepDocs) expect(t.notOpened).not.toContain(`sweep: ${d.name}`);
    },
    SWEEP_TIMEOUT_MS
  );

  it(
    'a sequence of edits in one session keeps every save acceptable',
    async () => {
      const t = newTally();
      const c = await controller();
      const reopen = await controller();
      for (const d of docs) {
        hints = [];
        if (!c.load(d.in)) {
          t.notOpened.push(d.name);
          continue;
        }
        t.opened++;
        let last: string | null = null;
        for (let k = 0; k < SPOTS; k++) {
          for (const kind of EDITS) {
            const p = spot(c.view.state.doc, (k + EDITS.indexOf(kind)) % SPOTS);
            if (p === null) continue;
            const tr = edit(c, kind, p);
            if (!tr) continue;
            const out = await check(t, c, reopen, d.name, d.in, p, kind, tr, null);
            last = out ?? last;
          }
        }
        if (last !== null) {
          expect(normalizeCritic(projectCritic(last, 'clean'))).toBe(
            normalizeCritic(projectCritic(d.in, 'clean'))
          );
        }
      }
      report('sequence', t);
      expect(t.failures).toEqual([]);
      expect(t.applied).toBeGreaterThan(docs.length);
    },
    SWEEP_TIMEOUT_MS
  );
});
