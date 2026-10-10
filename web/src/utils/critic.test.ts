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

import { readFileSync } from 'fs';
import { join } from 'path';
import { describe, expect, it } from 'vitest';

import {
  countCritic,
  criticCodeRanges,
  criticToolBlocked,
  criticToolEdit,
  criticToSentinels,
  normalizeCritic,
  onlyMarksChanged,
  parseCritic,
  projectCritic,
  renderCriticSentinels,
} from './critic.js';
import type { CriticTool } from './critic.js';

interface Case {
  name: string;
  in: string;
  clean: string;
  accept: string;
  /** When set, the text of each code range criticCodeRanges finds. */
  code?: string[];
}

const corpus = (
  JSON.parse(
    readFileSync(join(__dirname, '../../../pkg/artifacts/critic/testdata/corpus.json'), 'utf-8')
  ) as { cases: Case[] }
).cases;

describe('critic corpus shared with the hub', () => {
  it('has the shared cases', () => {
    expect(corpus.length).toBeGreaterThanOrEqual(40);
  });
  for (const c of corpus) {
    it(c.name, () => {
      expect(projectCritic(c.in, 'clean')).toBe(c.clean);
      expect(projectCritic(c.in, 'accept')).toBe(c.accept);
      expect(projectCritic(c.in, 'raw')).toBe(c.in);
      // Segments tile the input.
      let pos = 0;
      for (const s of parseCritic(c.in)) {
        expect(s.start).toBe(pos);
        expect(s.end).toBeGreaterThan(s.start);
        pos = s.end;
      }
      expect(pos).toBe(c.in.length);
      const code = criticCodeRanges(c.in);
      if (c.code) expect(code.map((r) => c.in.slice(r.start, r.end))).toEqual(c.code);
      // No mark token lies in code.
      const inCode = (p: number): boolean => code.some((r) => r.start <= p && p < r.end);
      for (const s of parseCritic(c.in)) {
        if (s.kind === 'text') continue;
        expect(inCode(s.start) || inCode(s.end - 3)).toBe(false);
        if (s.kind === 'substitution') expect(inCode(s.start + 3 + s.text.length)).toBe(false);
      }
    });
  }
  it('pins code for enough cases, some with none', () => {
    const pinned = corpus.filter((c) => c.code);
    expect(pinned.length).toBeGreaterThanOrEqual(40);
    expect(pinned.filter((c) => c.code!.length === 0).length).toBeGreaterThanOrEqual(10);
  });
});

function risingRuns(n: number): string {
  let out = '';
  for (let k = 2; out.length < n / 3; k++) out += '`'.repeat(k) + 'x';
  return out + '`x'.repeat(n / 3);
}

describe('parseCritic cost', () => {
  // A per-opener rescan of the tail examines O(n^2) characters and fails.
  const n = 1 << 14;
  const inputs: Record<string, string> = {
    'unterminated insertions': '{++'.repeat(n),
    'unterminated each kind': '{++{--{~~{>>{=='.repeat(n / 5),
    'substitutions without separator': '{~~a'.repeat(n) + '~~}',
    'separator far away': '{~~a~~}'.repeat(n) + '~>',
    'openers then one closer': '{++a'.repeat(n) + '++}',
    'malformed then valid': '{~~x~~}{++'.repeat(n / 2),
    // Unmatched runs of lengths 2, 3, ... then single-backtick pairs:
    // scanning the runs ahead for each opener's partner is O(n^1.5).
    'rising backtick runs': risingRuns(n),
    'single backtick spans': '`x'.repeat(n / 2),
    'closers hidden in code': '{++' + '`++}`'.repeat(n / 5),
    'separators in code': '{~~' + '`~>`'.repeat(n / 4) + '~~}',
    'openers hidden in code': '`{++`'.repeat(n / 5) + '++}',
    'fence lines': '```\n{++\n'.repeat(n / 8),
    'unterminated fence': '```\n' + '{++a++}\n'.repeat(n / 8),
    'escaped runs': '\\\\\\`'.repeat(n / 7),
  };
  for (const [name, src] of Object.entries(inputs)) {
    it(name, () => {
      const steps = { n: 0 };
      parseCritic(src, steps);
      expect(steps.n).toBeLessThanOrEqual(16 * (src.length + 1));
    });
  }
});

describe('review helpers', () => {
  it('counts marks', () => {
    expect(countCritic('{++a++}{--b--}{~~c~>d~~}{>>e<<}{==f==}{>>g<<}')).toEqual({
      suggestions: 3,
      comments: 2,
      highlights: 1,
    });
  });
  it('normalises line endings and NFC', () => {
    expect(normalizeCritic('café\r\nx\ry')).toBe('café\nx\ny');
  });
  it('accepts marks only', () => {
    const base = 'We ship in Q3.\n';
    expect(onlyMarksChanged('We {~~ship~>launch~~} in Q3.{>>ok?<<}\n', base)).toBe(true);
    expect(onlyMarksChanged('We ship in Q4.\n', base)).toBe(false);
    expect(onlyMarksChanged('We ship in Q3.\r\n', base)).toBe(true);
  });
});

describe('mark rendering sentinels', () => {
  it('renders marks as elements and numbers comments', () => {
    const html = renderCriticSentinels(
      criticToSentinels('a {++b++} {--c--} {~~d~>e~~} {==f==}{>>g<<} {>>h<<}')
    );
    expect(html).toBe(
      'a <ins class=critic-ins>b</ins> <del class=critic-del>c</del> ' +
        '<del class=critic-del>d</del><ins class=critic-ins>e</ins> ' +
        '<mark class=critic-hl>f</mark><sup class=critic-ref>1</sup><span class=critic-note>' +
        '<span class=critic-note-n>1</span> g</span> <sup class=critic-ref>2</sup>' +
        '<span class=critic-note><span class=critic-note-n>2</span> h</span>'
    );
  });
  it('does not let text pose as a mark', () => {
    const out = criticToSentinels('x\uE000y\uE006z');
    expect(out).toBe('x\uFFFDy\uFFFDz');
    expect(renderCriticSentinels(out)).not.toContain('<ins');
  });
});

describe('criticToolEdit', () => {
  const apply = (src: string, e: ReturnType<typeof criticToolEdit>): [string, string] => {
    if (!e) return [src, ''];
    const out = src.slice(0, e.from) + e.insert + src.slice(e.to);
    return [out, out.slice(e.selectFrom, e.selectTo)];
  };
  const src = 'We ship in Q3.';
  const sel = { from: 3, to: 7, text: 'ship' };
  const cursor = { from: 3, to: 3, text: '' };
  it('comments on a selection and leaves the comment selected', () => {
    expect(apply(src, criticToolEdit('comment', sel))).toEqual([
      'We {==ship==}{>>comment<<} in Q3.',
      'comment',
    ]);
    expect(apply(src, criticToolEdit('comment', cursor))).toEqual([
      'We {>>comment<<}ship in Q3.',
      'comment',
    ]);
  });
  it('suggests a replacement with the new side selected', () => {
    expect(apply(src, criticToolEdit('suggest', sel))).toEqual([
      'We {~~ship~>ship~~} in Q3.',
      'ship',
    ]);
    expect(apply(src, criticToolEdit('suggest', cursor))[1]).toBe('replacement');
  });
  it('inserts after the selection', () => {
    expect(apply(src, criticToolEdit('insert', sel))).toEqual(['We ship{++text++} in Q3.', 'text']);
  });
  it('deletes a selection and needs one', () => {
    expect(apply(src, criticToolEdit('delete', sel))[0]).toBe('We {--ship--} in Q3.');
    expect(criticToolEdit('delete', cursor)).toBeNull();
  });
  it('keeps the baseline: every toolbar mark is rejected by clean', () => {
    for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
      const [out] = apply(src, criticToolEdit(tool, sel));
      expect(onlyMarksChanged(out, src)).toBe(true);
    }
  });
});

describe('criticToolBlocked', () => {
  const doc = 'We {~~ship~>launch~~} in Q3. Owners: docs.';
  const at = (from: number, to: number) => ({ from, to, text: doc.slice(from, to) });
  const owners = doc.indexOf('Owners');
  it('allows plain text outside marks', () => {
    for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
      expect(criticToolBlocked(tool, at(owners, owners + 6), doc)).toBeNull();
    }
  });
  it('refuses a selection containing mark tokens', () => {
    for (const text of ['a--}b', 'x~>y', 'x~~}', 'a==}', '{++a', 'b<<}']) {
      expect(criticToolBlocked('comment', { from: 0, to: 0, text }, text)).toMatch(/CriticMarkup/);
    }
  });
  it('refuses a selection inside or across a mark, and an insertion point inside one', () => {
    const inMark = doc.indexOf('ship');
    expect(criticToolBlocked('comment', at(inMark, inMark + 4), doc)).toMatch(/inside or across/);
    // 'We {~' crosses the mark's start without holding a whole token.
    expect(criticToolBlocked('delete', at(0, doc.indexOf('{') + 2), doc)).toMatch(
      /inside or across/
    );
    expect(criticToolBlocked('insert', at(inMark, inMark), doc)).toMatch(/inside or across/);
    expect(criticToolBlocked('comment', at(inMark, inMark), doc)).toMatch(/inside or across/);
    // Right after a mark is outside it.
    const after = doc.indexOf(' in Q3');
    expect(criticToolBlocked('insert', at(after, after), doc)).toBeNull();
  });
  it('asks for a selection to delete', () => {
    expect(criticToolBlocked('delete', at(owners, owners), doc)).toMatch(/Select the text/);
  });
});

describe('renderCriticSentinels in attributes and note headers', () => {
  it('drops sentinels inside tags', () => {
    const html = renderCriticSentinels(
      '<a href="http://a\uE000b\uE001" title="t\uE006c\uE007">x\uE000y\uE001</a><img alt="p\uE002q\uE003">'
    );
    expect(html).toBe(
      '<a href="http://ab" title="tc">x<ins class=critic-ins>y</ins></a><img alt="pq">'
    );
  });
  it('heads notes with the escaped author', () => {
    expect(renderCriticSentinels('\uE006c\uE007', '<Al & "B">')).toContain(
      '<span class=critic-note-n>1 · &lt;Al &amp; &quot;B&quot;&gt;</span> c</span>'
    );
  });
});

describe('criticToolBlocked with an unclosed opener earlier in the text', () => {
  // Each lone opener is closed by the token of exactly one tool.
  const breaks: Record<string, CriticTool> = {
    '{++': 'insert',
    '{--': 'delete',
    '{~~': 'suggest',
    '{>>': 'comment',
    '{==': 'comment',
  };
  for (const [opener, culprit] of Object.entries(breaks)) {
    it(`refuses ${culprit} after a lone ${opener}, allows the other tools`, () => {
      const doc = `Write ${opener} to mark. Here is text.`;
      const from = doc.lastIndexOf('text');
      const sel = { from, to: from + 4, text: 'text' };
      for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
        const edit = criticToolEdit(tool, sel)!;
        const result = doc.slice(0, edit.from) + edit.insert + doc.slice(edit.to);
        const broken = !onlyMarksChanged(result, projectCritic(doc, 'clean'));
        expect(broken, `${opener} ${tool} breaks`).toBe(tool === culprit);
        const hint = criticToolBlocked(tool, sel, doc);
        if (tool === culprit) expect(hint, `${opener} ${tool}`).toMatch(/unclosed/);
        else expect(hint, `${opener} ${tool}`).toBeNull();
      }
    });
  }
  it('allows every tool once the opener is gone', () => {
    const doc = 'Write to mark. Here is text.';
    const from = doc.lastIndexOf('text');
    for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
      expect(criticToolBlocked(tool, { from, to: from + 4, text: 'text' }, doc)).toBeNull();
    }
  });
});

describe('criticToolBlocked with an unclosed opener directly before the selection', () => {
  // The opener becomes the new mark's opener and its text moves inside the
  // mark; the clean text is unchanged, so only the structural check sees it.
  const cases: [string, string, CriticTool][] = [
    ['{--abc', 'abc', 'delete'],
    ['{~~abc', 'abc', 'suggest'],
    ['{==abc', 'abc', 'comment'],
    ['x {--{--abc', 'abc', 'delete'],
  ];
  for (const [doc, word, tool] of cases) {
    it(`refuses ${tool} on '${word}' in '${doc}'`, () => {
      const from = doc.lastIndexOf(word);
      const sel = { from, to: from + word.length, text: word };
      const edit = criticToolEdit(tool, sel)!;
      const result = doc.slice(0, edit.from) + edit.insert + doc.slice(edit.to);
      expect(onlyMarksChanged(result, projectCritic(doc, 'clean'))).toBe(true);
      expect(criticToolBlocked(tool, sel, doc)).toMatch(/unclosed/);
    });
  }
  it('still allows marks next to existing complete marks', () => {
    const doc = 'A {++b++} c {>>d<<} e';
    const at = doc.indexOf(' c ') + 1;
    for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
      expect(criticToolBlocked(tool, { from: at, to: at + 1, text: 'c' }, doc), tool).toBeNull();
    }
  });
});

describe('criticToolBlocked and code', () => {
  const tools = ['comment', 'suggest', 'insert', 'delete'] as const;
  const sel = (doc: string, word: string, nth = 0) => {
    let from = -1;
    for (let i = 0; i <= nth; i++) from = doc.indexOf(word, from + 1);
    return { from, to: from + word.length, text: word };
  };
  const apply = (doc: string, tool: CriticTool, s: { from: number; to: number; text: string }) => {
    const e = criticToolEdit(tool, s)!;
    return doc.slice(0, e.from) + e.insert + doc.slice(e.to);
  };

  it('treats mark-like text in code as text: a whole code span can be marked', () => {
    const doc = 'Write `{++x++}` to insert. Done.';
    const s = sel(doc, '`{++x++}`');
    for (const tool of tools) {
      expect(criticToolBlocked(tool, s, doc), tool).toBeNull();
      expect(onlyMarksChanged(apply(doc, tool, s), doc), tool).toBe(true);
    }
    // Next to code that looks like a mark is not inside a mark.
    const done = sel(doc, 'Done');
    for (const tool of tools) expect(criticToolBlocked(tool, done, doc), tool).toBeNull();
  });

  it('still refuses mark tokens outside code in a selection that also holds code', () => {
    const doc = 'a `{++x++}` b++} c';
    expect(criticToolBlocked('comment', sel(doc, '`{++x++}` b++}'), doc)).toMatch(/CriticMarkup/);
  });

  it('refuses a selection or insertion point inside or across code', () => {
    const doc = 'Run `make test` now.\n\n```sh\ngo test ./...\n```\nEnd.';
    for (const tool of tools) {
      expect(criticToolBlocked(tool, sel(doc, 'make'), doc), tool).toMatch(/inside or across code/);
      // Insert keeps the selection and adds after it, outside code.
      if (tool !== 'insert') {
        expect(criticToolBlocked(tool, sel(doc, 'test` now'), doc), tool).toMatch(
          /inside or across code/
        );
      }
      expect(criticToolBlocked(tool, sel(doc, 'go test'), doc), tool).toMatch(
        /inside or across code/
      );
    }
    const p = doc.indexOf('test');
    for (const tool of ['comment', 'suggest', 'insert'] as const) {
      expect(criticToolBlocked(tool, { from: p, to: p, text: '' }, doc), tool).toMatch(
        /inside or across code/
      );
    }
    // Right after a code span, and a whole code span, are fine.
    const after = doc.indexOf(' now');
    expect(criticToolBlocked('insert', { from: after, to: after, text: '' }, doc)).toBeNull();
    expect(criticToolBlocked('delete', sel(doc, '`make test`'), doc)).toBeNull();
  });

  it('refuses a mark at the start of a fence line, with the fence hint', () => {
    const doc = 'Intro\n```\ncode\n```\nEnd';
    const at = doc.indexOf('```');
    const s = { from: at, to: at, text: '' };
    // The clean text is unchanged, so only a code check sees it.
    expect(onlyMarksChanged(apply(doc, 'comment', s), doc)).toBe(true);
    for (const tool of ['comment', 'suggest', 'insert'] as const) {
      expect(criticToolBlocked(tool, s, doc), tool).toMatch(/fenced code block starts or ends/);
    }
    // A selection ending at the fence would put its closing token there.
    expect(criticToolBlocked('delete', sel(doc, 'Intro\n'), doc)).toMatch(
      /fenced code block starts or ends/
    );
    // At the end of the paragraph before the fence it is fine.
    const end = doc.indexOf('\n');
    expect(criticToolBlocked('comment', { from: end, to: end, text: '' }, doc)).toBeNull();
  });

  it('refuses a mark that changes which text is code elsewhere', () => {
    // The highlight's closing token separates the backslash from the
    // backtick it escaped, so `x` becomes a code span.
    const doc = 'a\\`x` b';
    const s = sel(doc, 'a\\');
    const result = apply(doc, 'comment', s);
    expect(onlyMarksChanged(result, doc)).toBe(true);
    expect(criticCodeRanges(doc)).toEqual([]);
    expect(criticCodeRanges(result)).toHaveLength(1);
    expect(criticToolBlocked('comment', s, doc)).toMatch(/which text is code/);
    expect(criticToolBlocked('delete', s, doc)).toMatch(/which text is code/);
  });

  describe('a whole fenced block', () => {
    const doc = 'Intro\n\n```sh\ngo test\n```\n\nEnd';
    const [range] = criticCodeRanges(doc);
    const pick = (from: number, to: number) => ({ from, to, text: doc.slice(from, to) });
    it('is the one code range', () => {
      expect(doc.slice(range.start, range.end)).toBe('```sh\ngo test\n```\n');
    });
    for (const tool of ['comment', 'suggest', 'delete'] as const) {
      it(`${tool}: refused from the fence line start, with or without the final line break`, () => {
        expect(criticToolBlocked(tool, pick(range.start, range.end), doc)).toMatch(
          /fenced code block starts or ends/
        );
        expect(criticToolBlocked(tool, pick(range.start, range.end - 1), doc)).toMatch(
          /fenced code block starts or ends/
        );
      });
      it(`${tool}: refused from the end of the line before without the final line break`, () => {
        expect(criticToolBlocked(tool, pick(range.start - 1, range.end - 1), doc)).toMatch(
          /fenced code block starts or ends/
        );
      });
      it(`${tool}: allowed from the end of the line before through the closing line break`, () => {
        const s = pick(range.start - 1, range.end);
        expect(criticToolBlocked(tool, s, doc)).toBeNull();
        const result = apply(doc, tool, s);
        expect(onlyMarksChanged(result, doc)).toBe(true);
        // The block is still a fenced block in the review.
        expect(criticCodeRanges(result).map((r) => result.slice(r.start, r.end))).toContain(
          '```sh\ngo test\n```\n'
        );
      });
    }
  });
});
